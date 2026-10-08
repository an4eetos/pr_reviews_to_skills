// Package pipeline wires the stages together. Each stage reads the previous
// stage's artifacts from the repo cache dir and writes its own, so stages can
// run separately and interrupted runs resume. Runs are incremental: fetch
// only asks GitHub for PRs updated since the last fetch, digest packs only
// new or changed discussions into a new generation, and synthesis folds the
// new candidates into the existing rules.
//
// Cache layout (<cache>/<owner>/<repo>/):
//
//	fetch_state.json, raw/page-*.jsonl          fetch
//	ledger.json, digest.json                    digest
//	gens/gNNNN/{units,chunks}.jsonl             digest (one generation)
//	gens/gNNNN/{candidates.jsonl,extract.json}  extract
//	extract/ (llm results, batches.json)        extract
//	synthesize/ (llm results)                   synthesize
//	synth_state.json                            synthesize (rules + what's folded)
package pipeline

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"time"

	"github.com/an4eetos/pr_reviews_to_skills/internal/config"
	"github.com/an4eetos/pr_reviews_to_skills/internal/digest"
	"github.com/an4eetos/pr_reviews_to_skills/internal/export"
	"github.com/an4eetos/pr_reviews_to_skills/internal/extract"
	"github.com/an4eetos/pr_reviews_to_skills/internal/github"
	"github.com/an4eetos/pr_reviews_to_skills/internal/llm"
	"github.com/an4eetos/pr_reviews_to_skills/internal/metrics"
	"github.com/an4eetos/pr_reviews_to_skills/internal/output"
	"github.com/an4eetos/pr_reviews_to_skills/internal/rules"
	"github.com/an4eetos/pr_reviews_to_skills/internal/store"
	"github.com/an4eetos/pr_reviews_to_skills/internal/synthesize"
)

// errWriter receives cost estimates; tests silence it.
var errWriter io.Writer = os.Stderr

type Pipeline struct {
	Cfg  *config.Config
	Logf func(format string, args ...any)
	// LLM is created lazily so fetch/digest never need Anthropic credentials.
	LLM llm.Client
	// Confirm asks the user to approve spend; nil means always approve.
	Confirm func(prompt string) bool
	Now     func() time.Time
	// Dir overrides the cache directory; the combined rule set uses it since
	// it belongs to no single repo.
	Dir string
}

func (p *Pipeline) dir(parts ...string) string {
	base := p.Dir
	if base == "" {
		base = p.Cfg.RepoCacheDir()
	}
	return filepath.Join(append([]string{base}, parts...)...)
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
	_, err := gh.Fetch(ctx, p.dir(), github.FetchOptions{
		Owner:    p.Cfg.Repo.Owner,
		Name:     p.Cfg.Repo.Name,
		States:   p.Cfg.States,
		Since:    p.Cfg.Since,
		MaxPRs:   p.Cfg.MaxPRs,
		PageSize: p.Cfg.PageSize,
		Full:     p.Cfg.Full,
	})
	if err != nil {
		return fmt.Errorf("fetch: %w", err)
	}
	return nil
}

type DigestSummary struct {
	digest.Stats
	// Chunks counts every generation's chunks; NewPRs, Gen and EstTokens
	// describe the generation this digest opened (if any).
	Chunks    int `json:"chunks"`
	NewPRs    int `json:"new_prs"`
	Gen       int `json:"gen,omitempty"`
	EstTokens int `json:"est_tokens"`
}

