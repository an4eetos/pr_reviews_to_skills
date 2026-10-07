package synthesize

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"slices"
	"strings"
	"time"

	"github.com/an4eetos/pr_reviews_to_skills/internal/extract"
	"github.com/an4eetos/pr_reviews_to_skills/internal/llm"
	"github.com/an4eetos/pr_reviews_to_skills/internal/metrics"
	"github.com/an4eetos/pr_reviews_to_skills/internal/rules"
	"github.com/an4eetos/pr_reviews_to_skills/prompts"
)

const (
	scoreMaxTokens = 64000
	scoreGroupSize = 150
)

func scoreSchema() json.RawMessage {
	tiers := make([]string, len(rules.Tiers))
	for i, t := range rules.Tiers {
		tiers[i] = string(t)
	}
	score := map[string]any{
		"type":                 "object",
		"additionalProperties": false,
		"required":             []string{"id", "tier", "confidence", "tier_reason", "applies_when"},
		"properties": map[string]any{
			"id":           map[string]any{"type": "string"},
			"tier":         map[string]any{"type": "string", "enum": tiers},
			"confidence":   map[string]any{"type": "number"},
			"tier_reason":  map[string]any{"type": "string"},
			"applies_when": map[string]any{"type": "string"},
		},
	}
	s := map[string]any{
		"type":                 "object",
		"additionalProperties": false,
		"required":             []string{"scores"},
		"properties": map[string]any{
			"scores": map[string]any{"type": "array", "items": score},
		},
	}
	data, _ := json.Marshal(s)
	return data
}

type scoreOutput struct {
	Scores []struct {
		ID          string  `json:"id"`
		Tier        string  `json:"tier"`
		Confidence  float64 `json:"confidence"`
		TierReason  string  `json:"tier_reason"`
		AppliesWhen string  `json:"applies_when"`
	} `json:"scores"`
}

// AttachEvidence resolves each draft's member/opposing candidate IDs into
// evidence and computes its metrics.
func AttachEvidence(drafts []rules.Rule, cands []rules.Candidate, now time.Time) {
	byID := map[string]rules.Candidate{}
	for _, c := range cands {
		byID[c.ID] = c
	}
	for i := range drafts {
		d := &drafts[i]
		d.Evidence = collect(d.Members, byID)
		d.Metrics = metrics.Compute(d.Evidence, collect(d.Opposing, byID), now)
	}
}

func collect(ids []string, byID map[string]rules.Candidate) []rules.Evidence {
	seen := map[string]bool{}
	var out []rules.Evidence
	for _, id := range ids {
		for _, e := range byID[id].Evidence {
			if !seen[e.Ref] {
				seen[e.Ref] = true
				out = append(out, e)
			}
		}
	}
	return out
}

// Score grades drafts (which must already have evidence attached) per
// category, then applies the evidence floors.
func Score(ctx context.Context, opts Options, drafts []rules.Rule) ([]rules.Rule, error) {
	schema := scoreSchema()
	var reqs []llm.Request
	reqRules := map[string][]int{} // request ID -> draft indexes, in prompt order
	for _, cat := range rules.Categories {
		var idxs []int
		for i, d := range drafts {
			if d.Category == cat {
				idxs = append(idxs, i)
			}
		}
		for g := 0; g*scoreGroupSize < len(idxs); g++ {
			part := idxs[g*scoreGroupSize : min((g+1)*scoreGroupSize, len(idxs))]
			var b strings.Builder
			for n, i := range part {
				b.WriteString(renderScored(n+1, drafts[i]))
			}
			prompt := fmt.Sprintf("Category: %s\n\n<rules>\n%s</rules>\n\nGrade every rule.", cat, b.String())
			id := extract.RequestID(fmt.Sprintf("score-%s-G%d", cat, g+1), prompt)
			reqs = append(reqs, llm.Request{
				ID: id, System: prompts.Score, Prompt: prompt, Schema: schema,
				Effort: opts.Effort, MaxTokens: scoreMaxTokens,
			})
			reqRules[id] = part
		}
	}
	results, err := opts.Runner.RunSync(ctx, reqs)
	if err != nil {
		return nil, err
	}
	if fails := llm.Failures(results); len(fails) > 0 {
		return nil, fmt.Errorf("score: %d request(s) failed (first: %s: %s); re-run synthesize to retry", len(fails), fails[0].ID, fails[0].Error)
	}

	out := slices.Clone(drafts)
	graded := make([]bool, len(out))
	for _, q := range reqs {
		var so scoreOutput
		if err := llm.Decode(results[q.ID], &so); err != nil {
			return nil, err
		}
		part := reqRules[q.ID]
		for _, s := range so.Scores {
			var n int
			if _, err := fmt.Sscanf(strings.Trim(s.ID, "[] "), "r%d", &n); err != nil || n < 1 || n > len(part) {
				continue
			}
			r := &out[part[n-1]]
			r.Tier = rules.Tier(s.Tier)
			r.Confidence = clamp01(s.Confidence)
			r.TierReason = strings.TrimSpace(s.TierReason)
			if aw := strings.TrimSpace(s.AppliesWhen); aw != "" {
				r.AppliesWhen = aw
			}
			graded[part[n-1]] = true
		}
	}
	for i := range out {
		r := &out[i]
		if !graded[i] || !slices.Contains(rules.Tiers, r.Tier) {
			r.Tier = rules.TierConsider
			r.Confidence = 0.3
			r.TierReason = "The scorer returned no grade for this rule."
		}
		tier, note := metrics.ApplyFloors(r.Tier, r.AppliesWhen, r.Metrics)
		if note != "" {
			r.Tier = tier
			r.Confidence = math.Min(r.Confidence, 0.7)
			r.TierReason = strings.TrimSpace(r.TierReason + " " + note)
		}
	}
	return out, nil
}

func renderScored(n int, r rules.Rule) string {
	m := r.Metrics
	var b strings.Builder
	fmt.Fprintf(&b, "[r%d] (%s) %s: %s\n", n, r.Kind, oneLine(r.Title), oneLine(r.Rule))
	if r.AppliesWhen != "" {
		fmt.Fprintf(&b, "    when: %s\n", oneLine(r.AppliesWhen))
	}
	if len(r.Scope.Paths) > 0 {
		fmt.Fprintf(&b, "    paths: %s\n", strings.Join(r.Scope.Paths, ", "))
	}
	maint := "no"
	if m.MaintainerEndorsed {
		maint = "yes"
	}
	fmt.Fprintf(&b, "    prs=%d reviewers=%d maintainer=%s accepted=%d disputed=%d ignored=%d opposing=%d first=%s last=%s recent=%.2f\n",
		m.DistinctPRs, m.DistinctReviewers, maint, m.Accepted, m.Disputed, m.Ignored, m.Opposing,
		month(m.FirstSeen), month(m.LastSeen), m.RecentShare)
	return b.String()
}

func month(t time.Time) string {
	if t.IsZero() {
		return "?"
	}
	return t.Format("2006-01")
}

func clamp01(x float64) float64 {
	if math.IsNaN(x) {
		return 0
	}
	return math.Max(0, math.Min(1, x))
}
