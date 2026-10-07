// Package output assembles, validates and writes rules.json.
package output

import (
	"errors"
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/an4eetos/pr_reviews_to_skills/internal/rules"
	"github.com/an4eetos/pr_reviews_to_skills/internal/store"
)

const (
	SchemaVersion = 1
	maxEvidence   = 5
	maxSlugRunes  = 60
)

type Report struct {
	SchemaVersion int          `json:"schema_version"`
	Repo          string       `json:"repo"`
	GeneratedAt   time.Time    `json:"generated_at"`
	Model         string       `json:"model"`
	Stats         Stats        `json:"stats"`
	Rules         []rules.Rule `json:"rules"`
}

type Stats struct {
	PRsScanned        int            `json:"prs_scanned"`
	PRsWithDiscussion int            `json:"prs_with_discussion"`
	Threads           int            `json:"threads"`
	Comments          int            `json:"comments"`
	Chunks            int            `json:"chunks"`
	Candidates        int            `json:"candidates"`
	Rules             int            `json:"rules"`
	ByTier            map[string]int `json:"by_tier"`
}

// Finalize sorts rules by tier then confidence, drops rejected ones unless
// keepRejected, assigns stable slug IDs, keeps the strongest evidence, and
// strips intermediate fields.
func Finalize(in []rules.Rule, keepRejected bool) []rules.Rule {
	var out []rules.Rule
	for _, r := range in {
		if r.Tier == rules.TierRejected && !keepRejected {
			continue
		}
		r.Members, r.Opposing = nil, nil
		r.Evidence = topEvidence(r.Evidence, maxEvidence)
		if r.Scope.Paths == nil {
			r.Scope.Paths = []string{}
		}
		if r.Scope.Languages == nil {
			r.Scope.Languages = []string{}
		}
		out = append(out, r)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if a, b := out[i].Tier.Rank(), out[j].Tier.Rank(); a != b {
			return a < b
		}
		if out[i].Confidence != out[j].Confidence {
			return out[i].Confidence > out[j].Confidence
		}
		return out[i].Metrics.DistinctPRs > out[j].Metrics.DistinctPRs
	})
	used := map[string]int{}
	for i := range out {
		base := Slug(out[i].Title)
		if base == "" {
			base = "rule"
		}
		used[base]++
		id := base
		if n := used[base]; n > 1 {
			id = fmt.Sprintf("%s-%d", base, n)
		}
		out[i].ID = id
	}
	return out
}

func topEvidence(ev []rules.Evidence, n int) []rules.Evidence {
	ev = slices.Clone(ev)
	score := func(e rules.Evidence) int {
		s := 0
		if e.Role == "maintainer" {
			s += 4
		}
		switch e.Outcome {
		case "accepted":
			s += 3
		case "unclear":
			s += 1
		}
		return s + min(e.ThumbsUp, 3)
	}
	sort.SliceStable(ev, func(i, j int) bool {
		if a, b := score(ev[i]), score(ev[j]); a != b {
			return a > b
		}
		return ev[i].Date.After(ev[j].Date)
	})
	if len(ev) > n {
		ev = ev[:n]
	}
	if ev == nil {
		ev = []rules.Evidence{}
	}
	return ev
}

var nonSlugRe = regexp.MustCompile(`[^a-z0-9]+`)

func Slug(s string) string {
	s = strings.Trim(nonSlugRe.ReplaceAllString(strings.ToLower(s), "-"), "-")
	if len(s) > maxSlugRunes {
		s = strings.TrimRight(s[:maxSlugRunes], "-")
	}
	return s
}

func Tally(rs []rules.Rule) map[string]int {
	m := map[string]int{}
	for _, t := range rules.Tiers {
		m[string(t)] = 0
	}
	for _, r := range rs {
		m[string(r.Tier)]++
	}
	return m
}

// Validate enforces the invariants schema/rules.schema.json documents.
func Validate(r Report) error {
	var errs []error
	if r.SchemaVersion != SchemaVersion {
		errs = append(errs, fmt.Errorf("schema_version %d, want %d", r.SchemaVersion, SchemaVersion))
	}
	if r.Repo == "" {
		errs = append(errs, errors.New("repo is empty"))
	}
	ids := map[string]bool{}
	for i, x := range r.Rules {
		at := fmt.Sprintf("rules[%d] (%s)", i, x.ID)
		if x.ID == "" || ids[x.ID] {
			errs = append(errs, fmt.Errorf("%s: missing or duplicate id", at))
		}
		ids[x.ID] = true
		if strings.TrimSpace(x.Rule) == "" || strings.TrimSpace(x.Title) == "" {
			errs = append(errs, fmt.Errorf("%s: empty title or rule", at))
		}
		if !slices.Contains(rules.Categories, x.Category) {
			errs = append(errs, fmt.Errorf("%s: bad category %q", at, x.Category))
		}
		if !slices.Contains(rules.Kinds, x.Kind) {
			errs = append(errs, fmt.Errorf("%s: bad kind %q", at, x.Kind))
		}
		if !slices.Contains(rules.Tiers, x.Tier) {
			errs = append(errs, fmt.Errorf("%s: bad tier %q", at, x.Tier))
		}
		if x.Confidence < 0 || x.Confidence > 1 {
			errs = append(errs, fmt.Errorf("%s: confidence %v out of [0,1]", at, x.Confidence))
		}
		if x.Tier == rules.TierConditional && strings.TrimSpace(x.AppliesWhen) == "" {
			errs = append(errs, fmt.Errorf("%s: conditional rule without applies_when", at))
		}
		if len(x.Evidence) == 0 {
			errs = append(errs, fmt.Errorf("%s: no evidence", at))
		}
		for _, e := range x.Evidence {
			if e.URL == "" || e.PR <= 0 {
				errs = append(errs, fmt.Errorf("%s: evidence %q missing url or pr", at, e.Ref))
			}
		}
	}
	return errors.Join(errs...)
}

func Write(path string, r Report) error {
	if err := Validate(r); err != nil {
		return fmt.Errorf("rules.json failed validation: %w", err)
	}
	return store.WriteJSON(path, r)
}