// Digest builds units for every fetched PR and packs the ones whose
// discussion is new or changed into an open generation.
func (p *Pipeline) Digest() (DigestSummary, error) {
	prs, err := github.LoadAll(p.dir())
	if err != nil {
		return DigestSummary{}, err
	}
	if len(prs) == 0 {
		return DigestSummary{}, fmt.Errorf("digest: no fetched PRs in %s; run fetch first", p.dir("raw"))
	}
	l, err := p.loadLedger()
	if err != nil {
		return DigestSummary{}, err
	}
	if err := p.dropOpen(l); err != nil {
		return DigestSummary{}, err
	}
	units, st := digest.Build(prs, digest.Options{IncludeBots: p.Cfg.IncludeBots})
	var dirty []digest.Unit
	hashes := map[int]string{}
	for _, u := range units {
		h := digest.ContentHash(u)
		if l.PRs[u.PR] != h {
			dirty = append(dirty, u)
			hashes[u.PR] = h
		}
	}
	sum := DigestSummary{Stats: st, NewPRs: len(dirty)}
	if len(dirty) > 0 {
		g := &Gen{ID: l.nextID(), Status: genOpen, PRs: hashes, CreatedAt: p.now().UTC()}
		prefix := g.Key() + "-"
		chunks := digest.Pack(dirty, p.Cfg.ChunkTokens)
		for i := range chunks {
			chunks[i].ID = prefix + chunks[i].ID
			g.EstTokens += chunks[i].EstTokens
		}
		g.Chunks = len(chunks)
		if err := store.WriteJSONL(p.genDir(g.ID, "units.jsonl"), dirty); err != nil {
			return sum, err
		}
		if err := store.WriteJSONL(p.genDir(g.ID, "chunks.jsonl"), chunks); err != nil {
			return sum, err
		}
		l.Gens = append(l.Gens, g)
		sum.Gen, sum.EstTokens = g.ID, g.EstTokens
	}
	sum.Chunks = l.totalChunks()
	if err := p.saveLedger(l); err != nil {
		return sum, err
	}
	if err := store.WriteJSON(p.dir("digest.json"), sum); err != nil {
		return sum, err
	}
	p.Logf("digest: %d/%d PRs have reviewable discussion (%d threads, %d comments kept; %d noise, %d bot comments dropped)",
		st.PRsKept, st.PRsIn, st.Threads, st.Comments, st.DroppedNoise, st.DroppedBots)
	if len(dirty) == 0 {
		p.Logf("digest: no new or changed discussions since the last run")
	} else {
		p.Logf("digest: %d new or changed PR(s) -> generation %d, %d chunk(s), ~%d tokens", len(dirty), sum.Gen, l.Gens[len(l.Gens)-1].Chunks, sum.EstTokens)
	}
	return sum, nil
}

// ExtractPlan is the extraction work left for one repo: every generation
// not yet extracted and its requests.
type ExtractPlan struct {
	ledger *Ledger
	gens   []*Gen
	reqs   map[int][]llm.Request
	all    []llm.Request
	// Unsent are the requests nobody has paid for yet.
	Unsent []llm.Request
}

func (p *Pipeline) PlanExtract() (*ExtractPlan, error) {
	l, err := p.loadLedger()
	if err != nil {
		return nil, err
	}
	plan := &ExtractPlan{ledger: l, gens: l.pending(), reqs: map[int][]llm.Request{}}
	for _, g := range plan.gens {
		chunks, err := store.ReadJSONL[digest.Chunk](p.genDir(g.ID, "chunks.jsonl"))
		if err != nil {
			return nil, err
		}
		reqs := extract.BuildRequests(p.Cfg.Repo.String(), chunks, p.Cfg.ExtractEffort)
		plan.reqs[g.ID] = reqs
		plan.all = append(plan.all, reqs...)
	}
	if len(plan.all) > 0 {
		if plan.Unsent, err = p.runner("extract").Unsent(plan.all); err != nil {
			return nil, err
		}
	}
	return plan, nil
}

// Estimate prints the projected cost of extracting what's new, plus synthesis.
func (p *Pipeline) Estimate(ctx context.Context, calibrate bool) (Estimate, error) {
	plan, err := p.PlanExtract()
	if err != nil {
		return Estimate{}, err
	}
	est := p.estimate(ctx, plan.Unsent, calibrate)
	fmt.Fprint(errWriter, est.String())
	return est, nil
}

func (p *Pipeline) estimate(ctx context.Context, reqs []llm.Request, calibrate bool) Estimate {
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
	return estimate(p.Cfg.Model, reqs, ratio, !p.Cfg.Sync)
}

