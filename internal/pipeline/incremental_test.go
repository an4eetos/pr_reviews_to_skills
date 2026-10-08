package pipeline

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/an4eetos/pr_reviews_to_skills/internal/config"
	"github.com/an4eetos/pr_reviews_to_skills/internal/digest"
	"github.com/an4eetos/pr_reviews_to_skills/internal/github"
	"github.com/an4eetos/pr_reviews_to_skills/internal/llm"
	"github.com/an4eetos/pr_reviews_to_skills/internal/output"
	"github.com/an4eetos/pr_reviews_to_skills/internal/rules"
	"github.com/an4eetos/pr_reviews_to_skills/internal/store"
)

func init() { errWriter = io.Discard }

func (f *fakeLLM) take() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	ids := f.ids
	f.ids = nil
	return ids
}

func prefixes(ids []string) []string {
	var out []string
	for _, id := range ids {
		switch {
		case strings.Contains(id, "-chunk-"):
			out = append(out, id[:len("g0000")]+"-chunk")
		default:
			out = append(out, id[:strings.Index(id, "-")])
		}
	}
	slices.Sort(out)
	return out
}

func readReport(t *testing.T, path string) output.Report {
	t.Helper()
	var rep output.Report
	if ok, err := store.ReadJSON(path, &rep); !ok || err != nil {
		t.Fatalf("%s: %v %v", path, ok, err)
	}
	if err := output.Validate(rep); err != nil {
		t.Fatal(err)
	}
	return rep
}

func TestIncrementalRuns(t *testing.T) {
	srv, gh := newGH(t)
	p, f := testPipeline(t, srv, true)
	ctx := context.Background()
	if err := p.Run(ctx); err != nil {
		t.Fatal(err)
	}
	if got := prefixes(f.take()); !slices.Equal(got, []string{"g0001-chunk", "score"}) {
		t.Errorf("first run calls %v", got)
	}
	first, _ := os.ReadFile(p.Cfg.Out)

	// Nothing new on GitHub: no LLM calls and rules.json stays byte-identical.
	if err := p.Run(ctx); err != nil {
		t.Fatal(err)
	}
	if got := f.take(); len(got) != 0 {
		t.Errorf("idle run made calls %v", got)
	}
	// A PR bumped by CI with the same discussion is not re-extracted either.
	gh.set(3, now.Add(time.Minute), "Wrap this error with context using %w.")
	if err := p.Run(ctx); err != nil {
		t.Fatal(err)
	}
	if got := f.take(); len(got) != 0 {
		t.Errorf("updatedAt-only change made calls %v", got)
	}
	if again, _ := os.ReadFile(p.Cfg.Out); !bytes.Equal(first, again) {
		t.Errorf("rules.json rewritten without changes")
	}

	// A new PR is extracted alone and folded into the existing rule.
	gh.set(4, now.Add(time.Hour), "Please wrap errors with context, we keep losing the call site.")
	if err := p.Run(ctx); err != nil {
		t.Fatal(err)
	}
	if got := prefixes(f.take()); !slices.Equal(got, []string{"fold", "g0002-chunk", "score"}) {
		t.Errorf("new-PR run calls %v", got)
	}
	l, _ := p.loadLedger()
	if len(l.Gens) != 2 || len(l.Gens[1].PRs) != 1 || l.Gens[1].PRs[4] == "" {
		t.Errorf("generations %+v", l.Gens)
	}
	rep := readReport(t, p.Cfg.Out)
	if len(rep.Rules) != 1 || rep.Rules[0].Metrics.DistinctPRs != 4 || rep.Stats.Candidates != 2 {
		t.Fatalf("after new PR: %+v / %+v", rep.Stats, rep.Rules)
	}

	// PR 2's discussion changes: it is re-extracted, and its old evidence is
	// superseded rather than counted twice.
	gh.set(2, now.Add(2*time.Hour), "Wrap this error with context using %w, and log it once.")
	if err := p.Run(ctx); err != nil {
		t.Fatal(err)
	}
	// The rule's metrics come out the same, so its score request is cached.
	if got := prefixes(f.take()); !slices.Equal(got, []string{"fold", "g0003-chunk"}) {
		t.Errorf("changed-PR run calls %v", got)
	}
	rep = readReport(t, p.Cfg.Out)
	r := rep.Rules[0]
	n2 := 0
	for _, e := range r.Evidence {
		if e.PR == 2 {
			n2++
			if !strings.Contains(e.Quote, "log it once") && !strings.Contains(e.Quote, "Wrap this error") {
				t.Errorf("PR 2 evidence %+v", e)
			}
		}
	}
	if r.Metrics.DistinctPRs != 4 || n2 != 1 {
		t.Errorf("after changed PR: prs %d, PR 2 evidence %d", r.Metrics.DistinctPRs, n2)
	}
}

