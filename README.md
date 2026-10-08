# pr_reviews_to_skills

`prrules` reads every pull-request discussion in a GitHub repository and turns what reviewers keep asking for into a scored list of rules: architecture, style and naming conventions, and mistakes that keep recurring. Each rule carries quoted evidence from real review comments and a trust tier, so you can hand the strong ones to developers and coding agents and treat the weak ones as hints.

Run it once, or on a schedule: after the first run it only looks at PRs that are new or changed since the last one and folds what it finds into the existing rules. It can mine several repos in one command. The output is a `rules.json`, which `prrules` can also render as path-scoped Claude Code rules, a Claude Code skill, and a handbook for people.

## Quick start

```sh
go install github.com/an4eetos/pr_reviews_to_skills/cmd/prrules@latest

export GITHUB_TOKEN=ghp_...          # read access to the repo
export ANTHROPIC_API_KEY=sk-ant-...  # or log in once with `ant auth login`

prrules run --repo owner/name --dry-run   # fetch + digest, print the cost estimate, spend nothing
prrules run --repo owner/name             # the full run, asks before spending
prrules export --format all               # .claude/rules, a skill and PR-REVIEW-HANDBOOK.md from rules.json
```

If you leave out `--repo`, it asks for one. It accepts `owner/name`, a GitHub URL, or a GitHub Enterprise URL. For GitHub Enterprise Server, set `GITHUB_API_URL`.

Run the same command again later and it only processes what changed. To run it every day, see [Scheduled runs](#scheduled-runs).

## How it works

```
fetch  ──►  digest  ──►  extract (map)  ──►  synthesize (reduce + score)  ──►  rules.json
GraphQL     filter       Claude, Batch API   merge per category, compute
            & chunk      one call / chunk    evidence metrics, grade tiers
```

1. **fetch** pages through all PRs over the GraphQL API, most recently updated first. Each PR comes with its reviews, inline review threads (diff hunk, resolved and outdated state, reactions) and conversation comments. Rate limits are respected: it sleeps until the reset time when points run low, honours `Retry-After` on secondary limits, and shrinks the page size when GitHub times out. Progress is saved after every page, so an interrupted fetch resumes where it stopped.
2. **digest** drops noise: bots, "LGTM", "thanks", emoji and `/retest`. It labels every comment with the author's role (`maintainer`, `contributor` or `pr-author`) and every thread with its outcome (`resolved`, `code-changed`, `author-acknowledged` or `unresolved`). It then packs the discussions into chunks of about 40k tokens. Each kept comment gets a short ref such as `812.3`.
3. **extract** sends each chunk to Claude with a JSON schema and asks for *generalizable* rules, each citing comment refs as evidence. Refs are checked against the digest. Made-up refs are dropped, and a quote that isn't really in the cited comment is replaced with the comment's actual text. By default this stage runs through the **Message Batches API**, which costs 50% less.
4. **synthesize** merges duplicate candidates within each category. Large categories are merged in groups, then the groups are merged together. It then computes **deterministic metrics** for every rule (distinct PRs and reviewers, maintainer endorsement, accepted, disputed and opposing comments, how recent the evidence is) and asks Claude to grade each rule against them.

### Tiers

| tier          | meaning                                                                 |
|---------------|-------------------------------------------------------------------------|
| `golden`      | Firm team rule: raised repeatedly, by maintainers, consistently applied. Always follow it. |
| `conditional` | Valid only when `applies_when` holds (a part of the codebase, a type of change). |
| `consider`    | Reasonable, but the evidence is thin, contested or old. Weigh it, don't obey it. |
| `rejected`    | Contradicted, obsolete or too vague. Left out unless `--keep-rejected`. |

The model's grade is then **capped by hard evidence floors**:

- `golden` needs at least 3 PRs, plus either 2 or more reviewers or a maintainer, and no substantial pushback.
- `conditional` needs a condition.
- A rule seen in a single PR without maintainer support can't rise above `consider`.

When a rule is downgraded, `tier_reason` explains why.

## Output

`rules.json` follows [schema/rules.schema.json](schema/rules.schema.json):

```json
{
  "schema_version": 1,
  "repo": "owner/name",
  "model": "claude-opus-5-5",
  "stats": { "prs_scanned": 5123, "prs_with_discussion": 2210, "candidates": 1840, "rules": 140,
             "by_tier": { "golden": 22, "conditional": 41, "consider": 77, "rejected": 31 } },
  "rules": [{
    "id": "wrap-storage-errors-with-context",
    "title": "Wrap storage errors with context",
    "rule": "Wrap errors returned from storage calls with fmt.Errorf(\"...: %w\", err) instead of returning them bare.",
    "rationale": "Bare errors lose the call site and make incidents hard to trace.",
    "category": "error-handling", "kind": "do",
    "tier": "golden", "confidence": 0.92, "tier_reason": "Raised by 3 maintainers across 14 PRs, always applied.",
    "applies_when": "", "scope": { "paths": ["internal/storage/**"], "languages": ["go"] },
    "examples": { "bad": "return err", "good": "return fmt.Errorf(\"load user: %w\", err)" },
    "metrics": { "distinct_prs": 14, "distinct_reviewers": 4, "maintainer_endorsed": true, "accepted": 12,
                 "disputed": 1, "ignored": 0, "opposing": 0, "thumbs_up": 3,
                 "first_seen": "2023-02-01T00:00:00Z", "last_seen": "2025-09-12T00:00:00Z", "recent_share": 0.6 },
    "evidence": [{ "pr": 812, "ref": "812.3", "url": "https://github.com/owner/name/pull/812#discussion_r123",
                   "author": "alice", "role": "maintainer", "date": "2025-09-12T00:00:00Z",
                   "quote": "Please wrap this with context, we lose the call site otherwise.", "outcome": "accepted" }]
  }]
}
```

Rules are sorted by tier, then by confidence. Each rule keeps its 5 strongest evidence comments, maintainers and accepted outcomes first.

## Incremental runs

`prrules` remembers how far it got, per repo, in its cache directory (`.prrules/` by default):

- **fetch** stores a *watermark*: the newest PR `updatedAt` it has seen, from GitHub's own timestamps. Once the first fetch is complete, or has reached `--max-prs`, each fetch asks only for PRs updated since then and stops at the watermark. An interrupted refresh resumes from its cursor.
- **digest** hashes each PR's discussion. It packs only PRs that are new, or whose discussion changed, into a new **generation** (`gens/gNNNN/`). A PR that GitHub marks as updated (a CI push, a label, a merge) with the same discussion is skipped.
- **extract** works only on generations not yet extracted. When a PR is extracted again because its discussion changed, its new extraction replaces the old evidence instead of being counted twice.
- **synthesize** *folds* the new candidates into the existing rules: Claude assigns each one to an existing rule (as support or as pushback), groups the rest into new rules, or discards it. Evidence and metrics are recomputed for every rule, and only rules whose evidence changed are graded again. The wording of existing rules is never changed, so the exported files stay stable. Use `--full-resynth` to rebuild the rules from all candidates, and `--rescore-all` to grade every rule again.

When nothing changed, a run makes no Claude calls and leaves `rules.json` and the exports untouched. `prrules status` shows each repo's watermark, generations, batches in flight and rule counts.

A cache from an earlier version is adopted as generation 0 the first time a newer version runs, so PRs that were already extracted are not paid for again.

## Several repos

Repeat `--repo`, give a comma list, or pass `--repos-file` (one repo per line, `#` comments allowed):

```sh
prrules run --repo org/api --repo org/web,org/worker --combined
```

Each stage runs for every repo before the next stage starts. The cost estimate and confirmation cover all repos at once, and every repo's extraction batches are submitted before the run waits on any of them. If one repo fails (for example, the token can't read it), the others still finish, and the run exits non-zero with a summary.