// approve enforces --max-cost and asks for confirmation before spending.
func approve(cfg *config.Config, confirm func(string) bool, est Estimate, what string) error {
	if est.Requests == 0 {
		return nil
	}
	if cfg.MaxCost > 0 {
		if !est.KnownPrice {
			return fmt.Errorf("no price known for %s, so --max-cost can't be enforced", est.Model)
		}
		if est.Total() > cfg.MaxCost {
			return fmt.Errorf("%s: estimated ~$%.2f exceeds --max-cost $%.2f; raise it, or narrow the run with --since or --max-prs", what, est.Total(), cfg.MaxCost)
		}
	}
	if confirm != nil {
		fmt.Fprint(errWriter, est.String())
		if !confirm(fmt.Sprintf("Run %s on %d chunk(s), estimated total ~$%.2f?", what, est.Requests, est.Total())) {
			return fmt.Errorf("aborted")
		}
	}
	return nil
}

func (p *Pipeline) Extract(ctx context.Context) error {
	plan, err := p.PlanExtract()
	if err != nil {
		return err
	}
	if len(plan.gens) == 0 {
		p.Logf("extract: nothing new to extract")
		return nil
	}
	if err := approve(p.Cfg, p.Confirm, p.estimate(ctx, plan.Unsent, false), "extraction"); err != nil {
		return err
	}
	if err := p.SubmitExtract(ctx, plan); err != nil {
		return err
	}
	return p.CollectExtract(ctx, plan, p.Cfg.MaxWait)
}

// SubmitExtract seals open generations and, in batch mode, submits every
// unsent request. Spend must already be approved.
func (p *Pipeline) SubmitExtract(ctx context.Context, plan *ExtractPlan) error {
	for _, g := range plan.gens {
		plan.ledger.seal(g)
	}
	if err := p.saveLedger(plan.ledger); err != nil {
		return err
	}
	if p.Cfg.Sync || len(plan.all) == 0 {
		return nil
	}
	if _, err := p.runner("extract").Submit(ctx, plan.all); err != nil {
		return fmt.Errorf("extract: %w", err)
	}
	return nil
}

// CollectExtract runs (sync) or collects (batch) the plan's requests and
// turns every generation whose results are all in into candidates. Batches
// still running after maxWait are left for the next run.
func (p *Pipeline) CollectExtract(ctx context.Context, plan *ExtractPlan, maxWait time.Duration) error {
	if len(plan.gens) == 0 {
		return nil
	}
	r := p.runner("extract")
	var results map[string]llm.Result
	var err error
	if p.Cfg.Sync {
		results, err = r.RunSync(ctx, plan.all)
	} else {
		results, err = r.Collect(ctx, plan.all, maxWait)
	}
	pending := errors.Is(err, llm.ErrPending)
	if err != nil && !pending {
		return fmt.Errorf("extract: %w", err)
	}
	waiting := 0
	for _, g := range plan.gens {
		reqs := plan.reqs[g.ID]
		missing, failed := 0, 0
		for _, q := range reqs {
			if res, ok := results[q.ID]; !ok {
				missing++
			} else if !res.OK() {
				failed++
			}
		}
		if missing > 0 {
			waiting++
			continue
		}
		if failed > 0 {
			g.Attempts++
			if g.Attempts < maxExtractAttempts {
				p.Logf("extract: generation %d: %d chunk(s) failed; the next run retries them", g.ID, failed)
				continue
			}
			p.Logf("extract: generation %d: %d chunk(s) failed %d times; continuing without them", g.ID, failed, g.Attempts)
		}
		units, err := store.ReadJSONL[digest.Unit](p.genDir(g.ID, "units.jsonl"))
		if err != nil {
			return err
		}
		cands, st := extract.Parse(reqs, results, digest.RefIndex(units))
		if err := store.WriteJSONL(p.genDir(g.ID, "candidates.jsonl"), cands); err != nil {
			return err
		}
		if err := store.WriteJSON(p.genDir(g.ID, "extract.json"), st); err != nil {
			return err
		}
		g.Status, g.Candidates, g.ExtractedAt = genExtracted, len(cands), p.now().UTC()
		p.Logf("extract: generation %d: %d candidates from %d/%d chunks (%d unknown refs dropped, %d quotes replaced, %d candidates without evidence dropped)",
			g.ID, st.Candidates, st.Succeeded, st.Requests, st.UnknownRefs, st.QuotesReplaced, st.DroppedNoProof)
	}
	if err := p.saveLedger(plan.ledger); err != nil {
		return err
	}
	if waiting > 0 {
		p.Logf("extract: %d generation(s) still waiting on batches; the next run collects them", waiting)
	}
	return nil
}