func TestBatchMaxWaitResumesNextRun(t *testing.T) {
	srv, _ := newGH(t)
	p, f := testPipeline(t, srv, false)
	p.Cfg.MaxWait = time.Millisecond
	f.hold = true
	if err := p.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if store.Exists(p.Cfg.Out) || len(f.batches) != 1 {
		t.Fatalf("pending run wrote output or batches %d", len(f.batches))
	}
	l, _ := p.loadLedger()
	if l.Gens[0].Status != genSubmitted {
		t.Errorf("gen status %s", l.Gens[0].Status)
	}

	// The next scheduled run collects the batch instead of resubmitting.
	f.hold = false
	if err := p.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(f.batches) != 1 {
		t.Errorf("resubmitted: %d batches", len(f.batches))
	}
	readReport(t, p.Cfg.Out)
}

func TestMaxCostAbortsBeforeSpending(t *testing.T) {
	srv, _ := newGH(t)
	p, f := testPipeline(t, srv, true)
	p.Cfg.MaxCost = 0.000001
	err := p.Run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "exceeds --max-cost") {
		t.Fatalf("err = %v", err)
	}
	if f.calls != 0 {
		t.Errorf("made %d calls", f.calls)
	}
	// Nothing was sealed, so a later run with a higher cap does the work.
	if l, _ := p.loadLedger(); l.Gens[0].Status != genOpen {
		t.Errorf("gen sealed after abort: %s", l.Gens[0].Status)
	}
}

func TestMigratesPreGenerationCache(t *testing.T) {
	srv, _ := newGH(t)
	p, f := testPipeline(t, srv, true)
	ctx := context.Background()
	// Produce an old-layout cache: units.jsonl, chunks.jsonl, candidates.jsonl
	// and scored.json at the top level, no ledger.
	if err := p.Fetch(ctx); err != nil {
		t.Fatal(err)
	}
	prs, _ := github.LoadAll(p.dir())
	units, _ := digest.Build(prs, digest.Options{})
	store.WriteJSONL(p.dir("units.jsonl"), units)
	store.WriteJSONL(p.dir("chunks.jsonl"), digest.Pack(units, 40000))
	var ev []rules.Evidence
	for _, u := range units {
		c := u.Threads[0].Comments[0]
		ev = append(ev, rules.Evidence{PR: u.PR, Ref: c.Ref, URL: c.URL, Author: c.Author, Role: c.Role, Date: c.Date, Quote: c.Body, Outcome: "accepted"})
	}
	cand := rules.Candidate{ID: "chunk-00001.1", Chunk: "chunk-00001", Title: "Wrap errors with context", Statement: "Wrap errors.", Category: "error-handling", Kind: "do", Evidence: ev}
	store.WriteJSONL(p.dir("candidates.jsonl"), []rules.Candidate{cand})
	scored := []rules.Rule{{Title: "Wrap errors with context", Rule: "Wrap errors.", Category: "error-handling", Kind: "do",
		Tier: rules.TierGolden, Confidence: 0.9, TierReason: "old", Members: []string{cand.ID}, Evidence: ev}}
	store.WriteJSON(p.dir("scored.json"), scored)

	if err := p.Run(ctx); err != nil {
		t.Fatal(err)
	}
	if got := f.take(); len(got) != 0 {
		t.Errorf("migrated cache re-paid for %v", got)
	}
	rep := readReport(t, p.Cfg.Out)
	if len(rep.Rules) != 1 || rep.Rules[0].TierReason != "old" {
		t.Errorf("rules %+v", rep.Rules)
	}
}

func multiConfig(t *testing.T, srv string, repos ...string) *config.Config {
	dir := t.TempDir()
	rs, err := config.ParseRepos(repos)
	if err != nil {
		t.Fatal(err)
	}
	return &config.Config{
		Repos: rs, CacheDir: filepath.Join(dir, "cache"), OutDir: filepath.Join(dir, "out"), Out: filepath.Join(dir, "rules.json"),
		ExportDir: filepath.Join(dir, "export"), GitHubToken: "tok", GitHubAPIURL: srv + "/graphql",
		States: []string{"MERGED"}, PageSize: 25, ChunkTokens: 40000, Model: "claude-opus-5-5",
		Concurrency: 2, PollInterval: time.Millisecond, ExtractEffort: "medium", SynthEffort: "high",
	}
}

