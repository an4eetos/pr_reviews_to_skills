// Package config holds run configuration assembled from flags and env.
package config

import (
	"fmt"
	"path/filepath"
	"strings"
	"time"
)

const DefaultModel = "claude-opus-5-5"

type Config struct {
	Repo     Repo
	CacheDir string
	Out      string

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
