package synthesize

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/an4eetos/pr_reviews_to_skills/internal/llm"
	"github.com/an4eetos/pr_reviews_to_skills/internal/rules"
)

type fakeLLM struct {
	mu      sync.Mutex
	handle  func(req llm.Request) string
	prompts []string
}

func (f *fakeLLM) Complete(_ context.Context, req llm.Request) (llm.Result, error) {
	f.mu.Lock()
	f.prompts = append(f.prompts, req.ID)
	f.mu.Unlock()
	return llm.Result{ID: req.ID, Text: f.handle(req), StopReason: "end_turn"}, nil
}
func (f *fakeLLM) SubmitBatch(context.Context, []llm.Request) (string, error) { panic("unused") }
func (f *fakeLLM) BatchStatus(context.Context, string) (llm.BatchStatus, error) {
	panic("unused")
}
func (f *fakeLLM) BatchResults(context.Context, string) ([]llm.Result, error) { panic("unused") }
func (f *fakeLLM) CountTokens(context.Context, llm.Request) (int64, error)    { return 0, nil }

var now = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

func ev(pr int, ref, author, role, outcome string) rules.Evidence {
	return rules.Evidence{PR: pr, Ref: ref, URL: "https://x/" + ref, Author: author, Role: role, Outcome: outcome, Date: now.AddDate(0, -1, 0)}
}

func cand(id, cat, statement string, evidence ...rules.Evidence) rules.Candidate {
	return rules.Candidate{ID: id, Category: cat, Kind: "do", Title: statement, Statement: statement, Evidence: evidence}
}

func rule(title string, members, opposing []string, when string) map[string]any {
	return map[string]any{
		"title": title, "rule": title + ".", "rationale": "why", "kind": "do", "applies_when": when,
		"paths": []string{}, "languages": []string{"go"}, "bad_example": "", "good_example": "",
		"members": members, "opposing": opposing,
	}
}

func mustJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

func TestMergeAndScore(t *testing.T) {
	cands := []rules.Candidate{
		cand("c1", "error-handling", "Wrap errors", ev(1, "1.1", "alice", "maintainer", "accepted")),
		cand("c2", "error-handling", "Add context to errors",
			ev(2, "2.1", "bob", "maintainer", "accepted"),
			ev(3, "3.1", "carol", "contributor", "accepted"),
			ev(4, "4.1", "bob", "maintainer", "accepted")),
		cand("c3", "error-handling", "Return errors bare", ev(5, "5.1", "dan", "contributor", "accepted")),
		cand("c4", "error-handling", "Write good code", ev(6, "6.1", "erin", "contributor", "unclear")),
		cand("c5", "error-handling", "Log once at the top", ev(7, "7.1", "frank", "contributor", "accepted")),
		cand("c6", "testing", "Use table tests", ev(8, "8.1", "alice", "maintainer", "accepted")),
	}
	f := &fakeLLM{handle: func(req llm.Request) string {
		switch {
		case strings.HasPrefix(req.ID, "merge-error-handling-L1-G1"):
			if !strings.Contains(req.Prompt, "[i2] (do) Add context to errors: Add context to errors | prs: 3") {
				t.Errorf("merge prompt missing candidate line:\n%s", req.Prompt)
			}
			// i5 is "forgotten" by the model and must survive as its own rule.
			return mustJSON(map[string]any{
				"rules":     []any{rule("Wrap errors with context", []string{"i1", "[i2]"}, []string{"i3"}, "")},
				"discarded": []string{"i4"},
			})
		case strings.HasPrefix(req.ID, "score-error-handling-G1"):
			if !strings.Contains(req.Prompt, "prs=4 reviewers=3 maintainer=yes accepted=4 disputed=0 ignored=0 opposing=1") {
				t.Errorf("score prompt missing metrics:\n%s", req.Prompt)
			}
			return mustJSON(map[string]any{"scores": []any{
				map[string]any{"id": "r1", "tier": "golden", "confidence": 0.95, "tier_reason": "Repeated.", "applies_when": ""},
				map[string]any{"id": "r2", "tier": "golden", "confidence": 1.4, "tier_reason": "Sounds right.", "applies_when": ""},
			}})
		case strings.HasPrefix(req.ID, "score-testing-G1"):
			return `{"scores":[]}`
		}
		t.Errorf("unexpected request %s", req.ID)
		return "{}"
	}}
	opts := Options{Runner: &llm.Runner{Client: f, Dir: t.TempDir(), Concurrency: 2}, Effort: "high"}

	drafts, st, err := Merge(context.Background(), opts, cands)
	if err != nil {
		t.Fatal(err)
	}
	if len(drafts) != 3 || st.Discarded != 1 || st.Unassigned != 1 || st.Requests != 1 {
		t.Fatalf("drafts %d, stats %+v", len(drafts), st)
	}
	merged := drafts[0]
	if !slices.Equal(merged.Members, []string{"c1", "c2"}) || !slices.Equal(merged.Opposing, []string{"c3"}) {
		t.Errorf("members %v opposing %v", merged.Members, merged.Opposing)
	}
	if drafts[1].Members[0] != "c5" || drafts[2].Category != "testing" {
		t.Errorf("unassigned/single drafts: %+v / %+v", drafts[1], drafts[2])
	}

	AttachEvidence(drafts, cands, now)
	scored, err := Score(context.Background(), opts, drafts, nil)
	if err != nil {
		t.Fatal(err)
	}
	if scored[0].Tier != rules.TierGolden || scored[0].Confidence != 0.95 {
		t.Errorf("merged rule: %s %v %q", scored[0].Tier, scored[0].Confidence, scored[0].TierReason)
	}
	if scored[1].Tier != rules.TierConsider || scored[1].Confidence != 0.7 || !strings.Contains(scored[1].TierReason, "Downgraded from golden") {
		t.Errorf("single-PR golden not floored: %s %v %q", scored[1].Tier, scored[1].Confidence, scored[1].TierReason)
	}
	if scored[2].Tier != rules.TierConsider || !strings.Contains(scored[2].TierReason, "no grade") {
		t.Errorf("ungraded rule: %s %q", scored[2].Tier, scored[2].TierReason)
	}

	// A re-run reuses stored results: no new LLM calls.
	calls := len(f.prompts)
	if _, _, err := Merge(context.Background(), opts, cands); err != nil {
		t.Fatal(err)
	}
	if len(f.prompts) != calls {
		t.Errorf("re-run made %d new calls", len(f.prompts)-calls)
	}
}