With more than one repo, each repo's rules go to `<out-dir>/<owner>/<name>/rules.json` (`--out-dir` defaults to `prrules-out`). `--combined` also writes `<out-dir>/combined/rules.json`: one rule set mined across all repos, where each rule and each piece of evidence records its repo. A combined rule seen in only one repo can't be `golden`. It becomes `conditional` on that repo.

## Exports

`prrules export` renders a `rules.json`, and `prrules run --export …` does the same after each synthesis that changed the rules:

| format | writes | default tiers |
|---|---|---|
| `claude-rules` | `.claude/rules/prrules-<category>.md` for repo-wide rules, and `.claude/rules/prrules-scoped-<paths>.md` with `paths:` frontmatter for rules scoped to part of the codebase, so Claude Code loads those only when it works on matching files | golden, conditional |
| `skill` | `.claude/skills/<owner>-<name>-review-rules/SKILL.md`, with every rule and its examples, plus `reference/evidence.md` with the quoted review comments. Claude loads it when writing or reviewing code in that repo | golden, conditional, consider (as hints) |
| `manual` | `PR-REVIEW-HANDBOOK.md`, a handbook for people: a tier table, a table of contents, and every rule with its rationale, scope, examples, metrics and quoted evidence | all in `rules.json` |
| `json` | `rules.json` filtered to `--min-tier`; not part of `all`, so name it explicitly | all |

```sh
prrules export --in rules.json --format claude-rules,skill --dest path/to/repo
prrules export --format manual --min-tier golden
prrules run --repo owner/name --export all --export-dir .
```

