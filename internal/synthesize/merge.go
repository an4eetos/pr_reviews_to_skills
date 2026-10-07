// Package synthesize is the reduce stage: per-category merging of candidate
// rules (hierarchically when a category is too large for one call), then
// evidence-aware scoring into tiers.
package synthesize

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/an4eetos/pr_reviews_to_skills/internal/digest"
	"github.com/an4eetos/pr_reviews_to_skills/internal/extract"
	"github.com/an4eetos/pr_reviews_to_skills/internal/llm"
	"github.com/an4eetos/pr_reviews_to_skills/internal/rules"
	"github.com/an4eetos/pr_reviews_to_skills/prompts"
)

const (
	mergeMaxTokens = 100000
	maxMergeLevels = 5
)

type Options struct {
	Runner *llm.Runner
	Effort string
	// GroupTokens bounds one merge request's candidate list.
	GroupTokens int
	Logf        func(format string, args ...any)
}

func (o Options) logf(format string, args ...any) {
	if o.Logf != nil {
		o.Logf(format, args...)
	}
}

// item is a rule being merged: initially one candidate, later a merged rule.
// Members/Opposing always hold original candidate IDs.
type item struct {
	Kind, Title, Statement, Rationale, AppliesWhen, Bad, Good string
	Paths, Languages                                          []string
	Members, Opposing                                         []string
}

func mergeSchema() json.RawMessage {
	str := map[string]any{"type": "string"}
	strList := map[string]any{"type": "array", "items": str}
	rule := map[string]any{
		"type":                 "object",
		"additionalProperties": false,
		"required": []string{"title", "rule", "rationale", "kind", "applies_when", "paths", "languages",
			"bad_example", "good_example", "members", "opposing"},
		"properties": map[string]any{
			"title":        str,
			"rule":         str,
			"rationale":    str,
			"kind":         map[string]any{"type": "string", "enum": rules.Kinds},
			"applies_when": str,
			"paths":        strList,
			"languages":    strList,
			"bad_example":  str,
			"good_example": str,
			"members":      strList,
			"opposing":     strList,
		},
	}
	s := map[string]any{
		"type":                 "object",
		"additionalProperties": false,
		"required":             []string{"rules", "discarded"},
		"properties": map[string]any{
			"rules":     map[string]any{"type": "array", "items": rule},
			"discarded": strList,
		},
	}
	data, _ := json.Marshal(s)
	return data
}

type mergeOutput struct {
	Rules []struct {
		Title       string   `json:"title"`
		Rule        string   `json:"rule"`
		Rationale   string   `json:"rationale"`
		Kind        string   `json:"kind"`
		AppliesWhen string   `json:"applies_when"`
		Paths       []string `json:"paths"`
		Languages   []string `json:"languages"`
		BadExample  string   `json:"bad_example"`
		GoodExample string   `json:"good_example"`
		Members     []string `json:"members"`
		Opposing    []string `json:"opposing"`
	} `json:"rules"`
	Discarded []string `json:"discarded"`
}

type MergeStats struct {
	Categories int `json:"categories"`
	Requests   int `json:"requests"`
	Levels     int `json:"max_levels"`
	Drafts     int `json:"drafts"`
	Discarded  int `json:"discarded"`
	Unassigned int `json:"unassigned"`
}