var itemRe = regexp.MustCompile(`\[(i\d+)\]`)

func TestHierarchicalMerge(t *testing.T) {
	var cands []rules.Candidate
	for i := 1; i <= 8; i++ {
		cands = append(cands, cand(fmt.Sprintf("c%d", i), "style", strings.Repeat("long statement ", 20),
			ev(i, fmt.Sprintf("%d.1", i), "alice", "maintainer", "accepted")))
	}
	levels := map[string]int{}
	f := &fakeLLM{handle: func(req llm.Request) string {
		level := req.ID[len("merge-style-"):][:2]
		levels[level]++
		// Merge every item in the group into one rule.
		ids := itemRe.FindAllStringSubmatch(req.Prompt, -1)
		var members []string
		for _, m := range ids {
			members = append(members, m[1])
		}
		return mustJSON(map[string]any{"rules": []any{rule("Group "+level, members, []string{}, "")}, "discarded": []string{}})
	}}
	opts := Options{Runner: &llm.Runner{Client: f, Dir: t.TempDir()}, GroupTokens: 500}
	drafts, st, err := Merge(context.Background(), opts, cands)
	if err != nil {
		t.Fatal(err)
	}
	if levels["L1"] < 2 || levels["L2"] != 1 || st.Levels != 2 {
		t.Errorf("levels %v, stats %+v", levels, st)
	}
	if len(drafts) != 1 || len(drafts[0].Members) != 8 {
		t.Fatalf("drafts %+v", drafts)
	}
}