func TestMultiRepoWithCombined(t *testing.T) {
	srv, _ := newGH(t)
	f := &fakeLLM{batches: map[string][]llm.Request{}}
	cfg := multiConfig(t, srv.URL, "o/a", "o/b")
	cfg.Combined = true
	cfg.Export = []string{"manual"}
	confirms := 0
	m := &Multi{Cfg: cfg, Logf: t.Logf, LLM: f, Now: func() time.Time { return now },
		Confirm: func(q string) bool { confirms++; return strings.Contains(q, "2 repos") }}
	if err := m.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if confirms != 1 || len(f.batches) != 2 || !slices.Equal(f.events[:2], []string{"submit", "submit"}) {
		t.Errorf("confirms %d, batches %d, events %v: want one approval and every repo's batch submitted before polling", confirms, len(f.batches), f.events)
	}
	for _, r := range []string{"a", "b"} {
		rep := readReport(t, filepath.Join(cfg.OutDir, "o", r, "rules.json"))
		if rep.Repo != "o/"+r || len(rep.Rules) != 1 || rep.Rules[0].Metrics.DistinctPRs != 3 {
			t.Errorf("o/%s: %+v", r, rep)
		}
		if !store.Exists(filepath.Join(cfg.ExportDir, "o", r, "PR-REVIEW-HANDBOOK.md")) {
			t.Errorf("o/%s: handbook not exported", r)
		}
	}
	comb := readReport(t, filepath.Join(cfg.OutDir, "combined", "rules.json"))
	if comb.Repo != "" || !slices.Equal(comb.Repos, []string{"o/a", "o/b"}) || len(comb.Rules) != 1 {
		t.Fatalf("combined %+v", comb)
	}
	cr := comb.Rules[0]
	if cr.Metrics.DistinctPRs != 6 || cr.Metrics.DistinctRepos != 2 || !slices.Equal(cr.Repos, []string{"o/a", "o/b"}) || cr.Tier != rules.TierGolden {
		t.Errorf("combined rule %+v", cr)
	}
	if !store.Exists(filepath.Join(cfg.ExportDir, "combined", "PR-REVIEW-HANDBOOK.md")) {
		t.Errorf("combined handbook not exported")
	}

	var status bytes.Buffer
	if err := m.Status(&status); err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{"o/a\n", "g0001:       extracted", "rules:       1 (golden 1", "combined\n"} {
		if !strings.Contains(status.String(), s) {
			t.Errorf("status missing %q:\n%s", s, status.String())
		}
	}
}

func TestMultiRepoFailureDoesNotStopOthers(t *testing.T) {
	srv, _ := newGH(t)
	f := &fakeLLM{batches: map[string][]llm.Request{}}
	cfg := multiConfig(t, srv.URL, "o/a", "o/b")
	cfg.Sync = true
	m := &Multi{Cfg: cfg, Logf: t.Logf, LLM: f, Now: func() time.Time { return now }}
	ps := m.Pipelines()
	ps[0].Cfg.GitHubToken = "" // o/a can't be fetched
	err := m.Run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "o/a: GITHUB_TOKEN") {
		t.Fatalf("err = %v", err)
	}
	readReport(t, filepath.Join(cfg.OutDir, "o", "b", "rules.json"))
	if store.Exists(filepath.Join(cfg.OutDir, "o", "a", "rules.json")) {
		t.Errorf("failed repo wrote output")
	}
}

func TestCombinedKeepsReposThatFailedThisRun(t *testing.T) {
	srv, gh := newGH(t)
	f := &fakeLLM{batches: map[string][]llm.Request{}}
	cfg := multiConfig(t, srv.URL, "o/a", "o/b")
	cfg.Combined, cfg.Sync = true, true
	newMulti := func() *Multi { return &Multi{Cfg: cfg, Logf: t.Logf, LLM: f, Now: func() time.Time { return now }} }
	if err := newMulti().Run(context.Background()); err != nil {
		t.Fatal(err)
	}

	// Next run: o/b gets a new PR, but o/a can't be fetched.
	gh.set(4, now.Add(time.Hour), "Please wrap errors with context.")
	m := newMulti()
	m.Pipelines()[0].Cfg.GitHubToken = ""
	if err := m.Run(context.Background()); err == nil {
		t.Fatal("want o/a's failure reported")
	}
	comb := readReport(t, filepath.Join(cfg.OutDir, "combined", "rules.json"))
	if r := comb.Rules[0]; r.Metrics.DistinctRepos != 2 || r.Metrics.DistinctPRs != 7 {
		t.Errorf("combined rule after a failed repo: repos %d, prs %d", r.Metrics.DistinctRepos, r.Metrics.DistinctPRs)
	}
}
