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
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/an4eetos/pr_reviews_to_skills/internal/config"
	"github.com/an4eetos/pr_reviews_to_skills/internal/export"
	"github.com/an4eetos/pr_reviews_to_skills/internal/output"
	"github.com/an4eetos/pr_reviews_to_skills/internal/pipeline"
	"github.com/an4eetos/pr_reviews_to_skills/internal/store"
	"github.com/an4eetos/pr_reviews_to_skills/internal/workflow"
)

const usage = `prrules - mine PR review discussions into scored code-review rules

Usage:
  prrules <command> [flags]

Commands:
  run            fetch -> digest -> extract -> synthesize (default)
  fetch          download PRs with reviews, threads and comments (GitHub GraphQL);
                 after the first fetch, only PRs updated since the last one
  digest         filter noise and pack new or changed discussions into chunks
  estimate       print the token and cost estimate for what's left to extract
  extract        extract candidate rules per chunk (Batch API unless --sync)
  synthesize     fold new candidates into the rules, score them, write rules.json
  status         show each repo's fetch watermark, generations, batches and rules
  export         render rules.json as Claude Code rules, a skill or a handbook
  init-workflow  write a GitHub Actions workflow that runs prrules on a schedule

Every repo command accepts several repos: repeat --repo, give a comma list,
or pass --repos-file.

Environment:
  GITHUB_TOKEN       token with read access to the repos (required for fetch)
  GITHUB_API_URL     optional, for GitHub Enterprise Server
  ANTHROPIC_API_KEY  or an 'ant auth login' profile (required for extract/synthesize)
  PRRULES_REPO       default for --repo (comma-separated)

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
	case "run", "fetch", "digest", "estimate", "extract", "synthesize", "status", "export", "init-workflow":
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n%s", cmd, usage)
		os.Exit(2)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var err error
	switch cmd {
	case "export":
		err = runExport(args)
	case "init-workflow":
		err = runInitWorkflow(args)
	default:
		err = runRepos(ctx, cmd, args)
	}
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return
		}
		if errors.Is(err, context.Canceled) {
			log.Fatal("interrupted; re-run the same command to resume")
		}
		log.Fatal(err)
	}
}

func runRepos(ctx context.Context, cmd string, args []string) error {
	cfg, err := parseFlags(cmd, args)
	if err != nil {
		return err
	}
	m := &pipeline.Multi{Cfg: cfg, Logf: log.Printf}
	if !cfg.Yes {
		m.Confirm = confirm
	}
	each := func(fn func(*pipeline.Pipeline) error) error {
		if err := m.Each(ctx, fn); err != nil {
			return err
		}
		return m.Err()
	}
	switch cmd {
	case "run":
		return m.Run(ctx)
	case "fetch":
		return each(func(p *pipeline.Pipeline) error { return p.Fetch(ctx) })
	case "digest":
		return each(func(p *pipeline.Pipeline) error { _, err := p.Digest(); return err })
	case "estimate":
		if _, err := m.Estimate(ctx, true); err != nil {
			return err
		}
		return m.Err()
	case "extract":
		if err := m.Extract(ctx); err != nil {
			return err
		}
		return m.Err()
	case "synthesize":
		return m.Synthesize(ctx, false)
	case "status":
		return m.Status(os.Stdout)
	}
	return fmt.Errorf("unknown command %q", cmd)
}

// repoList collects repeated --repo flags.
type repoList []string

func (r *repoList) String() string     { return strings.Join(*r, ",") }
func (r *repoList) Set(v string) error { *r = append(*r, v); return nil }

func parseFlags(cmd string, args []string) (*config.Config, error) {
	fs := flag.NewFlagSet("prrules "+cmd, flag.ContinueOnError)
	var repoArgs repoList
	fs.Var(&repoArgs, "repo", "repository as owner/name or URL; repeat it or give a comma list (prompted if omitted)")
	var (
		reposFile = fs.String("repos-file", "", "file with one repo per line (# comments allowed)")
		cacheDir  = fs.String("cache-dir", ".prrules", "directory for intermediate artifacts and incremental state")
		out       = fs.String("out", "rules.json", "output file (single repo)")
		outDir    = fs.String("out-dir", "prrules-out", "output directory with several repos: <out-dir>/<owner>/<name>/rules.json")
		combined  = fs.Bool("combined", false, "also build one rule set across all repos: <out-dir>/combined/rules.json")
		states    = fs.String("state", "open,closed,merged", "PR states to fetch")
		since     = fs.String("since", "", "only PRs updated on or after this date (YYYY-MM-DD)")
		maxPRs    = fs.Int("max-prs", 0, "stop the initial fetch after this many PRs (0 = all); later runs still pick up new PRs")
		pageSize  = fs.Int("page-size", 25, "PRs per GraphQL page (shrinks automatically on timeouts)")
		bots      = fs.Bool("include-bots", false, "keep comments from bots (including AI review bots)")
		chunk     = fs.Int("chunk-tokens", 40000, "approximate tokens of discussion per extraction request")
		model     = fs.String("model", config.DefaultModel, "Claude model")
		sync      = fs.Bool("sync", false, "use direct concurrent calls instead of the Batch API")
		conc      = fs.Int("concurrency", 4, "parallel requests in --sync mode and synthesis")
		noFB      = fs.Bool("no-fallbacks", false, "disable server-side refusal fallback")
		poll      = fs.Duration("poll", time.Minute, "Batch API polling interval")
		maxWait   = fs.Duration("max-wait", 0, "stop waiting for batches after this long and leave them for the next run (0 = wait until done)")
		maxCost   = fs.Float64("max-cost", 0, "abort before spending if the estimated cost in USD is higher (0 = no cap)")
		exEffort  = fs.String("extract-effort", "medium", "effort for extraction (low|medium|high|xhigh|max)")
		syEffort  = fs.String("synth-effort", "high", "effort for merging and scoring")
		dryRun    = fs.Bool("dry-run", false, "run: stop after fetch + digest and print the cost estimate")
		yes       = fs.Bool("yes", false, "do not ask for confirmation before spending on the LLM")
		keepRej   = fs.Bool("keep-rejected", false, "include rejected rules in rules.json")
		full      = fs.Bool("full", false, "discard the fetch state and fetch every PR again")
		resynth   = fs.Bool("full-resynth", false, "rebuild the rules from all candidates instead of folding in only new ones")
		rescore   = fs.Bool("rescore-all", false, "grade every rule again, not only the ones whose evidence changed")
		exportF   = fs.String("export", "", "after synthesis, also write these formats: "+strings.Join(export.Formats, ",")+", or all (every format but json)")
		exportDir = fs.String("export-dir", ".", "where --export writes (with several repos, <export-dir>/<owner>/<name>)")
		minTier   = fs.String("min-tier", "", "weakest tier to export (golden|conditional|consider|rejected; default depends on the format)")
	)
	fs.Usage = func() {
		fmt.Fprintf(fs.Output(), "Usage: prrules %s [flags]\n\n", cmd)
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return nil, err
	}

	values := []string(repoArgs)
	if *reposFile != "" {
		more, err := config.ReadReposFile(*reposFile)
		if err != nil {
			return nil, fmt.Errorf("--repos-file: %w", err)
		}
		values = append(values, more...)
	}
	if len(values) == 0 {
		if env := os.Getenv("PRRULES_REPO"); env != "" {
			values = []string{env}
		}
	}
	if len(values) == 0 && cmd == "status" {
		found, err := discoverRepos(*cacheDir)
		if err != nil {
			return nil, err
		}
		if len(found) == 0 {
			return nil, fmt.Errorf("no repos in %s yet; pass --repo", *cacheDir)
		}
		values = found
	}
	if len(values) == 0 {
		r, err := prompt("GitHub repo(s) (owner/name or URL, comma-separated): ")
		if err != nil {
			return nil, fmt.Errorf("--repo is required: %w", err)
		}
		values = []string{r}
	}
	repos, err := config.ParseRepos(values)
	if err != nil {
		return nil, err
	}
	if len(repos) == 0 {
		return nil, errors.New("no repos given")
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
	if *maxCost < 0 || *maxWait < 0 {
		return nil, fmt.Errorf("--max-cost and --max-wait can't be negative")
	}
	var formats []string
	if *exportF != "" {
		if formats, err = export.ParseFormats(*exportF); err != nil {
			return nil, err
		}
	}
	if _, err := export.ParseTier(*minTier); err != nil {
		return nil, fmt.Errorf("--min-tier: %w", err)
	}
	if cmd == "status" && store.Exists(filepath.Join(*cacheDir, "_combined", "synth_state.json")) {
		*combined = true
	}
	return &config.Config{
		Repo:          repos[0],
		Repos:         repos,
		CacheDir:      *cacheDir,
		Out:           *out,
		OutDir:        *outDir,
		Combined:      *combined,
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
		Full:          *full,
		FullResynth:   *resynth,
		RescoreAll:    *rescore,
		MaxWait:       *maxWait,
		MaxCost:       *maxCost,
		Export:        formats,
		ExportDir:     *exportDir,
		MinTier:       *minTier,
	}, nil
}

// discoverRepos lists the repos that have state in the cache dir.
func discoverRepos(cacheDir string) ([]string, error) {
	matches, err := filepath.Glob(filepath.Join(cacheDir, "*", "*", "fetch_state.json"))
	if err != nil {
		return nil, err
	}
	var out []string
	for _, m := range matches {
		name := filepath.Dir(m)
		owner := filepath.Dir(name)
		out = append(out, filepath.Base(owner)+"/"+filepath.Base(name))
	}
	return out, nil
}

func runExport(args []string) error {
	fs := flag.NewFlagSet("prrules export", flag.ContinueOnError)
	var (
		in      = fs.String("in", "rules.json", "rules.json to render")
		format  = fs.String("format", "all", "formats: "+strings.Join(export.Formats, ",")+", or all (every format but json)")
		dest    = fs.String("dest", ".", "directory to write under (the repo root, for Claude Code to pick the files up)")
		minTier = fs.String("min-tier", "", "weakest tier to include (default: conditional for claude-rules, consider for skill, all for manual and json)")
	)
	fs.Usage = func() {
		fmt.Fprint(fs.Output(), "Usage: prrules export [flags]\n\n"+
			"Formats:\n"+
			"  claude-rules  .claude/rules/prrules-*.md, path-scoped rule files for Claude Code\n"+
			"  skill         .claude/skills/<repo>-review-rules/ (SKILL.md + reference/evidence.md)\n"+
			"  manual        "+export.ManualFile+", a handbook for people\n"+
			"  json          rules.json filtered to --min-tier\n\n")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return err
	}
	formats, err := export.ParseFormats(*format)
	if err != nil {
		return err
	}
	tier, err := export.ParseTier(*minTier)
	if err != nil {
		return fmt.Errorf("--min-tier: %w", err)
	}
	var rep output.Report
	ok, err := store.ReadJSON(*in, &rep)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("%s not found; run prrules first", *in)
	}
	if err := output.Validate(rep); err != nil {
		return fmt.Errorf("%s: %w", *in, err)
	}
	if abs, _ := filepath.Abs(*in); slices.Contains(formats, export.FormatJSON) && abs == mustAbs(filepath.Join(*dest, "rules.json")) {
		return fmt.Errorf("the json export would overwrite %s; pick another --dest", *in)
	}
	files, err := export.Write(rep, export.Options{Formats: formats, Dest: *dest, MinTier: tier})
	if err != nil {
		return err
	}
	for _, f := range files {
		fmt.Println(f)
	}
	return nil
}

func mustAbs(p string) string {
	a, _ := filepath.Abs(p)
	return a
}

func runInitWorkflow(args []string) error {
	fs := flag.NewFlagSet("prrules init-workflow", flag.ContinueOnError)
	var repoArgs repoList
	fs.Var(&repoArgs, "repo", "repository to mine; repeat it or give a comma list")
	var (
		reposFile = fs.String("repos-file", "", "file with one repo per line")
		combined  = fs.Bool("combined", false, "also build one rule set across all repos")
		cron      = fs.String("cron", "0 5 * * *", "schedule (UTC, GitHub cron syntax)")
		exportF   = fs.String("export", "claude-rules,skill,manual", "formats the workflow commits")
		maxCost   = fs.Float64("max-cost", 25, "per-run spend cap in USD")
		maxWait   = fs.Duration("max-wait", 45*time.Minute, "how long a run waits for batches before leaving them to the next run")
		version   = fs.String("version", "latest", "prrules version to install")
		out       = fs.String("out", workflow.DefaultPath, "where to write the workflow (- for stdout)")
		force     = fs.Bool("force", false, "overwrite an existing workflow file")
	)
	fs.Usage = func() {
		fmt.Fprint(fs.Output(), "Usage: prrules init-workflow --repo owner/name [flags]\n\n")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return err
	}
	values := []string(repoArgs)
	if *reposFile != "" {
		more, err := config.ReadReposFile(*reposFile)
		if err != nil {
			return fmt.Errorf("--repos-file: %w", err)
		}
		values = append(values, more...)
	}
	repos, err := config.ParseRepos(values)
	if err != nil {
		return err
	}
	var names []string
	for _, r := range repos {
		names = append(names, r.String())
	}
	formats, err := export.ParseFormats(*exportF)
	if err != nil {
		return err
	}
	data, err := workflow.Render(workflow.Options{Repos: names, Combined: *combined, Cron: *cron, Export: formats,
		MaxCost: *maxCost, MaxWait: *maxWait, Version: *version})
	if err != nil {
		return err
	}
	if *out == "-" {
		_, err := os.Stdout.Write(data)
		return err
	}
	if store.Exists(*out) && !*force {
		return fmt.Errorf("%s already exists; pass --force to overwrite it", *out)
	}
	if err := store.WriteFile(*out, data); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "wrote %s\nadd the ANTHROPIC_API_KEY secret (and PRRULES_GITHUB_TOKEN if it mines other repos), then commit the file\n", *out)
	return nil
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
