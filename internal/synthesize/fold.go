package synthesize

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/an4eetos/pr_reviews_to_skills/internal/digest"
	"github.com/an4eetos/pr_reviews_to_skills/internal/extract"
	"github.com/an4eetos/pr_reviews_to_skills/internal/llm"
	"github.com/an4eetos/pr_reviews_to_skills/internal/rules"
	"github.com/an4eetos/pr_reviews_to_skills/prompts"
)

// minFoldGroupTokens keeps candidate groups useful even when a category
// already has many rules.
const minFoldGroupTokens = 20000

type FoldStats struct {
	NewCandidates int `json:"new_candidates"`
	Requests      int `json:"requests"`
	Assigned      int `json:"assigned"`
	NewRules      int `json:"new_rules"`
	Discarded     int `json:"discarded"`
	Unassigned    int `json:"unassigned"`
	Dropped       int `json:"dropped"`
	Touched       int `json:"touched"`
}

type foldOutput struct {
	Assignments []struct {
		Candidate string `json:"candidate"`
		Rule      string `json:"rule"`
		Relation  string `json:"relation"`
	} `json:"assignments"`
	NewRules  []mergeRule `json:"new_rules"`
	Discarded []string    `json:"discarded"`
}

func foldSchema() json.RawMessage {
	str := map[string]any{"type": "string"}
	assignment := map[string]any{
		"type":                 "object",
		"additionalProperties": false,
		"required":             []string{"candidate", "rule", "relation"},
		"properties": map[string]any{
			"candidate": str,
			"rule":      str,
			"relation":  map[string]any{"type": "string", "enum": []string{"member", "opposing"}},
		},
	}
	s := map[string]any{
		"type":                 "object",
		"additionalProperties": false,
		"required":             []string{"assignments", "new_rules", "discarded"},
		"properties": map[string]any{
			"assignments": map[string]any{"type": "array", "items": assignment},
			"new_rules":   map[string]any{"type": "array", "items": mergeRuleSchema()},
			"discarded":   map[string]any{"type": "array", "items": str},
		},
	}
	data, _ := json.Marshal(s)
	return data
}

// Fold adds new candidates to an existing rule set without rewriting the
// existing rules, so generated files stay stable between scheduled runs. Per
// category, the model assigns each new candidate to an existing rule (as
// support or opposition), groups the rest into new rules, or discards them.
//
// Evidence and metrics are then recomputed for every rule from all
// candidates, and touched[i] reports whether rule i is new or its evidence
// changed, i.e. whether it needs scoring. Rules left without evidence (every
// PR behind them was re-extracted without the rule reappearing) are dropped.
func Fold(ctx context.Context, opts Options, existing []rules.Rule, newCands, all []rules.Candidate, now time.Time) ([]rules.Rule, []bool, FoldStats, error) {
	st := FoldStats{NewCandidates: len(newCands)}
	byID := map[string]rules.Candidate{}
	for _, c := range all {
		byID[c.ID] = c
	}
	out := make([]rules.Rule, len(existing))
	before := make([]string, len(existing))
	for i, r := range existing {
		r.Members = slices.Clone(r.Members)
		r.Opposing = slices.Clone(r.Opposing)
		out[i] = r
		before[i] = signature(r)
	}
	assigned := make([]bool, len(existing))

	byCat := map[string][]item{}
	for _, c := range newCands {
		byCat[c.Category] = append(byCat[c.Category], candidateItem(c))
	}
	var added []rules.Rule
	for _, cat := range rules.Categories {
		items := byCat[cat]
		if len(items) == 0 {
			continue
		}
		var idxs []int
		for i, r := range out {
			if r.Category == cat {
				idxs = append(idxs, i)
			}
		}
		var created []item
		if len(idxs) == 0 {
			// Nothing to fold into: a plain merge of the new candidates.
			var ms MergeStats
			merged, err := mergeCategory(ctx, opts, cat, items, byID, &ms)
			if err != nil {
				return nil, nil, st, err
			}
			st.Requests += ms.Requests
			st.Discarded += ms.Discarded
			st.Unassigned += ms.Unassigned
			created = merged
		} else {
			var err error
			created, err = foldCategory(ctx, opts, cat, out, idxs, items, byID, assigned, &st)
			if err != nil {
				return nil, nil, st, err
			}
		}
		st.NewRules += len(created)
		for _, it := range created {
			added = append(added, it.rule(cat))
		}
		opts.logf("synthesize: %s: %d new candidates folded into %d existing rules, %d new rules", cat, len(items), len(idxs), len(created))
	}
	out = append(out, added...)

	AttachEvidence(out, all, now)
	var kept []rules.Rule
	var touched []bool
	for i, r := range out {
		if len(r.Evidence) == 0 {
			st.Dropped++
			continue
		}
		r.Members = known(r.Members, byID)
		r.Opposing = known(r.Opposing, byID)
		t := i >= len(existing) || assigned[i] || signature(r) != before[i]
		if t {
			st.Touched++
		}
		kept = append(kept, r)
		touched = append(touched, t)
	}
	return kept, touched, st, nil
}

