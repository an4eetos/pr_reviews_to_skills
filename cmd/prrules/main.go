// Command prrules mines a GitHub repository's pull-request review
// discussions into scored code-review rules (rules.json).
package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/an4eetos/pr_reviews_to_skills/internal/config"
	"github.com/an4eetos/pr_reviews_to_skills/internal/pipeline"
)

const usage = `prrules - mine PR review discussions into scored code-review rules

Usage:
  prrules <command> [flags]

Commands:
  run         fetch -> digest -> extract -> synthesize (default)
  fetch       download PRs with reviews, threads and comments (GitHub GraphQL)
  digest      filter noise and pack discussions into chunks
  estimate    print the token and cost estimate for extract + synthesize
  extract     extract candidate rules per chunk (Batch API unless --sync)
  synthesize  merge candidates, score them, write rules.json

Environment:
  GITHUB_TOKEN       token with read access to the repo (required for fetch)
  GITHUB_API_URL     optional, for GitHub Enterprise Server
  ANTHROPIC_API_KEY  or an 'ant auth login' profile (required for extract/synthesize)

Run 'prrules <command> -h' for flags.
`

func main() {
	log.SetFlags(log.Ltime)
	log.SetOutput(os.Stderr)

	args := os.Args[1:]
	cmd := "run"
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		cmd, args = args[0], args[1:]
	}
	if cmd == "help" || cmd == "-h" || cmd == "--help" {
		fmt.Print(usage)
		return
	}
	switch cmd {
	case "run", "fetch", "digest", "estimate", "extract", "synthesize":
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n%s", cmd, usage)
		os.Exit(2)
	}

	cfg, err := parseFlags(cmd, args)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return
		}
		log.Fatal(err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	p := &pipeline.Pipeline{Cfg: cfg, Logf: log.Printf}
	if !cfg.Yes {
		p.Confirm = confirm
	}

	switch cmd {
	case "run":
		err = p.Run(ctx)
	case "fetch":
		err = p.Fetch(ctx)
	case "digest":
		_, err = p.Digest()
	case "estimate":
		_, err = p.Estimate(ctx, true)
	case "extract":
		err = p.Extract(ctx)
	case "synthesize":
		err = p.Synthesize(ctx)
	}
	if err != nil {
		if errors.Is(err, context.Canceled) {
			log.Fatal("interrupted; re-run the same command to resume")
		}
		log.Fatal(err)
	}
}

func parseFlags(cmd string, args []string) (*config.Config, error) {
	fs := flag.NewFlagSet("prrules "+cmd, flag.ContinueOnError)
	var (
		repo     = fs.String("repo", "", "repository as owner/name or URL (prompted if omitted)")
		cacheDir = fs.String("cache-dir", ".prrules", "directory for intermediate artifacts")
		out      = fs.String("out", "rules.json", "output file")
		states   = fs.String("state", "open,closed,merged", "PR states to fetch")
		since    = fs.String("since", "", "only PRs updated on or after this date (YYYY-MM-DD)")
		maxPRs   = fs.Int("max-prs", 0, "stop after this many PRs (0 = all)")
		pageSize = fs.Int("page-size", 25, "PRs per GraphQL page (shrinks automatically on timeouts)")
		bots     = fs.Bool("include-bots", false, "keep comments from bots (including AI review bots)")
		chunk    = fs.Int("chunk-tokens", 40000, "approximate tokens of discussion per extraction request")
		model    = fs.String("model", config.DefaultModel, "Claude model")
		sync     = fs.Bool("sync", false, "use direct concurrent calls instead of the Batch API")
		conc     = fs.Int("concurrency", 4, "parallel requests in --sync mode and synthesis")
		noFB     = fs.Bool("no-fallbacks", false, "disable server-side refusal fallback")
		poll     = fs.Duration("poll", time.Minute, "Batch API polling interval")
		exEffort = fs.String("extract-effort", "medium", "effort for extraction (low|medium|high|xhigh|max)")
		syEffort = fs.String("synth-effort", "high", "effort for merging and scoring")
		dryRun   = fs.Bool("dry-run", false, "run: stop after fetch + digest and print the cost estimate")
		yes      = fs.Bool("yes", false, "do not ask for confirmation before spending on the LLM")
		keepRej  = fs.Bool("keep-rejected", false, "include rejected rules in rules.json")
	)
	fs.Usage = func() {
		fmt.Fprintf(fs.Output(), "Usage: prrules %s [flags]\n\n", cmd)
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return nil, err
	}

	if *repo == "" {
		*repo = os.Getenv("PRRULES_REPO")
	}
	if *repo == "" {
		r, err := prompt("GitHub repo (owner/name or URL): ")
		if err != nil {
			return nil, fmt.Errorf("--repo is required: %w", err)
		}
		*repo = r
	}
	rp, err := config.ParseRepo(*repo)
	if err != nil {
		return nil, err
	}
	st, err := config.ParseStates(*states)
	if err != nil {
		return nil, err
	}
	sinceT, err := config.ParseSince(*since)
	if err != nil {
		return nil, err
	}
	for _, e := range []string{*exEffort, *syEffort} {
		switch e {
		case "low", "medium", "high", "xhigh", "max":
		default:
			return nil, fmt.Errorf("invalid effort %q", e)
		}
	}
	if *chunk < 4000 {
		return nil, fmt.Errorf("--chunk-tokens must be at least 4000")
	}
	return &config.Config{
		Repo:          rp,
		CacheDir:      *cacheDir,
		Out:           *out,
		GitHubToken:   os.Getenv("GITHUB_TOKEN"),
		GitHubAPIURL:  os.Getenv("GITHUB_API_URL"),
		States:        st,
		Since:         sinceT,
		MaxPRs:        *maxPRs,
		PageSize:      *pageSize,
		IncludeBots:   *bots,
		ChunkTokens:   *chunk,
		Model:         *model,
		Sync:          *sync,
		Concurrency:   *conc,
		Fallbacks:     !*noFB,
		PollInterval:  *poll,
		ExtractEffort: *exEffort,
		SynthEffort:   *syEffort,
		DryRun:        *dryRun,
		Yes:           *yes,
		KeepRejected:  *keepRej,
	}, nil
}

func isTerminal(f *os.File) bool {
	st, err := f.Stat()
	return err == nil && st.Mode()&os.ModeCharDevice != 0
}

func prompt(q string) (string, error) {
	if !isTerminal(os.Stdin) {
		return "", errors.New("stdin is not a terminal")
	}
	fmt.Fprint(os.Stderr, q)
	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return "", err
	}
	line = strings.TrimSpace(line)
	if line == "" {
		return "", errors.New("no repo given")
	}
	return line, nil
}

func confirm(q string) bool {
	if !isTerminal(os.Stdin) {
		log.Print("not a terminal: pass --yes to approve LLM spend non-interactively")
		return false
	}
	fmt.Fprintf(os.Stderr, "%s [y/N] ", q)
	line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
	ans := strings.ToLower(strings.TrimSpace(line))
	return ans == "y" || ans == "yes"
}
