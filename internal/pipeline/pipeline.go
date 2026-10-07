// Package pipeline wires the stages together. Each stage reads the previous
// stage's artifacts from the repo cache dir and writes its own, so stages can
// run separately and interrupted runs resume.
//
// Cache layout (<cache>/<owner>/<repo>/):
//
//	fetch_state.json, raw/page-*.jsonl      fetch
//	units.jsonl, chunks.jsonl, digest.json  digest
//	extract/ (llm results, batches.json)    extract
//	candidates.jsonl, extract.json
//	synthesize/ (llm results)               synthesize
//	drafts.json, scored.json, synthesize.json
package pipeline

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/an4eetos/pr_reviews_to_skills/internal/config"
	"github.com/an4eetos/pr_reviews_to_skills/internal/digest"
	"github.com/an4eetos/pr_reviews_to_skills/internal/extract"
	"github.com/an4eetos/pr_reviews_to_skills/internal/github"
	"github.com/an4eetos/pr_reviews_to_skills/internal/llm"
	"github.com/an4eetos/pr_reviews_to_skills/internal/output"
	"github.com/an4eetos/pr_reviews_to_skills/internal/rules"
	"github.com/an4eetos/pr_reviews_to_skills/internal/store"
	"github.com/an4eetos/pr_reviews_to_skills/internal/synthesize"
)

type Pipeline struct {
	Cfg  *config.Config
	Logf func(format string, args ...any)
	// LLM is created lazily so fetch/digest never need Anthropic credentials.
	LLM llm.Client
	// Confirm asks the user to approve spend; nil means always approve.
	Confirm func(prompt string) bool
	Now     func() time.Time
}

func (p *Pipeline) dir(parts ...string) string {
	return filepath.Join(append([]string{p.Cfg.RepoCacheDir()}, parts...)...)
}

func (p *Pipeline) now() time.Time {
	if p.Now != nil {
		return p.Now()
	}
	return time.Now()
}

func (p *Pipeline) llmClient() llm.Client {
	if p.LLM == nil {
		p.LLM = llm.NewAnthropic(p.Cfg.Model, p.Cfg.Fallbacks)
	}
	return p.LLM
}

func (p *Pipeline) runner(sub string) *llm.Runner {
	return &llm.Runner{
		Client:       p.llmClient(),
		Dir:          p.dir(sub),
		Concurrency:  p.Cfg.Concurrency,
		PollInterval: p.Cfg.PollInterval,
		Logf:         p.Logf,
	}
}

func (p *Pipeline) Fetch(ctx context.Context) error {
	if p.Cfg.GitHubToken == "" {
		return fmt.Errorf("GITHUB_TOKEN is not set (a token with read access to %s)", p.Cfg.Repo)
	}
	endpoint := config.GraphQLEndpoint(p.Cfg.Repo, p.Cfg.GitHubAPIURL)
	gh := github.NewClient(endpoint, p.Cfg.GitHubToken, p.Logf)
	st, err := gh.Fetch(ctx, p.dir(), github.FetchOptions{
		Owner:    p.Cfg.Repo.Owner,
		Name:     p.Cfg.Repo.Name,
		States:   p.Cfg.States,
		Since:    p.Cfg.Since,
		MaxPRs:   p.Cfg.MaxPRs,
		PageSize: p.Cfg.PageSize,
	})
	if err != nil {
		return fmt.Errorf("fetch: %w", err)
	}
	p.Logf("fetch: %d PRs stored", st.PRs)
	return nil
}

type DigestSummary struct {
	digest.Stats
	Chunks    int `json:"chunks"`
	EstTokens int `json:"est_tokens"`
}

func (p *Pipeline) Digest() (DigestSummary, error) {
	prs, err := github.LoadAll(p.dir())
	if err != nil {
		return DigestSummary{}, err
	}
	if len(prs) == 0 {
		return DigestSummary{}, fmt.Errorf("digest: no fetched PRs in %s; run fetch first", p.dir("raw"))
	}
	units, st := digest.Build(prs, digest.Options{IncludeBots: p.Cfg.IncludeBots})
	chunks := digest.Pack(units, p.Cfg.ChunkTokens)
	sum := DigestSummary{Stats: st, Chunks: len(chunks)}
	for _, c := range chunks {
		sum.EstTokens += c.EstTokens
	}
	if err := store.WriteJSONL(p.dir("units.jsonl"), units); err != nil {
		return sum, err
	}
	if err := store.WriteJSONL(p.dir("chunks.jsonl"), chunks); err != nil {
		return sum, err
	}
	if err := store.WriteJSON(p.dir("digest.json"), sum); err != nil {
		return sum, err
	}
	p.Logf("digest: %d/%d PRs have reviewable discussion (%d threads, %d comments kept; %d noise, %d bot comments dropped) -> %d chunks, ~%d tokens",
		st.PRsKept, st.PRsIn, st.Threads, st.Comments, st.DroppedNoise, st.DroppedBots, len(chunks), sum.EstTokens)
	return sum, nil
}

func (p *Pipeline) loadChunks() ([]digest.Chunk, error) {
	chunks, err := store.ReadJSONL[digest.Chunk](p.dir("chunks.jsonl"))
	if os.IsNotExist(err) {
		return nil, fmt.Errorf("no chunks.jsonl in %s; run digest first", p.dir())
	}
	return chunks, err
}

