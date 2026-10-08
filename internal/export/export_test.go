package export

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/an4eetos/pr_reviews_to_skills/internal/output"
	"github.com/an4eetos/pr_reviews_to_skills/internal/rules"
)

var day = time.Date(2025, 9, 12, 0, 0, 0, 0, time.UTC)

func rule(id, title, cat string, tier rules.Tier, paths ...string) rules.Rule {
	return rules.Rule{
		ID: id, Title: title, Rule: title + " always.", Rationale: "Because reviewers said so.",
		Category: cat, Kind: "do", Tier: tier, Confidence: 0.8, TierReason: "Raised often.",
		Scope:    rules.Scope{Paths: paths, Languages: []string{"go"}},
		Examples: rules.Examples{Bad: "return err", Good: "return fmt.Errorf(\"load: %w\", err)"},
		Metrics:  rules.Metrics{DistinctPRs: 4, DistinctReviewers: 2, MaintainerEndorsed: true, Accepted: 3, FirstSeen: day.AddDate(-1, 0, 0), LastSeen: day},
		Evidence: []rules.Evidence{{PR: 812, Ref: "812.3", URL: "https://github.com/o/r/pull/812#discussion_r1", Author: "alice",
			Role: "maintainer", Date: day, Quote: "Please wrap this.\nWe lose the call site.", Outcome: "accepted"}},
	}
}

func sampleReport() output.Report {
	rs := []rules.Rule{
		rule("wrap-errors", "Wrap errors", "error-handling", rules.TierGolden),
		rule("close-rows", "Close rows", "correctness", rules.TierGolden, "internal/db/**"),
		rule("tx-in-repo", "Use transactions", "architecture", rules.TierConditional, "internal/db/**"),
		rule("table-tests", "Use table tests", "testing", rules.TierConsider),
		rule("old-idea", "Old idea", "style", rules.TierRejected),
	}
	rs[2].AppliesWhen = "a repository method writes more than one table"
	return output.Report{
		SchemaVersion: output.SchemaVersion, Repo: "o/r", GeneratedAt: day, Model: "m",
		Stats: output.Stats{PRsScanned: 10, PRsWithDiscussion: 6, Rules: len(rs), ByTier: output.Tally(rs)},
		Rules: rs,
	}
}

