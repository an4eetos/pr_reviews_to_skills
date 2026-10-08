// Package metrics computes deterministic evidence metrics for a rule and
// enforces tier floors, so a model's enthusiasm can never promote a rule
// beyond what the PR history supports.
package metrics

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/an4eetos/pr_reviews_to_skills/internal/rules"
)

const recentWindow = 365 * 24 * time.Hour

// Compute derives metrics from the evidence supporting a rule and the
// evidence arguing against it.
func Compute(support, opposing []rules.Evidence, now time.Time) rules.Metrics {
	var m rules.Metrics
	prs := map[string]bool{}
	reviewers := map[string]bool{}
	authors := map[string]bool{}
	repos := map[string]bool{}
	recent := 0
	for _, e := range support {
		prs[e.Repo+"#"+strconv.Itoa(e.PR)] = true
		if e.Repo != "" {
			repos[e.Repo] = true
		}
		authors[strings.ToLower(e.Author)] = true
		if e.Role != "pr-author" {
			reviewers[strings.ToLower(e.Author)] = true
		}
		if e.Role == "maintainer" && e.Outcome != "disputed" {
			m.MaintainerEndorsed = true
		}
		switch e.Outcome {
		case "accepted":
			m.Accepted++
		case "disputed":
			m.Disputed++
		case "ignored":
			m.Ignored++
		}
		m.ThumbsUp += e.ThumbsUp
		if !e.Date.IsZero() {
			if m.FirstSeen.IsZero() || e.Date.Before(m.FirstSeen) {
				m.FirstSeen = e.Date
			}
			if e.Date.After(m.LastSeen) {
				m.LastSeen = e.Date
			}
			if now.Sub(e.Date) <= recentWindow {
				recent++
			}
		}
	}
	m.DistinctPRs = len(prs)
	m.DistinctRepos = len(repos)
	m.DistinctReviewers = len(reviewers)
	if m.DistinctReviewers == 0 {
		m.DistinctReviewers = len(authors)
	}
	seen := map[string]bool{}
	for _, e := range opposing {
		key := e.URL
		if key == "" {
			key = e.Repo + "#" + e.Ref
		}
		if !seen[key] {
			seen[key] = true
			m.Opposing++
		}
	}
	if len(support) > 0 {
		m.RecentShare = float64(recent) / float64(len(support))
	}
	return m
}

// Contested is true when pushback is a substantial share of the evidence.
func Contested(m rules.Metrics) bool {
	against := m.Disputed + m.Opposing
	return against > 0 && against*3 >= m.Accepted
}

// ApplyFloors returns the tier the evidence can actually support, plus a
// note explaining any downgrade (empty when unchanged).
func ApplyFloors(tier rules.Tier, appliesWhen string, m rules.Metrics) (rules.Tier, string) {
	orig := tier
	var why []string

	if tier == rules.TierGolden {
		if m.DistinctPRs < 3 {
			why = append(why, fmt.Sprintf("only %d PR(s)", m.DistinctPRs))
		}
		if m.DistinctReviewers < 2 && !m.MaintainerEndorsed {
			why = append(why, "a single non-maintainer reviewer")
		}
		if Contested(m) {
			why = append(why, "contested")
		}
		if len(why) > 0 {
			tier = rules.TierConditional
		}
	}
	if tier == rules.TierConditional && strings.TrimSpace(appliesWhen) == "" {
		if orig == rules.TierConditional {
			why = append(why, "no applies_when condition")
		}
		tier = rules.TierConsider
	}
	if (tier == rules.TierGolden || tier == rules.TierConditional) && m.DistinctPRs <= 1 && !m.MaintainerEndorsed {
		why = append(why, "single PR without maintainer support")
		tier = rules.TierConsider
	}
	if tier == orig {
		return tier, ""
	}
	return tier, fmt.Sprintf("Downgraded from %s by evidence floor: %s.", orig, strings.Join(why, "; "))
}

// RepoFloor applies to combined (multi-repo) rule sets: a rule seen in only
// one repo can't be a golden rule for all of them, so it becomes a rule
// conditional on that repo.
func RepoFloor(tier rules.Tier, appliesWhen string, repos []string) (rules.Tier, string, string) {
	if tier != rules.TierGolden || len(repos) > 1 {
		return tier, appliesWhen, ""
	}
	if strings.TrimSpace(appliesWhen) == "" && len(repos) == 1 {
		appliesWhen = "In " + repos[0]
	}
	return rules.TierConditional, appliesWhen, "Downgraded from golden: seen in a single repository."
}