// Merge consolidates candidates into draft rules (no tier yet), category by
// category. Failed requests abort the stage; completed ones are cached by the
// runner so a re-run only redoes what failed.
func Merge(ctx context.Context, opts Options, cands []rules.Candidate) ([]rules.Rule, MergeStats, error) {
	byID := map[string]rules.Candidate{}
	byCat := map[string][]item{}
	for _, c := range cands {
		byID[c.ID] = c
		byCat[c.Category] = append(byCat[c.Category], item{
			Kind: c.Kind, Title: c.Title, Statement: c.Statement, Rationale: c.Rationale,
			AppliesWhen: c.AppliesWhen, Bad: c.BadExample, Good: c.GoodExample,
			Paths: c.Paths, Languages: c.Languages, Members: []string{c.ID},
		})
	}
	var st MergeStats
	var drafts []rules.Rule
	for _, cat := range rules.Categories {
		items := byCat[cat]
		if len(items) == 0 {
			continue
		}
		st.Categories++
		merged, err := mergeCategory(ctx, opts, cat, items, byID, &st)
		if err != nil {
			return nil, st, err
		}
		for _, it := range merged {
			drafts = append(drafts, rules.Rule{
				Title: it.Title, Rule: it.Statement, Rationale: it.Rationale,
				Category: cat, Kind: it.Kind, AppliesWhen: it.AppliesWhen,
				Scope:    rules.Scope{Paths: orEmpty(it.Paths), Languages: orEmpty(it.Languages)},
				Examples: rules.Examples{Bad: it.Bad, Good: it.Good},
				Members:  it.Members, Opposing: it.Opposing,
			})
		}
		opts.logf("synthesize: %s: %d candidates -> %d rules", cat, len(items), len(merged))
	}
	st.Drafts = len(drafts)
	return drafts, st, nil
}

func mergeCategory(ctx context.Context, opts Options, cat string, items []item, byID map[string]rules.Candidate, st *MergeStats) ([]item, error) {
	if len(items) == 1 {
		return items, nil
	}
	groupTokens := opts.GroupTokens
	if groupTokens <= 0 {
		groupTokens = 120000
	}
	schema := mergeSchema()
	for level := 1; level <= maxMergeLevels; level++ {
		st.Levels = max(st.Levels, level)
		groups := groupItems(items, byID, groupTokens)
		reqs := make([]llm.Request, len(groups))
		for g, grp := range groups {
			prompt := fmt.Sprintf("Category: %s\n\n<candidates>\n%s</candidates>\n\nConsolidate these candidates.", cat, renderItems(grp, byID))
			reqs[g] = llm.Request{
				ID:        extract.RequestID(fmt.Sprintf("merge-%s-L%d-G%d", cat, level, g+1), prompt),
				System:    prompts.Merge,
				Prompt:    prompt,
				Schema:    schema,
				Effort:    opts.Effort,
				MaxTokens: mergeMaxTokens,
			}
		}
		st.Requests += len(reqs)
		results, err := opts.Runner.RunSync(ctx, reqs)
		if err != nil {
			return nil, err
		}
		if fails := llm.Failures(results); len(fails) > 0 {
			return nil, fmt.Errorf("merge %s: %d request(s) failed (first: %s: %s); re-run synthesize to retry", cat, len(fails), fails[0].ID, fails[0].Error)
		}
		var next []item
		for g, grp := range groups {
			var mo mergeOutput
			if err := llm.Decode(results[reqs[g].ID], &mo); err != nil {
				return nil, err
			}
			next = append(next, applyMerge(grp, mo, st)...)
		}
		if len(groups) == 1 {
			return next, nil
		}
		// Stop when another level would not consolidate meaningfully: the
		// remaining items are distinct rules that just don't fit one call.
		if float64(len(next)) >= 0.85*float64(len(items)) {
			opts.logf("synthesize: %s: stopping at level %d with %d rules across %d groups", cat, level, len(next), len(groups))
			return next, nil
		}
		items = next
	}
	return items, nil
}

