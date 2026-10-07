package pipeline

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/an4eetos/pr_reviews_to_skills/internal/config"
	"github.com/an4eetos/pr_reviews_to_skills/internal/llm"
	"github.com/an4eetos/pr_reviews_to_skills/internal/output"
	"github.com/an4eetos/pr_reviews_to_skills/internal/store"
)

var now = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

func ghServer(t *testing.T) *httptest.Server {
	comment := func(id int, login, assoc, body string) map[string]any {
		return map[string]any{
			"id": fmt.Sprint(id), "url": fmt.Sprintf("https://github.com/o/r/pull/x#c%d", id), "body": body,
			"createdAt": now.AddDate(0, -1, 0), "diffHunk": "@@ -1 +1 @@\n+return err",
			"author": map[string]any{"login": login, "__typename": "User"}, "authorAssociation": assoc,
			"thumbsUp": map[string]any{"totalCount": 0}, "thumbsDown": map[string]any{"totalCount": 0},
		}
	}
	empty := map[string]any{"totalCount": 0, "pageInfo": map[string]any{"hasNextPage": false}, "nodes": []any{}}
	var prs []any
	for i := 1; i <= 3; i++ {
		prs = append(prs, map[string]any{
			"number": i, "title": fmt.Sprintf("PR %d", i), "body": "", "url": fmt.Sprintf("https://github.com/o/r/pull/%d", i),
			"state": "MERGED", "merged": true, "createdAt": now, "updatedAt": now.Add(-time.Duration(i) * time.Hour),
			"author": map[string]any{"login": "bob", "__typename": "User"}, "authorAssociation": "CONTRIBUTOR",
			"labels": map[string]any{"nodes": []any{}}, "files": map[string]any{"totalCount": 1, "nodes": []any{map[string]any{"path": "internal/db/db.go"}}},
			"reviews": empty, "comments": empty,
			"reviewThreads": map[string]any{
				"totalCount": 1, "pageInfo": map[string]any{"hasNextPage": false},
				"nodes": []any{map[string]any{
					"id": "t", "path": "internal/db/db.go", "line": 10, "isResolved": true, "isOutdated": true,
					"comments": map[string]any{"totalCount": 2, "nodes": []any{
						comment(i*10, fmt.Sprintf("maint%d", i), "MEMBER", "Wrap this error with context using %w."),
						comment(i*10+1, "bob", "CONTRIBUTOR", "Done"),
					}},
				}},
			},
		})
	}
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{
			"rateLimit": map[string]any{"cost": 1, "remaining": 4999, "resetAt": now},
			"repository": map[string]any{"pullRequests": map[string]any{
				"pageInfo": map[string]any{"hasNextPage": false, "endCursor": "x"},
				"nodes":    prs,
			}},
		}})
	}))
}

// fakeLLM answers each stage by request ID prefix and also implements the
// batch API in memory.
type fakeLLM struct {
	mu      sync.Mutex
	batches map[string][]llm.Request
	calls   int
}

var refRe = regexp.MustCompile(`\[(\d+\.\d+)\] maintainer`)
var itemRe = regexp.MustCompile(`\[(i\d+)\]`)

func (f *fakeLLM) answer(req llm.Request) llm.Result {
	f.calls++
	var out any
	switch {
	case strings.HasPrefix(req.ID, "chunk-"):
		var ev []any
		for _, m := range refRe.FindAllStringSubmatch(req.Prompt, -1) {
			ev = append(ev, map[string]any{"ref": m[1], "quote": "Wrap this error with context", "outcome": "accepted"})
		}
		out = map[string]any{"candidates": []any{map[string]any{
			"title": "Wrap errors with context", "statement": "Wrap returned errors with fmt.Errorf and %w.",
			"rationale": "Keeps call-site context.", "category": "error-handling", "kind": "do", "applies_when": "",
			"paths": []string{"internal/**"}, "languages": []string{"go"}, "bad_example": "return err",
			"good_example": `return fmt.Errorf("load: %w", err)`, "evidence": ev,
		}}}
	case strings.HasPrefix(req.ID, "merge-"):
		var members []string
		for _, m := range itemRe.FindAllStringSubmatch(req.Prompt, -1) {
			members = append(members, m[1])
		}
		out = map[string]any{"rules": []any{map[string]any{
			"title": "Wrap errors with context", "rule": "Wrap returned errors with fmt.Errorf and %w.", "rationale": "context",
			"kind": "do", "applies_when": "", "paths": []string{"internal/**"}, "languages": []string{"go"},
			"bad_example": "return err", "good_example": "return fmt.Errorf(...)", "members": members, "opposing": []string{},
		}}, "discarded": []string{}}
	case strings.HasPrefix(req.ID, "score-"):
		out = map[string]any{"scores": []any{map[string]any{
			"id": "r1", "tier": "golden", "confidence": 0.9, "tier_reason": "Three maintainers, always applied.", "applies_when": "",
		}}}
	default:
		return llm.Result{ID: req.ID, Error: "unexpected request"}
	}
	b, _ := json.Marshal(out)
	return llm.Result{ID: req.ID, Text: string(b), StopReason: "end_turn"}
}

func (f *fakeLLM) Complete(_ context.Context, req llm.Request) (llm.Result, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.answer(req), nil
}

