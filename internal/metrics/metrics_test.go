package metrics

import (
	"strings"
	"testing"
	"time"

	"github.com/an4eetos/pr_reviews_to_skills/internal/rules"
)

var now = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

func ev(pr int, author, role, outcome string, age time.Duration) rules.Evidence {
	return rules.Evidence{PR: pr, Ref: author + string(rune('0'+pr)), Author: author, Role: role, Outcome: outcome, Date: now.Add(-age)}
}

func TestCompute(t *testing.T) {
	day := 24 * time.Hour
	support := []rules.Evidence{
		ev(1, "alice", "maintainer", "accepted", 10*day),
		ev(2, "Alice", "maintainer", "accepted", 400*day),
		ev(3, "bob", "contributor", "disputed", 20*day),
		ev(3, "carl", "pr-author", "ignored", 20*day),
	}
	opposing := []rules.Evidence{ev(9, "dan", "contributor", "accepted", day), ev(9, "dan", "contributor", "accepted", day)}
	m := Compute(support, opposing, now)
	if m.DistinctPRs != 3 {
		t.Errorf("DistinctPRs %d", m.DistinctPRs)
	}
	if m.DistinctReviewers != 2 {
		t.Errorf("DistinctReviewers %d, want 2 (case-insensitive, pr-author excluded)", m.DistinctReviewers)
	}
	if !m.MaintainerEndorsed || m.Accepted != 2 || m.Disputed != 1 || m.Ignored != 1 || m.Opposing != 1 {
		t.Errorf("metrics %+v", m)
	}
	if m.RecentShare != 0.75 {
		t.Errorf("RecentShare %v", m.RecentShare)
	}
	if !m.FirstSeen.Equal(now.Add(-400*day)) || !m.LastSeen.Equal(now.Add(-10*day)) {
		t.Errorf("first/last %v %v", m.FirstSeen, m.LastSeen)
	}
}

func TestApplyFloors(t *testing.T) {
	strong := rules.Metrics{DistinctPRs: 5, DistinctReviewers: 3, MaintainerEndorsed: true, Accepted: 6}
	cases := []struct {
		name    string
		tier    rules.Tier
		when    string
		m       rules.Metrics
		want    rules.Tier
		changed bool
	}{
		{"strong golden stays", rules.TierGolden, "", strong, rules.TierGolden, false},
		{"golden with few PRs and condition", rules.TierGolden, "in handlers", rules.Metrics{DistinctPRs: 2, DistinctReviewers: 2, Accepted: 2}, rules.TierConditional, true},
		{"golden with few PRs no condition", rules.TierGolden, "", rules.Metrics{DistinctPRs: 2, DistinctReviewers: 2, Accepted: 2}, rules.TierConsider, true},
		{"contested golden", rules.TierGolden, "", rules.Metrics{DistinctPRs: 4, DistinctReviewers: 3, MaintainerEndorsed: true, Accepted: 3, Disputed: 1}, rules.TierConsider, true},
		{"minor dispute ok", rules.TierGolden, "", rules.Metrics{DistinctPRs: 6, DistinctReviewers: 3, MaintainerEndorsed: true, Accepted: 6, Disputed: 1}, rules.TierGolden, false},
		{"conditional without condition", rules.TierConditional, " ", strong, rules.TierConsider, true},
		{"single PR non-maintainer", rules.TierConditional, "x", rules.Metrics{DistinctPRs: 1, DistinctReviewers: 1}, rules.TierConsider, true},
		{"single PR maintainer conditional ok", rules.TierConditional, "x", rules.Metrics{DistinctPRs: 1, DistinctReviewers: 1, MaintainerEndorsed: true}, rules.TierConditional, false},
		{"consider untouched", rules.TierConsider, "", rules.Metrics{}, rules.TierConsider, false},
		{"rejected untouched", rules.TierRejected, "", strong, rules.TierRejected, false},
	}
	for _, c := range cases {
		got, note := ApplyFloors(c.tier, c.when, c.m)
		if got != c.want || (note != "") != c.changed {
			t.Errorf("%s: got %s %q, want %s changed=%v", c.name, got, note, c.want, c.changed)
		}
		if c.changed && !strings.Contains(note, "Downgraded from "+string(c.tier)) {
			t.Errorf("%s: note %q", c.name, note)
		}
	}
}

func TestRepoFloor(t *testing.T) {
	tier, when, note := RepoFloor(rules.TierGolden, "", []string{"o/a"})
	if tier != rules.TierConditional || when != "In o/a" || note == "" {
		t.Errorf("single repo: %s %q %q", tier, when, note)
	}
	if tier, when, note := RepoFloor(rules.TierGolden, "", []string{"o/a", "o/b"}); tier != rules.TierGolden || when != "" || note != "" {
		t.Errorf("two repos: %s %q %q", tier, when, note)
	}
	if tier, _, note := RepoFloor(rules.TierConsider, "", []string{"o/a"}); tier != rules.TierConsider || note != "" {
		t.Errorf("consider is untouched: %s %q", tier, note)
	}
}