func foldCategory(ctx context.Context, opts Options, cat string, out []rules.Rule, idxs []int, items []item,
	byID map[string]rules.Candidate, assigned []bool, st *FoldStats) ([]item, error) {
	var rb strings.Builder
	for n, i := range idxs {
		rb.WriteString(renderExisting(n+1, out[i]))
	}
	existingText := rb.String()
	groupTokens := opts.GroupTokens
	if groupTokens <= 0 {
		groupTokens = 120000
	}
	groups := groupItems(items, byID, max(groupTokens-digest.EstimateTokens(existingText), minFoldGroupTokens))

	schema := foldSchema()
	reqs := make([]llm.Request, len(groups))
	for g, grp := range groups {
		prompt := fmt.Sprintf("Category: %s\n\n<rules>\n%s</rules>\n\n<candidates>\n%s</candidates>\n\nFold the new candidates into the existing rules.",
			cat, existingText, renderItems(grp, byID))
		reqs[g] = llm.Request{
			ID:        extract.RequestID(fmt.Sprintf("fold-%s-G%d", cat, g+1), prompt),
			System:    prompts.Fold,
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
		return nil, fmt.Errorf("fold %s: %d request(s) failed (first: %s: %s); re-run synthesize to retry", cat, len(fails), fails[0].ID, fails[0].Error)
	}

	var created []item
	for g, grp := range groups {
		var fo foldOutput
		if err := llm.Decode(results[reqs[g].ID], &fo); err != nil {
			return nil, err
		}
		used := make([]bool, len(grp))
		for _, a := range fo.Assignments {
			n, ok := localIndex("i", a.Candidate, len(grp))
			if !ok || used[n] {
				continue
			}
			r, ok := localIndex("R", a.Rule, len(idxs))
			if !ok {
				continue
			}
			rule := &out[idxs[r]]
			if a.Relation == "opposing" {
				rule.Opposing = dedupe(append(rule.Opposing, grp[n].Members...))
			} else {
				rule.Members = dedupe(append(rule.Members, grp[n].Members...))
			}
			used[n] = true
			assigned[idxs[r]] = true
			st.Assigned++
		}
		var ms MergeStats
		created = append(created, applyMerge(grp, mergeOutput{Rules: fo.NewRules, Discarded: fo.Discarded}, &ms, used)...)
		st.Discarded += ms.Discarded
		st.Unassigned += ms.Unassigned
	}
	return created, nil
}

// localIndex parses a short prompt id like "i3" or "[R12]" into a 0-based
// index below n.
func localIndex(prefix, id string, n int) (int, bool) {
	id = strings.Trim(strings.TrimSpace(id), "[]")
	var k int
	if _, err := fmt.Sscanf(id, prefix+"%d", &k); err != nil || k < 1 || k > n {
		return 0, false
	}
	return k - 1, true
}

func renderExisting(n int, r rules.Rule) string {
	var b strings.Builder
	fmt.Fprintf(&b, "[R%d] (%s) %s: %s", n, r.Kind, oneLine(r.Title), oneLine(r.Rule))
	if r.AppliesWhen != "" {
		fmt.Fprintf(&b, " | when: %s", oneLine(r.AppliesWhen))
	}
	if len(r.Scope.Paths) > 0 {
		fmt.Fprintf(&b, " | paths: %s", strings.Join(r.Scope.Paths, ", "))
	}
	if len(r.Scope.Languages) > 0 {
		fmt.Fprintf(&b, " | langs: %s", strings.Join(r.Scope.Languages, ", "))
	}
	fmt.Fprintf(&b, " | prs: %d\n", r.Metrics.DistinctPRs)
	return b.String()
}

// signature captures what scoring depends on: which comments back a rule,
// their outcomes and roles, and how much opposition it has.
func signature(r rules.Rule) string {
	keys := make([]string, len(r.Evidence))
	for i, e := range r.Evidence {
		keys[i] = e.URL + "|" + e.Outcome + "|" + e.Role
	}
	sort.Strings(keys)
	return fmt.Sprintf("%s#opp=%d", strings.Join(keys, ","), r.Metrics.Opposing)
}

func known(ids []string, byID map[string]rules.Candidate) []string {
	var out []string
	for _, id := range ids {
		if _, ok := byID[id]; ok {
			out = append(out, id)
		}
	}
	return out
}