// SynthState is the rule set as of the last synthesis, with the candidate
// IDs behind every rule, and the generations already folded into it.
type SynthState struct {
	Folded    []string     `json:"folded"`
	UpdatedAt time.Time    `json:"updated_at"`
	Rules     []rules.Rule `json:"rules"`
}

func (p *Pipeline) loadSynthState() (SynthState, bool, error) {
	var st SynthState
	ok, err := store.ReadJSON(p.dir("synth_state.json"), &st)
	if err != nil || ok {
		return st, ok, err
	}
	// A cache from before generations: its scored rules cover generation 0.
	var scored []rules.Rule
	if ok, err := store.ReadJSON(p.dir("scored.json"), &scored); err != nil || !ok {
		return st, false, err
	}
	l, err := p.loadLedger()
	if err != nil || len(l.Gens) == 0 || l.Gens[0].ID != 0 {
		return st, false, err
	}
	st.Rules, st.Folded = scored, []string{l.Gens[0].Key()}
	if info, err := os.Stat(p.dir("scored.json")); err == nil {
		st.UpdatedAt = info.ModTime().UTC()
	}
	return st, true, nil
}

// Synthesize folds newly extracted candidates into the rules (or builds them
// from scratch on the first run) and writes rules.json plus any exports.
func (p *Pipeline) Synthesize(ctx context.Context) error {
	cs, err := p.Candidates()
	if err != nil {
		return err
	}
	if ok, err := p.HasExtracted(); err != nil {
		return err
	} else if !ok {
		return fmt.Errorf("synthesize: nothing extracted yet in %s; run extract first", p.dir())
	}
	if len(cs.All) == 0 {
		return fmt.Errorf("synthesize: no candidate rules were extracted")
	}
	st, changed, err := p.synthesize(ctx, cs, false)
	if err != nil {
		return err
	}
	stats, err := p.stats(len(cs.All))
	if err != nil {
		return err
	}
	return p.writeOutput(st, changed, stats, p.Cfg.Repo.String(), nil)
}

