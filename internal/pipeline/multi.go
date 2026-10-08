package pipeline

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/an4eetos/pr_reviews_to_skills/internal/config"
	"github.com/an4eetos/pr_reviews_to_skills/internal/github"
	"github.com/an4eetos/pr_reviews_to_skills/internal/llm"
	"github.com/an4eetos/pr_reviews_to_skills/internal/output"
	"github.com/an4eetos/pr_reviews_to_skills/internal/rules"
	"github.com/an4eetos/pr_reviews_to_skills/internal/store"
)

// Multi runs the pipeline over every repo of a command. Stages run across
// all repos before the next stage starts, so extraction batches for every
// repo are in flight at the same time, and a repo that fails (no access, say)
// is reported at the end without stopping the others.
type Multi struct {
	Cfg     *config.Config
	Logf    func(format string, args ...any)
	LLM     llm.Client
	Confirm func(prompt string) bool
	Now     func() time.Time

	ps     []*Pipeline
	failed map[*Pipeline]error
}

func (m *Multi) prefixed(label string) func(string, ...any) {
	if !m.Cfg.Multi() {
		return m.Logf
	}
	return func(format string, args ...any) {
		m.Logf("[%s] %s", label, fmt.Sprintf(format, args...))
	}
}

// Pipelines returns one pipeline per repo, created once.
func (m *Multi) Pipelines() []*Pipeline {
	if m.ps == nil {
		m.failed = map[*Pipeline]error{}
		for _, r := range m.Cfg.Repos {
			m.ps = append(m.ps, &Pipeline{Cfg: m.Cfg.ForRepo(r), Logf: m.prefixed(r.String()), LLM: m.LLM, Confirm: m.Confirm, Now: m.Now})
		}
	}
	return m.ps
}

func (m *Multi) live() []*Pipeline {
	var out []*Pipeline
	for _, p := range m.Pipelines() {
		if m.failed[p] == nil {
			out = append(out, p)
		}
	}
	return out
}

// Each runs fn for every repo that hasn't failed yet. An interrupted context
// stops everything; any other error marks just that repo as failed.
func (m *Multi) Each(ctx context.Context, fn func(*Pipeline) error) error {
	for _, p := range m.live() {
		if err := fn(p); err != nil {
			if ctx.Err() != nil || len(m.Pipelines()) == 1 {
				return err
			}
			m.failed[p] = err
			p.Logf("failed: %v", err)
		}
	}
	return nil
}

// Err summarizes the repos that failed.
func (m *Multi) Err() error {
	var errs []error
	for _, p := range m.Pipelines() {
		if err := m.failed[p]; err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", p.Cfg.Repo, err))
		}
	}
	if len(errs) > 0 {
		m.Logf("%d of %d repos failed", len(errs), len(m.Pipelines()))
	}
	return errors.Join(errs...)
}

// Run executes every stage for every repo, then the combined rule set.
func (m *Multi) Run(ctx context.Context) error {
	if err := m.Each(ctx, func(p *Pipeline) error { return p.Fetch(ctx) }); err != nil {
		return err
	}
	if err := m.Each(ctx, func(p *Pipeline) error { _, err := p.Digest(); return err }); err != nil {
		return err
	}
	if m.Cfg.DryRun {
		if _, err := m.Estimate(ctx, true); err != nil {
			return err
		}
		return m.Err()
	}
	if err := m.Extract(ctx); err != nil {
		return err
	}
	return m.Synthesize(ctx, true)
}

// Estimate prints one estimate covering every repo's unsent extraction work.
func (m *Multi) Estimate(ctx context.Context, calibrate bool) (Estimate, error) {
	var total Estimate
	err := m.Each(ctx, func(p *Pipeline) error {
		plan, err := p.PlanExtract()
		if err != nil {
			return err
		}
		total = total.Add(p.estimate(ctx, plan.Unsent, calibrate && total.Requests == 0))
		return nil
	})
	if err != nil {
		return total, err
	}
	if total.Model == "" {
		total = estimate(m.Cfg.Model, nil, 1, !m.Cfg.Sync)
	}
	fmt.Fprint(errWriter, total.String())
	return total, nil
}