func TestFoldAssignsNewCandidatesAndReportsTouched(t *testing.T) {
	old := []rules.Candidate{
		cand("c1", "error-handling", "Wrap errors", ev(1, "1.1", "alice", "maintainer", "accepted")),
		cand("c2", "error-handling", "Never panic", ev(2, "2.1", "bob", "maintainer", "accepted")),
		cand("c9", "error-handling", "Use sentinel errors", ev(9, "9.1", "carol", "contributor", "accepted")),
	}
	existing := []rules.Rule{
		{Title: "Wrap errors", Rule: "Wrap errors.", Category: "error-handling", Kind: "do", Tier: rules.TierConsider, Confidence: 0.5, Members: []string{"c1"}},
		{Title: "Never panic", Rule: "Never panic.", Category: "error-handling", Kind: "dont", Tier: rules.TierConsider, Confidence: 0.6, Members: []string{"c2"}},
		{Title: "Use sentinel errors", Rule: "Use sentinel errors.", Category: "error-handling", Kind: "do", Tier: rules.TierConsider, Members: []string{"c9"}},
	}
	AttachEvidence(existing, old, now)

	newCands := []rules.Candidate{
		cand("c3", "error-handling", "Add context when wrapping", ev(3, "3.1", "dave", "maintainer", "accepted")),
		cand("c4", "error-handling", "Log errors once", ev(4, "4.1", "erin", "maintainer", "accepted")),
		cand("c5", "error-handling", "Be careful", ev(5, "5.1", "frank", "contributor", "unclear")),
		cand("c6", "testing", "Use t.Helper", ev(6, "6.1", "gus", "maintainer", "accepted")),
	}
	// PR 9 was re-extracted and the sentinel-errors rule did not come back:
	// c9 is gone from the candidate set.
	all := append([]rules.Candidate{old[0], old[1]}, newCands...)

	f := &fakeLLM{handle: func(req llm.Request) string {
		switch {
		case strings.HasPrefix(req.ID, "fold-error-handling"):
			if !strings.Contains(req.Prompt, "[R1] (do) Wrap errors: Wrap errors.") || !strings.Contains(req.Prompt, "[i3] (do) Be careful") {
				t.Errorf("fold prompt:\n%s", req.Prompt)
			}
			return mustJSON(map[string]any{
				"assignments": []any{map[string]any{"candidate": "i1", "rule": "R1", "relation": "member"}},
				"new_rules":   []any{rule("Log errors once", []string{"i2"}, []string{}, "")},
				"discarded":   []string{"i3"},
			})
		case strings.HasPrefix(req.ID, "merge-testing"):
			t.Errorf("a single new testing candidate needs no merge call")
		}
		return "{}"
	}}
	opts := Options{Runner: &llm.Runner{Client: f, Dir: t.TempDir(), Concurrency: 2}, Effort: "high"}
	out, touched, st, err := Fold(context.Background(), opts, existing, newCands, all, now)
	if err != nil {
		t.Fatal(err)
	}
	var titles []string
	for _, r := range out {
		titles = append(titles, r.Title)
	}
	if !slices.Equal(titles, []string{"Wrap errors", "Never panic", "Log errors once", "Use t.Helper"}) {
		t.Fatalf("rules %v", titles)
	}
	if !slices.Equal(touched, []bool{true, false, true, true}) {
		t.Errorf("touched %v", touched)
	}
	if !slices.Equal(out[0].Members, []string{"c1", "c3"}) || out[0].Metrics.DistinctPRs != 2 || out[0].Tier != rules.TierConsider {
		t.Errorf("wrap rule %+v", out[0])
	}
	if st.Assigned != 1 || st.NewRules != 2 || st.Discarded != 1 || st.Dropped != 1 || st.Touched != 3 {
		t.Errorf("stats %+v", st)
	}

	// The fold is cached: running it again makes no calls.
	n := len(f.prompts)
	if _, _, _, err := Fold(context.Background(), opts, existing, newCands, all, now); err != nil || len(f.prompts) != n {
		t.Errorf("re-fold made %d calls (%v)", len(f.prompts)-n, err)
	}
}

func TestScoreOnlySelected(t *testing.T) {
	cands := []rules.Candidate{
		cand("c1", "testing", "A", ev(1, "1.1", "a", "maintainer", "accepted")),
		cand("c2", "testing", "B", ev(2, "2.1", "b", "maintainer", "accepted")),
	}
	drafts := []rules.Rule{
		{Title: "A", Rule: "A.", Category: "testing", Kind: "do", Tier: rules.TierGolden, Confidence: 0.9, TierReason: "kept", Members: []string{"c1"}},
		{Title: "B", Rule: "B.", Category: "testing", Kind: "do", Members: []string{"c2"}},
	}
	AttachEvidence(drafts, cands, now)
	f := &fakeLLM{handle: func(req llm.Request) string {
		if strings.Contains(req.Prompt, "(do) A:") {
			t.Errorf("unselected rule sent to the scorer")
		}
		return mustJSON(map[string]any{"scores": []any{map[string]any{"id": "r1", "tier": "consider", "confidence": 0.4, "tier_reason": "thin", "applies_when": ""}}})
	}}
	opts := Options{Runner: &llm.Runner{Client: f, Dir: t.TempDir()}, Effort: "high"}
	out, err := Score(context.Background(), opts, drafts, []bool{false, true})
	if err != nil {
		t.Fatal(err)
	}
	// A keeps its stored grade but the single-PR floor still applies.
	if out[0].TierReason == "kept" || out[0].Tier != rules.TierConditional && out[0].Tier != rules.TierConsider {
		t.Errorf("A: %+v", out[0])
	}
	if out[1].Tier != rules.TierConsider || out[1].Confidence != 0.4 {
		t.Errorf("B: %+v", out[1])
	}
}