func (f *fakeLLM) SubmitBatch(_ context.Context, reqs []llm.Request) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	id := fmt.Sprintf("b%d", len(f.batches)+1)
	f.batches[id] = reqs
	return id, nil
}

func (f *fakeLLM) BatchStatus(context.Context, string) (llm.BatchStatus, error) {
	return llm.BatchStatus{Ended: true}, nil
}

func (f *fakeLLM) BatchResults(_ context.Context, id string) ([]llm.Result, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []llm.Result
	for _, q := range f.batches[id] {
		out = append(out, f.answer(q))
	}
	return out, nil
}

func (f *fakeLLM) CountTokens(context.Context, llm.Request) (int64, error) { return 1000, nil }

func testPipeline(t *testing.T, srv *httptest.Server, sync bool) (*Pipeline, *fakeLLM) {
	dir := t.TempDir()
	f := &fakeLLM{batches: map[string][]llm.Request{}}
	cfg := &config.Config{
		Repo: config.Repo{Host: "github.com", Owner: "o", Name: "r"}, CacheDir: filepath.Join(dir, "cache"),
		Out: filepath.Join(dir, "rules.json"), GitHubToken: "tok", GitHubAPIURL: srv.URL + "/graphql",
		States: []string{"MERGED"}, PageSize: 25, ChunkTokens: 40000, Model: "claude-opus-5-5", Sync: sync,
		Concurrency: 2, PollInterval: time.Millisecond, ExtractEffort: "medium", SynthEffort: "high",
	}
	return &Pipeline{Cfg: cfg, Logf: t.Logf, LLM: f, Now: func() time.Time { return now }}, f
}

func TestRunEndToEnd(t *testing.T) {
	srv := ghServer(t)
	defer srv.Close()
	for _, sync := range []bool{false, true} {
		t.Run(fmt.Sprintf("sync=%v", sync), func(t *testing.T) {
			p, f := testPipeline(t, srv, sync)
			if err := p.Run(context.Background()); err != nil {
				t.Fatal(err)
			}
			var rep output.Report
			if ok, err := store.ReadJSON(p.Cfg.Out, &rep); !ok || err != nil {
				t.Fatalf("rules.json: %v %v", ok, err)
			}
			if err := output.Validate(rep); err != nil {
				t.Fatal(err)
			}
			if rep.Stats.PRsScanned != 3 || rep.Stats.PRsWithDiscussion != 3 || rep.Stats.Candidates != 1 || len(rep.Rules) != 1 {
				t.Fatalf("stats %+v rules %d", rep.Stats, len(rep.Rules))
			}
			r := rep.Rules[0]
			if r.ID != "wrap-errors-with-context" || r.Tier != "golden" || r.Metrics.DistinctPRs != 3 || r.Metrics.DistinctReviewers != 3 || !r.Metrics.MaintainerEndorsed {
				t.Errorf("rule %+v", r)
			}
			if len(r.Evidence) != 3 || !strings.HasPrefix(r.Evidence[0].URL, "https://github.com/o/r/pull/x#c") {
				t.Errorf("evidence %+v", r.Evidence)
			}
			if sync == (len(f.batches) > 0) {
				t.Errorf("sync=%v but %d batches submitted", sync, len(f.batches))
			}

			// Everything is cached: a second run makes no LLM calls.
			calls := f.calls
			if err := p.Run(context.Background()); err != nil {
				t.Fatal(err)
			}
			if f.calls != calls {
				t.Errorf("second run made %d LLM calls", f.calls-calls)
			}
		})
	}
}

func TestDryRunStopsBeforeLLMSpend(t *testing.T) {
	srv := ghServer(t)
	defer srv.Close()
	p, f := testPipeline(t, srv, false)
	p.Cfg.DryRun = true
	if err := p.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if f.calls != 0 || len(f.batches) != 0 || store.Exists(p.Cfg.Out) {
		t.Errorf("dry run spent: calls %d, batches %d", f.calls, len(f.batches))
	}
}

func TestDeclinedConfirmationAborts(t *testing.T) {
	srv := ghServer(t)
	defer srv.Close()
	p, f := testPipeline(t, srv, true)
	p.Confirm = func(string) bool { return false }
	if err := p.Run(context.Background()); err == nil || !strings.Contains(err.Error(), "aborted") {
		t.Fatalf("err = %v", err)
	}
	if f.calls != 0 {
		t.Errorf("made %d calls after declining", f.calls)
	}
}

func TestEstimate(t *testing.T) {
	reqs := []llm.Request{{System: strings.Repeat("s", 3000), Prompt: strings.Repeat("p", 296997)}}
	e := estimate("claude-opus-5-5", reqs, 1, true)
	if e.InputTokens != 100000 || e.OutputTokens != outputPerExtract {
		t.Errorf("tokens %d/%d", e.InputTokens, e.OutputTokens)
	}
	// (0.1M * $4 + 0.005M * $20) = $0.50; batch halves extract to $0.25.
	if e.ExtractUSD < 0.2499 || e.ExtractUSD > 0.2501 || e.SynthUSD < 0.0749 || e.SynthUSD > 0.0751 {
		t.Errorf("usd %.4f / %.4f", e.ExtractUSD, e.SynthUSD)
	}
	if u := estimate("some-new-model", reqs, 1, true); u.KnownPrice || !strings.Contains(u.String(), "token counts only") {
		t.Errorf("unknown model: %+v", u)
	}
}