// Extract approves the spend for all repos at once, submits every repo's
// batches, and only then waits, sharing one --max-wait deadline.
func (m *Multi) Extract(ctx context.Context) error {
	plans := map[*Pipeline]*ExtractPlan{}
	var total Estimate
	err := m.Each(ctx, func(p *Pipeline) error {
		plan, err := p.PlanExtract()
		if err != nil {
			return err
		}
		plans[p] = plan
		total = total.Add(p.estimate(ctx, plan.Unsent, false))
		return nil
	})
	if err != nil {
		return err
	}
	what := "extraction"
	if m.Cfg.Multi() {
		what = fmt.Sprintf("extraction for %d repos", len(plans))
	}
	if err := approve(m.Cfg, m.Confirm, total, what); err != nil {
		return err
	}
	if err := m.Each(ctx, func(p *Pipeline) error { return p.SubmitExtract(ctx, plans[p]) }); err != nil {
		return err
	}
	var deadline time.Time
	if m.Cfg.MaxWait > 0 {
		deadline = time.Now().Add(m.Cfg.MaxWait)
	}
	return m.Each(ctx, func(p *Pipeline) error {
		wait := time.Duration(0)
		if !deadline.IsZero() {
			// Past the deadline, still poll once to pick up finished batches.
			wait = max(time.Until(deadline), time.Millisecond)
		}
		return p.CollectExtract(ctx, plans[p], wait)
	})
}

// Synthesize updates each repo's rules (skipping repos still waiting for
// their first extraction) and then the combined rule set. With fromRun, it
// also reports failed repos.
func (m *Multi) Synthesize(ctx context.Context, fromRun bool) error {
	synth := func(p *Pipeline) error { return p.Synthesize(ctx) }
	if fromRun || m.Cfg.Multi() {
		synth = func(p *Pipeline) error { return p.synthesizeIfReady(ctx) }
	}
	if err := m.Each(ctx, synth); err != nil {
		return err
	}
	if m.Cfg.Combined {
		if err := m.Combined(ctx); err != nil {
			if ctx.Err() != nil {
				return err
			}
			m.Logf("combined: failed: %v", err)
			return errors.Join(m.Err(), fmt.Errorf("combined: %w", err))
		}
	}
	return m.Err()
}

func (m *Multi) combinedPipeline() *Pipeline {
	cfg := *m.Cfg
	cfg.Repo = config.Repo{}
	cfg.Out = filepath.Join(m.Cfg.OutDir, "combined", "rules.json")
	cfg.ExportDir = filepath.Join(m.Cfg.ExportDir, "combined")
	return &Pipeline{Cfg: &cfg, Logf: m.prefixed("combined"), LLM: m.LLM, Now: m.Now,
		Dir: filepath.Join(m.Cfg.CacheDir, "_combined")}
}

// Combined builds one rule set from every repo's candidates. Candidate IDs
// and evidence carry their repo, and a rule seen in only one repo can't be
// golden for all of them.
//
// It reads every repo's cached candidates, including repos that failed a
// stage in this run: leaving one out would strip its evidence from the
// combined rules rather than just delay its new PRs.
func (m *Multi) Combined(ctx context.Context) error {
	cp := m.combinedPipeline()
	cs := CandidateSet{ByGen: map[string][]rules.Candidate{}}
	var stats output.Stats
	var repos []string
	for _, p := range m.Pipelines() {
		pcs, err := p.Candidates()
		if err != nil {
			return err
		}
		if len(pcs.All) == 0 {
			continue
		}
		repo := p.Cfg.Repo.String()
		repos = append(repos, repo)
		keys := make([]string, 0, len(pcs.ByGen))
		for k := range pcs.ByGen {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			for _, c := range pcs.ByGen[k] {
				c.ID = repo + ":" + c.ID
				c.Repo = repo
				c.Evidence = slices.Clone(c.Evidence)
				for i := range c.Evidence {
					c.Evidence[i].Repo = repo
				}
				cs.All = append(cs.All, c)
				cs.ByGen[repo+"#"+k] = append(cs.ByGen[repo+"#"+k], c)
			}
		}
		ps, err := p.stats(len(pcs.All))
		if err != nil {
			return err
		}
		stats.PRsScanned += ps.PRsScanned
		stats.PRsWithDiscussion += ps.PRsWithDiscussion
		stats.Threads += ps.Threads
		stats.Comments += ps.Comments
		stats.Chunks += ps.Chunks
		stats.Candidates += ps.Candidates
	}
	if len(repos) < 2 {
		cp.Logf("needs candidates from at least two repos; skipping for now")
		return nil
	}
	st, changed, err := cp.synthesize(ctx, cs, true)
	if err != nil {
		return err
	}
	return cp.writeOutput(st, changed, stats, "", repos)
}