// synthesize updates the stored rule set with cs and reports whether any
// rule changed. combined applies the cross-repo floor.
func (p *Pipeline) synthesize(ctx context.Context, cs CandidateSet, combined bool) (SynthState, bool, error) {
	st, have, err := p.loadSynthState()
	if err != nil {
		return st, false, err
	}
	keys := make([]string, 0, len(cs.ByGen))
	for k := range cs.ByGen {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var newCands []rules.Candidate
	for _, k := range keys {
		if !slices.Contains(st.Folded, k) {
			newCands = append(newCands, cs.ByGen[k]...)
		}
	}

	opts := synthesize.Options{Runner: p.runner("synthesize"), Effort: p.Cfg.SynthEffort, Logf: p.Logf}
	var drafts []rules.Rule
	var touched []bool
	if !have || p.Cfg.FullResynth {
		var mst synthesize.MergeStats
		drafts, mst, err = synthesize.Merge(ctx, opts, cs.All)
		if err != nil {
			return st, false, err
		}
		synthesize.AttachEvidence(drafts, cs.All, p.now())
		touched = make([]bool, len(drafts))
		for i := range touched {
			touched[i] = true
		}
		if err := store.WriteJSON(p.dir("synthesize.json"), mst); err != nil {
			return st, false, err
		}
	} else {
		if len(newCands) == 0 && !p.Cfg.RescoreAll {
			p.Logf("synthesize: no new candidates since the last synthesis")
		}
		var fst synthesize.FoldStats
		drafts, touched, fst, err = synthesize.Fold(ctx, opts, st.Rules, newCands, cs.All, p.now())
		if err != nil {
			return st, false, err
		}
		if fst.NewCandidates > 0 || fst.Dropped > 0 {
			p.Logf("synthesize: folded %d new candidates: %d into existing rules, %d new rules, %d discarded; %d rules dropped, %d to re-score",
				fst.NewCandidates, fst.Assigned, fst.NewRules, fst.Discarded, fst.Dropped, fst.Touched)
		}
		if err := store.WriteJSON(p.dir("fold.json"), fst); err != nil {
			return st, false, err
		}
	}
	if p.Cfg.RescoreAll {
		for i := range touched {
			touched[i] = true
		}
	}
	changed := len(drafts) != len(st.Rules) || slices.Contains(touched, true)
	scored, err := synthesize.Score(ctx, opts, drafts, touched)
	if err != nil {
		return st, false, err
	}
	if combined {
		for i := range scored {
			r := &scored[i]
			tier, when, note := metrics.RepoFloor(r.Tier, r.AppliesWhen, r.Repos)
			if note != "" {
				r.Tier, r.AppliesWhen = tier, when
				r.TierReason = trimJoin(r.TierReason, note)
			}
		}
	}
	st.Folded, st.Rules = keys, scored
	if changed || st.UpdatedAt.IsZero() {
		st.UpdatedAt = p.now().UTC()
	}
	if err := store.WriteJSON(p.dir("synth_state.json"), st); err != nil {
		return st, false, err
	}
	return st, changed, nil
}

func trimJoin(a, b string) string {
	if a == "" {
		return b
	}
	return a + " " + b
}

// writeOutput writes rules.json and the configured exports. When no rule
// changed it leaves existing files alone, so scheduled runs with nothing new
// don't produce diffs; GeneratedAt is the time the rules last changed.
func (p *Pipeline) writeOutput(st SynthState, changed bool, stats output.Stats, repo string, repos []string) error {
	final := output.Finalize(st.Rules, p.Cfg.KeepRejected)
	stats.Rules = len(final)
	stats.ByTier = output.Tally(st.Rules)
	rep := output.Report{
		SchemaVersion: output.SchemaVersion,
		Repo:          repo,
		Repos:         repos,
		GeneratedAt:   st.UpdatedAt,
		Model:         p.Cfg.Model,
		Stats:         stats,
		Rules:         final,
	}
	if !changed && store.Exists(p.Cfg.Out) {
		p.Logf("synthesize: rules unchanged; %s left as is", p.Cfg.Out)
		if p.exportOpts() != nil && export.Missing(rep, *p.exportOpts()) {
			return p.export(rep)
		}
		return nil
	}
	if err := output.Write(p.Cfg.Out, rep); err != nil {
		return err
	}
	t := rep.Stats.ByTier
	p.Logf("done: %d rules -> %s (golden %d, conditional %d, consider %d; %d rejected%s)",
		len(final), p.Cfg.Out, t["golden"], t["conditional"], t["consider"], t["rejected"],
		map[bool]string{true: " and kept", false: " and omitted"}[p.Cfg.KeepRejected])
	return p.export(rep)
}

func (p *Pipeline) exportOpts() *export.Options {
	if len(p.Cfg.Export) == 0 {
		return nil
	}
	// The tier was validated when the flags were parsed.
	minTier, _ := export.ParseTier(p.Cfg.MinTier)
	return &export.Options{Formats: p.Cfg.Export, Dest: p.Cfg.ExportDir, MinTier: minTier}
}

func (p *Pipeline) export(rep output.Report) error {
	opts := p.exportOpts()
	if opts == nil {
		return nil
	}
	files, err := export.Write(rep, *opts)
	if err != nil {
		return err
	}
	p.Logf("export: wrote %d file(s) under %s", len(files), p.Cfg.ExportDir)
	return nil
}

// HasExtracted reports whether any generation has candidates to synthesize.
func (p *Pipeline) HasExtracted() (bool, error) {
	l, err := p.loadLedger()
	if err != nil {
		return false, err
	}
	return len(l.extracted()) > 0, nil
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
	return p.synthesizeIfReady(ctx)
}

func (p *Pipeline) synthesizeIfReady(ctx context.Context) error {
	ok, err := p.HasExtracted()
	if err != nil {
		return err
	}
	if !ok {
		p.Logf("synthesize: waiting for the first extraction to finish; the next run continues")
		return nil
	}
	return p.Synthesize(ctx)
}