// Estimate prints the projected cost of extract + synthesize.
func (p *Pipeline) Estimate(ctx context.Context, calibrate bool) (Estimate, error) {
	chunks, err := p.loadChunks()
	if err != nil {
		return Estimate{}, err
	}
	reqs := extract.BuildRequests(p.Cfg.Repo.String(), chunks, p.Cfg.ExtractEffort)
	ratio := 1.0
	if calibrate && len(reqs) > 0 {
		sample := reqs[0]
		counted, err := p.llmClient().CountTokens(ctx, sample)
		if err != nil {
			p.Logf("estimate: count_tokens failed (%v); using the local estimate", err)
		} else {
			local := digest.EstimateTokens(sample.System + sample.Prompt + string(sample.Schema))
			ratio = float64(counted) / float64(local)
		}
	}
	est := estimate(p.Cfg.Model, reqs, ratio, !p.Cfg.Sync)
	fmt.Fprint(os.Stderr, est.String())
	return est, nil
}

func (p *Pipeline) Extract(ctx context.Context) error {
	chunks, err := p.loadChunks()
	if err != nil {
		return err
	}
	units, err := store.ReadJSONL[digest.Unit](p.dir("units.jsonl"))
	if err != nil {
		return err
	}
	reqs := extract.BuildRequests(p.Cfg.Repo.String(), chunks, p.Cfg.ExtractEffort)
	r := p.runner("extract")

	pending := 0
	for _, q := range reqs {
		if _, ok := r.Stored(q.ID); !ok {
			pending++
		}
	}
	if pending > 0 && p.Confirm != nil {
		est, err := p.Estimate(ctx, false)
		if err != nil {
			return err
		}
		if !p.Confirm(fmt.Sprintf("Run extraction on %d chunk(s) (%d already done), estimated total ~$%.2f?", pending, len(reqs)-pending, est.Total())) {
			return fmt.Errorf("aborted")
		}
	}

	var results map[string]llm.Result
	if p.Cfg.Sync {
		results, err = r.RunSync(ctx, reqs)
	} else {
		results, err = r.RunBatch(ctx, reqs)
	}
	if err != nil {
		return fmt.Errorf("extract: %w", err)
	}
	cands, st := extract.Parse(reqs, results, digest.RefIndex(units))
	if err := store.WriteJSONL(p.dir("candidates.jsonl"), cands); err != nil {
		return err
	}
	if err := store.WriteJSON(p.dir("extract.json"), st); err != nil {
		return err
	}
	p.Logf("extract: %d candidates from %d/%d chunks (%d unknown refs dropped, %d quotes replaced, %d candidates without evidence dropped)",
		st.Candidates, st.Succeeded, st.Requests, st.UnknownRefs, st.QuotesReplaced, st.DroppedNoProof)
	if st.Failed > 0 {
		p.Logf("extract: %d chunk(s) failed; re-run extract to retry them before synthesizing", st.Failed)
	}
	return nil
}

func (p *Pipeline) Synthesize(ctx context.Context) error {
	cands, err := store.ReadJSONL[rules.Candidate](p.dir("candidates.jsonl"))
	if os.IsNotExist(err) {
		return fmt.Errorf("no candidates.jsonl in %s; run extract first", p.dir())
	}
	if err != nil {
		return err
	}
	if len(cands) == 0 {
		return fmt.Errorf("synthesize: no candidate rules were extracted")
	}
	opts := synthesize.Options{
		Runner: p.runner("synthesize"),
		Effort: p.Cfg.SynthEffort,
		Logf:   p.Logf,
	}
	drafts, mst, err := synthesize.Merge(ctx, opts, cands)
	if err != nil {
		return err
	}
	synthesize.AttachEvidence(drafts, cands, p.now())
	if err := store.WriteJSON(p.dir("drafts.json"), drafts); err != nil {
		return err
	}
	scored, err := synthesize.Score(ctx, opts, drafts)
	if err != nil {
		return err
	}
	if err := store.WriteJSON(p.dir("scored.json"), scored); err != nil {
		return err
	}
	if err := store.WriteJSON(p.dir("synthesize.json"), mst); err != nil {
		return err
	}
	return p.writeOutput(cands, scored)
}

func (p *Pipeline) writeOutput(cands []rules.Candidate, scored []rules.Rule) error {
	var ds DigestSummary
	if _, err := store.ReadJSON(p.dir("digest.json"), &ds); err != nil {
		return err
	}
	final := output.Finalize(scored, p.Cfg.KeepRejected)
	rep := output.Report{
		SchemaVersion: output.SchemaVersion,
		Repo:          p.Cfg.Repo.String(),
		GeneratedAt:   p.now().UTC(),
		Model:         p.Cfg.Model,
		Stats: output.Stats{
			PRsScanned:        ds.PRsIn,
			PRsWithDiscussion: ds.PRsKept,
			Threads:           ds.Threads,
			Comments:          ds.Comments,
			Chunks:            ds.Chunks,
			Candidates:        len(cands),
			Rules:             len(final),
			ByTier:            output.Tally(scored),
		},
		Rules: final,
	}
	if err := output.Write(p.Cfg.Out, rep); err != nil {
		return err
	}
	t := rep.Stats.ByTier
	p.Logf("done: %d rules -> %s (golden %d, conditional %d, consider %d; %d rejected%s)",
		len(final), p.Cfg.Out, t["golden"], t["conditional"], t["consider"], t["rejected"],
		map[bool]string{true: " and kept", false: " and omitted"}[p.Cfg.KeepRejected])
	return nil
}

// Run executes every stage; with DryRun it stops after printing the estimate.
func (p *Pipeline) Run(ctx context.Context) error {
	if err := p.Fetch(ctx); err != nil {
		return err
	}
	if _, err := p.Digest(); err != nil {
		return err
	}
	if p.Cfg.DryRun {
		_, err := p.Estimate(ctx, true)
		return err
	}
	if err := p.Extract(ctx); err != nil {
		return err
	}
	return p.Synthesize(ctx)
}