func read(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestWriteAllFormats(t *testing.T) {
	dest := t.TempDir()
	rulesDir := filepath.Join(dest, ".claude", "rules")
	// A stale generated file is removed; a hand-written one with the same
	// prefix is kept.
	if err := os.MkdirAll(rulesDir, 0o755); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(rulesDir, "prrules-naming.md"), []byte("<!-- "+Marker+" -->\nold"), 0o644)
	os.WriteFile(filepath.Join(rulesDir, "prrules-mine.md"), []byte("hand written"), 0o644)

	files, err := Write(sampleReport(), Options{Formats: Formats, Dest: dest})
	if err != nil {
		t.Fatal(err)
	}
	var rel []string
	for _, f := range files {
		r, _ := filepath.Rel(dest, f)
		rel = append(rel, filepath.ToSlash(r))
	}
	want := []string{
		".claude/rules/prrules-error-handling.md",
		".claude/rules/prrules-scoped-internal-db.md",
		".claude/skills/o-r-review-rules/SKILL.md",
		".claude/skills/o-r-review-rules/reference/evidence.md",
		ManualFile,
		"rules.json",
	}
	if !slices.Equal(rel, want) {
		t.Errorf("files\n got %v\nwant %v", rel, want)
	}
	if _, err := os.Stat(filepath.Join(rulesDir, "prrules-naming.md")); !os.IsNotExist(err) {
		t.Errorf("stale generated file kept")
	}
	if _, err := os.Stat(filepath.Join(rulesDir, "prrules-mine.md")); err != nil {
		t.Errorf("hand-written file removed: %v", err)
	}

	scoped := read(t, filepath.Join(rulesDir, "prrules-scoped-internal-db.md"))
	if !strings.HasPrefix(scoped, "---\npaths:\n  - \"internal/db/**\"\n---\n") {
		t.Errorf("scoped file frontmatter:\n%s", scoped)
	}
	for _, s := range []string{"## Always", "**Close rows** [correctness]", "## When the condition holds",
		"When a repository method writes more than one table: Use transactions always."} {
		if !strings.Contains(scoped, s) {
			t.Errorf("scoped file missing %q:\n%s", s, scoped)
		}
	}
	// Consider-tier rules stay out of always-loaded rule files by default.
	if all := read(t, filepath.Join(rulesDir, "prrules-error-handling.md")); strings.HasPrefix(all, "---") || strings.Contains(all, "table tests") {
		t.Errorf("repo-wide file:\n%s", all)
	}

	skill := read(t, filepath.Join(dest, ".claude/skills/o-r-review-rules/SKILL.md"))
	for _, s := range []string{"name: o-r-review-rules\ndescription: \"Code review conventions for o/r", "## Golden rules",
		"## Hints", "#### Use table tests (`table-tests`)", "```go\nreturn err\n```"} {
		if !strings.Contains(skill, s) {
			t.Errorf("SKILL.md missing %q:\n%s", s, skill)
		}
	}
	if strings.Contains(skill, "Old idea") {
		t.Errorf("SKILL.md includes a rejected rule")
	}
	ev := read(t, filepath.Join(dest, ".claude/skills/o-r-review-rules/reference/evidence.md"))
	if !strings.Contains(ev, "> Please wrap this.\n> We lose the call site.\n>\n> alice (maintainer), [PR #812](https://github.com/o/r/pull/812#discussion_r1), 2025-09-12, accepted") {
		t.Errorf("evidence.md:\n%s", ev)
	}

	manual := read(t, filepath.Join(dest, ManualFile))
	for _, s := range []string{"# o/r code review handbook", "| Golden | 2 |", "[Golden rules](#golden-rules)",
		"### Error handling", "#### Old idea", "4 PRs · 2 reviewers · maintainer-endorsed · 3 accepted · seen 2024-09 to 2025-09"} {
		if !strings.Contains(manual, s) {
			t.Errorf("manual missing %q:\n%s", s, manual)
		}
	}
}

func TestMinTierOverride(t *testing.T) {
	dest := t.TempDir()
	if _, err := Write(sampleReport(), Options{Formats: []string{FormatManual}, Dest: dest, MinTier: rules.TierGolden}); err != nil {
		t.Fatal(err)
	}
	if m := read(t, filepath.Join(dest, ManualFile)); strings.Contains(m, "Use transactions") || !strings.Contains(m, "Wrap errors") {
		t.Errorf("min tier not applied:\n%s", m)
	}
}

func TestFence(t *testing.T) {
	if got := fence("", "a ``` b"); got != "````\na ``` b\n````" {
		t.Errorf("fence = %q", got)
	}
}

func TestParseFormats(t *testing.T) {
	if f, err := ParseFormats(" skill,manual,skill "); err != nil || !slices.Equal(f, []string{"skill", "manual"}) {
		t.Errorf("got %v %v", f, err)
	}
	if f, _ := ParseFormats("json,all"); !slices.Equal(f, []string{"json", "claude-rules", "skill", "manual"}) {
		t.Errorf("all = %v", f)
	}
	if _, err := ParseFormats("pdf"); err == nil {
		t.Error("want error for unknown format")
	}
}

func TestSkillName(t *testing.T) {
	if got := SkillName(output.Report{Repo: "Big.Org/My_Repo"}); got != "big-org-my-repo-review-rules" {
		t.Errorf("got %q", got)
	}
	if got := SkillName(output.Report{Repos: []string{"a/b", "c/d"}}); got != "combined-review-rules" {
		t.Errorf("got %q", got)
	}
}
