// Package workflow renders a GitHub Actions workflow that runs prrules on a
// schedule and opens a pull request when the rules change.
package workflow

import (
	"bytes"
	_ "embed"
	"fmt"
	"path"
	"strconv"
	"strings"
	"text/template"
	"time"
)

//go:embed prrules.yml.tmpl
var tmplText string

// The template uses [[ ]] so GitHub's own ${{ }} expressions pass through.
var tmpl = template.Must(template.New("workflow").Delims("[[", "]]").Parse(tmplText))

type Options struct {
	Repos    []string
	Combined bool
	// Cron is the schedule in UTC, in GitHub's five-field cron syntax.
	Cron    string
	Export  []string
	MaxCost float64
	MaxWait time.Duration
	// Version is the prrules version `go install` fetches.
	Version string
}

// DefaultExportDir is where a multi-repo workflow writes its rules; a
// single-repo workflow writes into the repository root, where Claude Code
// picks the files up.
const DefaultExportDir = "prrules-out"

type data struct {
	Options
	Multi     bool
	ExportDir string
	Export    string
	MaxCost   string
	MaxWait   string
	Paths     []string
}

func Render(o Options) ([]byte, error) {
	if len(o.Repos) == 0 {
		return nil, fmt.Errorf("at least one --repo is required")
	}
	if len(strings.Fields(o.Cron)) != 5 {
		return nil, fmt.Errorf("--cron %q: want five fields, e.g. \"0 5 * * *\"", o.Cron)
	}
	if o.MaxCost <= 0 {
		return nil, fmt.Errorf("--max-cost must be positive: a scheduled run should never spend without a cap")
	}
	d := data{
		Options: o,
		Multi:   len(o.Repos) > 1,
		Export:  strings.Join(o.Export, ","),
		MaxCost: strconv.FormatFloat(o.MaxCost, 'f', -1, 64),
		MaxWait: o.MaxWait.String(),
	}
	if d.Multi {
		d.ExportDir = DefaultExportDir
		d.Paths = []string{DefaultExportDir + "/"}
	} else {
		d.ExportDir = "."
		for _, f := range o.Export {
			switch f {
			case "claude-rules":
				d.Paths = append(d.Paths, ".claude/rules/prrules-*.md")
			case "skill":
				d.Paths = append(d.Paths, ".claude/skills/")
			case "manual":
				d.Paths = append(d.Paths, "PR-REVIEW-HANDBOOK.md")
			case "json":
				d.Paths = append(d.Paths, "rules.json")
			}
		}
	}
	if len(d.Paths) == 0 {
		return nil, fmt.Errorf("--export must name at least one format, or there is nothing to open a pull request with")
	}
	var b bytes.Buffer
	if err := tmpl.Execute(&b, d); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}

// DefaultPath is where the workflow file goes in a repository.
var DefaultPath = path.Join(".github", "workflows", "prrules.yml")
