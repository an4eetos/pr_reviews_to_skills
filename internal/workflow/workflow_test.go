package workflow

import (
	"strings"
	"testing"
	"time"
)

func TestRenderSingleRepo(t *testing.T) {
	out, err := Render(Options{Repos: []string{"o/r"}, Cron: "0 5 * * *", Export: []string{"claude-rules", "skill", "manual"},
		MaxCost: 20, MaxWait: 45 * time.Minute, Version: "latest"})
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	for _, want := range []string{
		`- cron: "0 5 * * *"`,
		"prrules@latest",
		"key: prrules-${{ github.run_id }}",
		"GITHUB_TOKEN: ${{ secrets.PRRULES_GITHUB_TOKEN || github.token }}",
		"prrules run --yes\n          --repo o/r\n          --max-wait 45m0s\n          --max-cost 20\n          --export claude-rules,skill,manual\n          --export-dir .\n",
		"add-paths: |\n            .claude/rules/prrules-*.md\n            .claude/skills/\n            PR-REVIEW-HANDBOOK.md\n",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("missing %q in:\n%s", want, s)
		}
	}
	if strings.Contains(s, "[[") || strings.Contains(s, "--combined") {
		t.Errorf("unexpected content:\n%s", s)
	}
}

func TestRenderMultiRepo(t *testing.T) {
	out, err := Render(Options{Repos: []string{"o/a", "o/b"}, Combined: true, Cron: "30 4 * * 1", Export: []string{"manual"},
		MaxCost: 7.5, MaxWait: time.Hour, Version: "v1.2.0"})
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	for _, want := range []string{"--repo o/a\n          --repo o/b\n          --combined\n          --out-dir prrules-out\n",
		"--export-dir prrules-out", "add-paths: |\n            prrules-out/\n", "--max-cost 7.5"} {
		if !strings.Contains(s, want) {
			t.Errorf("missing %q in:\n%s", want, s)
		}
	}
}

func TestRenderValidates(t *testing.T) {
	base := Options{Repos: []string{"o/r"}, Cron: "0 5 * * *", Export: []string{"manual"}, MaxCost: 1}
	for name, mod := range map[string]func(*Options){
		"no repos":  func(o *Options) { o.Repos = nil },
		"bad cron":  func(o *Options) { o.Cron = "daily" },
		"no cap":    func(o *Options) { o.MaxCost = 0 },
		"no export": func(o *Options) { o.Export = nil },
	} {
		o := base
		mod(&o)
		if _, err := Render(o); err == nil {
			t.Errorf("%s: want error", name)
		}
	}
}
