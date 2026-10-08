// Package config holds run configuration assembled from flags and env.
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const DefaultModel = "claude-opus-5-5"

type Config struct {
	// Repo is the repository a single-repo pipeline works on; Repos lists
	// every repository of the command.
	Repo     Repo
	Repos    []Repo
	CacheDir string
	Out      string
	// OutDir holds per-repo and combined rules.json files when there is more
	// than one repo.
	OutDir   string
	Combined bool

	// GitHub
	GitHubToken  string
	GitHubAPIURL string
	States       []string // OPEN, CLOSED, MERGED
	Since        time.Time
	MaxPRs       int
	PageSize     int

	// Digest
	IncludeBots bool
	ChunkTokens int

	// LLM
	Model         string
	Sync          bool
	Concurrency   int
	Fallbacks     bool
	PollInterval  time.Duration
	ExtractEffort string
	SynthEffort   string

	// Run control
	DryRun       bool
	Yes          bool
	KeepRejected bool
	// Full refetches everything; FullResynth rebuilds the rules from all
	// candidates instead of folding new ones in; RescoreAll grades every rule
	// again, not only the changed ones.
	Full        bool
	FullResynth bool
	RescoreAll  bool
	// MaxWait bounds how long a run waits for batches (0 = until they end).
	MaxWait time.Duration
	// MaxCost aborts before spending when the estimate exceeds it (0 = no cap).
	MaxCost float64

	// Export
	Export    []string
	ExportDir string
	MinTier   string
}

// Multi reports whether the command covers more than one repository.
func (c *Config) Multi() bool { return len(c.Repos) > 1 }

// ForRepo returns a copy of c for one repository. With several repos, each
// one writes its rules.json and exports under its own owner/name directory.
func (c *Config) ForRepo(r Repo) *Config {
	cp := *c
	cp.Repo = r
	if c.Multi() {
		cp.Out = filepath.Join(c.OutDir, r.Owner, r.Name, "rules.json")
		cp.ExportDir = filepath.Join(c.ExportDir, r.Owner, r.Name)
	}
	return &cp
}

// ParseRepos parses and dedupes repos given as separate values and/or
// comma-separated lists.
func ParseRepos(values []string) ([]Repo, error) {
	var out []Repo
	seen := map[string]bool{}
	for _, v := range values {
		for _, s := range strings.Split(v, ",") {
			if strings.TrimSpace(s) == "" {
				continue
			}
			r, err := ParseRepo(s)
			if err != nil {
				return nil, err
			}
			key := strings.ToLower(r.Host + "/" + r.String())
			if !seen[key] {
				seen[key] = true
				out = append(out, r)
			}
		}
	}
	return out, nil
}

// ReadReposFile reads one repo per line; blank lines and # comments are
// skipped.
func ReadReposFile(path string) ([]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, line := range strings.Split(string(data), "\n") {
		if i := strings.Index(line, "#"); i >= 0 {
			line = line[:i]
		}
		if line = strings.TrimSpace(line); line != "" {
			out = append(out, line)
		}
	}
	return out, nil
}

// RepoCacheDir is where all stage artifacts for this repo live.
func (c *Config) RepoCacheDir() string {
	return filepath.Join(c.CacheDir, c.Repo.Owner, c.Repo.Name)
}

// ParseStates turns "open,closed,merged" into GraphQL PullRequestState values.
func ParseStates(s string) ([]string, error) {
	var out []string
	seen := map[string]bool{}
	for _, p := range strings.Split(s, ",") {
		p = strings.ToUpper(strings.TrimSpace(p))
		if p == "" {
			continue
		}
		switch p {
		case "OPEN", "CLOSED", "MERGED":
		default:
			return nil, fmt.Errorf("unknown PR state %q (want open, closed, merged)", p)
		}
		if !seen[p] {
			seen[p] = true
			out = append(out, p)
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no PR states given")
	}
	return out, nil
}

// ParseSince accepts YYYY-MM-DD or RFC3339; empty means no lower bound.
func ParseSince(s string) (time.Time, error) {
	if s == "" {
		return time.Time{}, nil
	}
	if t, err := time.Parse("2006-01-02", s); err == nil {
		return t, nil
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}, fmt.Errorf("invalid --since %q: want YYYY-MM-DD or RFC3339", s)
	}
	return t, nil
}