// stats are the report statistics from this repo's digest and ledger.
func (p *Pipeline) stats(candidates int) (output.Stats, error) {
	l, err := p.loadLedger()
	if err != nil {
		return output.Stats{}, err
	}
	var ds DigestSummary
	if _, err := store.ReadJSON(p.dir("digest.json"), &ds); err != nil {
		return output.Stats{}, err
	}
	return output.Stats{
		PRsScanned:        ds.PRsIn,
		PRsWithDiscussion: ds.PRsKept,
		Threads:           ds.Threads,
		Comments:          ds.Comments,
		Chunks:            l.totalChunks(),
		Candidates:        candidates,
	}, nil
}

// Status prints where every repo stands: fetch watermark, generations,
// batches in flight, and the current rule set.
func (m *Multi) Status(w io.Writer) error {
	for _, p := range m.Pipelines() {
		if err := p.Status(w); err != nil {
			return err
		}
	}
	if m.Cfg.Combined {
		cp := m.combinedPipeline()
		fmt.Fprintln(w, "combined")
		cp.statusRules(w)
	}
	return nil
}

func (p *Pipeline) Status(w io.Writer) error {
	fmt.Fprintln(w, p.Cfg.Repo.String())
	fs, err := github.LoadState(p.dir())
	if err != nil {
		return err
	}
	switch {
	case fs.Key == "":
		fmt.Fprintln(w, "  fetch:       never run")
	case fs.Refresh != nil:
		fmt.Fprintf(w, "  fetch:       %d PRs; a refresh is in progress (%d PRs so far)\n", fs.PRs, fs.Refresh.PRs)
	case fs.Watermark.IsZero():
		fmt.Fprintf(w, "  fetch:       %d PRs, initial fetch not finished\n", fs.PRs)
	default:
		fmt.Fprintf(w, "  fetch:       %d PRs, up to date as of %s (newest PR update %s)\n",
			fs.PRs, stamp(fs.LastRefresh), stamp(fs.Watermark))
	}
	l, err := p.loadLedger()
	if err != nil {
		return err
	}
	if len(l.Gens) == 0 {
		fmt.Fprintln(w, "  generations: none")
	}
	for _, g := range l.Gens {
		line := fmt.Sprintf("  %s:       %-9s %d PRs, %d chunks", g.Key(), g.Status, len(g.PRs), g.Chunks)
		if g.Status == genExtracted {
			line += fmt.Sprintf(", %d candidates (%s)", g.Candidates, stamp(g.ExtractedAt))
		}
		fmt.Fprintln(w, line)
	}
	r := &llm.Runner{Dir: p.dir("extract")}
	if n, err := r.InFlight(); err != nil {
		return err
	} else if n > 0 {
		fmt.Fprintf(w, "  batches:     %d in flight\n", n)
	}
	unfolded := 0
	if st, ok, _ := p.loadSynthState(); ok {
		for _, g := range l.extracted() {
			if !slices.Contains(st.Folded, g.Key()) {
				unfolded++
			}
		}
	}
	p.statusRules(w)
	if unfolded > 0 {
		fmt.Fprintf(w, "  pending:     %d extracted generation(s) not folded into the rules yet\n", unfolded)
	}
	return nil
}

func (p *Pipeline) statusRules(w io.Writer) {
	st, ok, err := p.loadSynthState()
	if err != nil || !ok {
		fmt.Fprintln(w, "  rules:       not synthesized yet")
		return
	}
	t := output.Tally(st.Rules)
	var parts []string
	for _, tier := range rules.Tiers {
		parts = append(parts, fmt.Sprintf("%s %d", tier, t[string(tier)]))
	}
	fmt.Fprintf(w, "  rules:       %d (%s), last changed %s\n", len(st.Rules), strings.Join(parts, ", "), stamp(st.UpdatedAt))
}

func stamp(t time.Time) string {
	if t.IsZero() {
		return "never"
	}
	return t.UTC().Format("2006-01-02 15:04 UTC")
}