// applyMerge maps the model's short local ids back to candidate IDs.
func applyMerge(grp []item, mo mergeOutput, st *MergeStats) []item {
	local := func(id string) (int, bool) {
		id = strings.Trim(strings.TrimSpace(id), "[]")
		var n int
		if _, err := fmt.Sscanf(id, "i%d", &n); err != nil || n < 1 || n > len(grp) {
			return 0, false
		}
		return n - 1, true
	}
	used := make([]bool, len(grp))
	var out []item
	for _, r := range mo.Rules {
		it := item{
			Kind: r.Kind, Title: r.Title, Statement: r.Rule, Rationale: r.Rationale,
			AppliesWhen: strings.TrimSpace(r.AppliesWhen), Bad: r.BadExample, Good: r.GoodExample,
			Paths: r.Paths, Languages: r.Languages,
		}
		members := map[int]bool{}
		for _, id := range r.Members {
			if n, ok := local(id); ok && !used[n] {
				members[n] = true
			}
		}
		if len(members) == 0 {
			continue // a rule with no surviving members has no evidence
		}
		for n := range members {
			used[n] = true
		}
		for _, n := range sortedKeys(members) {
			it.Members = append(it.Members, grp[n].Members...)
			it.Opposing = append(it.Opposing, grp[n].Opposing...)
		}
		for _, id := range r.Opposing {
			if n, ok := local(id); ok && !members[n] {
				it.Opposing = append(it.Opposing, grp[n].Members...)
				used[n] = true
			}
		}
		it.Members = dedupe(it.Members)
		it.Opposing = dedupe(it.Opposing)
		out = append(out, it)
	}
	for _, id := range mo.Discarded {
		if n, ok := local(id); ok && !used[n] {
			used[n] = true
			st.Discarded++
		}
	}
	// Items the model forgot are kept as-is rather than silently dropped.
	for n, u := range used {
		if !u {
			st.Unassigned++
			out = append(out, grp[n])
		}
	}
	return out
}

func sortedKeys(m map[int]bool) []int {
	keys := make([]int, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Ints(keys)
	return keys
}

func groupItems(items []item, byID map[string]rules.Candidate, groupTokens int) [][]item {
	var groups [][]item
	var cur []item
	tok := 0
	for _, it := range items {
		t := digest.EstimateTokens(renderItem(0, it, byID))
		if len(cur) > 0 && tok+t > groupTokens {
			groups = append(groups, cur)
			cur, tok = nil, 0
		}
		cur = append(cur, it)
		tok += t
	}
	if len(cur) > 0 {
		groups = append(groups, cur)
	}
	return groups
}

func renderItems(grp []item, byID map[string]rules.Candidate) string {
	var b strings.Builder
	for i, it := range grp {
		b.WriteString(renderItem(i+1, it, byID))
	}
	return b.String()
}

func renderItem(n int, it item, byID map[string]rules.Candidate) string {
	var b strings.Builder
	fmt.Fprintf(&b, "[i%d] (%s) %s: %s", n, it.Kind, oneLine(it.Title), oneLine(it.Statement))
	if it.AppliesWhen != "" {
		fmt.Fprintf(&b, " | when: %s", oneLine(it.AppliesWhen))
	}
	if len(it.Paths) > 0 {
		fmt.Fprintf(&b, " | paths: %s", strings.Join(it.Paths, ", "))
	}
	if len(it.Languages) > 0 {
		fmt.Fprintf(&b, " | langs: %s", strings.Join(it.Languages, ", "))
	}
	fmt.Fprintf(&b, " | prs: %d\n", distinctPRs(it.Members, byID))
	if it.Rationale != "" {
		fmt.Fprintf(&b, "    why: %s\n", clip(it.Rationale, 240))
	}
	if it.Bad != "" {
		fmt.Fprintf(&b, "    bad: %s\n", clip(it.Bad, 160))
	}
	if it.Good != "" {
		fmt.Fprintf(&b, "    good: %s\n", clip(it.Good, 160))
	}
	return b.String()
}

func distinctPRs(ids []string, byID map[string]rules.Candidate) int {
	prs := map[int]bool{}
	for _, id := range ids {
		for _, e := range byID[id].Evidence {
			prs[e.PR] = true
		}
	}
	return len(prs)
}

func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }

func clip(s string, n int) string {
	s = oneLine(s)
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

func dedupe(xs []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, x := range xs {
		if !seen[x] {
			seen[x] = true
			out = append(out, x)
		}
	}
	return out
}

func orEmpty(xs []string) []string {
	if xs == nil {
		return []string{}
	}
	return xs
}