Generated files carry a `generated by prrules` marker. On each export, stale `prrules-*.md` rule files with that marker are deleted, and files you wrote yourself are never touched. Rules are ordered by tier, category and ID rather than confidence, so small score changes don't reorder the files.

## Scheduled runs

`prrules init-workflow` writes a GitHub Actions workflow to `.github/workflows/prrules.yml`:

```sh
prrules init-workflow --repo owner/name --cron "0 5 * * *" --max-cost 25
```

Each scheduled run:

1. restores `.prrules` from the Actions cache;
2. runs `prrules run --yes --max-wait 45m --max-cost … --export claude-rules,skill,manual`;
3. saves `.prrules` back to the cache, even if the run failed;
4. opens or updates a pull request when the exported files changed.

Add an `ANTHROPIC_API_KEY` secret. To mine repos other than the one the workflow lives in, also add a `PRRULES_GITHUB_TOKEN` secret that can read them. With several repos (`--repo` repeated, or `--repos-file`), exports go under `prrules-out/<owner>/<name>/`, and `--combined` adds `prrules-out/combined/`.

Two flags make unattended runs safe:

- `--max-wait 45m` stops waiting for Batch API jobs after 45 minutes. The batch IDs are saved, and the next run collects the results without submitting again. Generations that are already extracted are folded in right away.
- `--max-cost 25` aborts before anything is submitted if the estimate is above $25. If the Actions cache is ever evicted (GitHub drops caches unused for 7 days; a daily schedule keeps it fresh), the next run would start from scratch, and this cap stops it from spending a full run's budget unattended.

## Commands and flags

`prrules run` runs every stage. Each stage can also be run on its own: `fetch`, `digest`, `estimate`, `extract` and `synthesize`. Each stage reads the previous stage's files from `.prrules/<owner>/<repo>/`, so you can re-run one stage (for example after editing a prompt) without starting over. Every completed LLM call is cached under a hash of its prompt, so interrupted or repeated runs never pay twice for the same work. `status`, `export` and `init-workflow` are described above.

| flag | default | |
|---|---|---|
| `--repo` | prompted | `owner/name` or URL; repeat it or give a comma list |
| `--repos-file` | | one repo per line |
| `--combined` | off | also build one rule set across all repos |
| `--since` | | only PRs updated on or after `YYYY-MM-DD` |
| `--max-prs` | all | stop the initial fetch after N PRs (raise it later to continue); new PRs are still picked up |
| `--state` | `open,closed,merged` | PR states to fetch |
| `--include-bots` | off | keep comments from bots, including AI reviewers |
| `--chunk-tokens` | 40000 | discussion per extraction request |
| `--model` | `claude-opus-5-5` | Claude model |
| `--sync` | off | direct parallel calls instead of the Batch API: faster on small repos, but no batch discount |
| `--extract-effort` / `--synth-effort` | `medium` / `high` | thinking effort per stage |
| `--dry-run` | off | stop after the cost estimate |
| `--yes` | off | skip the spend confirmation (needed when not on a terminal) |
| `--max-cost` | no cap | abort before spending if the estimate in USD is higher |
| `--max-wait` | until done | stop waiting for batches after this long; the next run collects them |
| `--full` | off | fetch every PR again |
| `--full-resynth` / `--rescore-all` | off | rebuild the rules from all candidates / grade every rule again |
| `--export` / `--export-dir` / `--min-tier` | none / `.` / per format | write exports after synthesis |
| `--keep-rejected` | off | include `rejected` rules in the output |
| `--no-fallbacks` | off | turn off the server-side refusal fallback |
| `--cache-dir` / `--out` / `--out-dir` | `.prrules` / `rules.json` / `prrules-out` | `--out` is for one repo, `--out-dir` for several |

### Large repos

- Batch jobs usually finish within an hour and can take up to 24h. The batch IDs are saved, so if you stop `prrules`, or `--max-wait` runs out, running the same command again picks up polling instead of resubmitting.
- Use `--since` to focus on current practice. Older evidence still counts, but `recent_share` and the scorer weigh it down.
- Use `prrules estimate` to see the cost of what's left to extract before you commit. The estimate calibrates its token count against the API's `count_tokens`.

### Notes

- Review comments are untrusted input. They are wrapped in delimiters, any of the tool's own tag names inside them are neutralized, and the prompts tell the model never to follow instructions found in the data.
- PRs are fetched in order of last update. A PR updated while the fetch is running can show up twice; digest keeps only the latest copy.
- Comments inside a single review thread are capped at 30, which is enough to capture the decision.

## Development

```sh
go test ./...
```

The GitHub client is tested against an `httptest` GraphQL server and the LLM stages against a fake client, so the tests need no credentials. Prompts are in [prompts/](prompts/).
