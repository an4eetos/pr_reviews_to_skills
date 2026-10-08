package github

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/an4eetos/pr_reviews_to_skills/internal/store"
)

const (
	threadCommentsPage = 30
	nestedPage         = 50
	minPageSize        = 5
)

const actorFields = `author { login __typename } authorAssociation`

const commentFields = `id url body createdAt ` + actorFields + `
	thumbsUp: reactions(content: THUMBS_UP) { totalCount }
	thumbsDown: reactions(content: THUMBS_DOWN) { totalCount }`

const reviewFields = `id url state body submittedAt ` + actorFields

var threadFields = fmt.Sprintf(`id path line isResolved isOutdated
	comments(first: %d) { totalCount nodes { %s diffHunk } }`, threadCommentsPage, commentFields)

var prFields = fmt.Sprintf(`number title body url state merged createdAt updatedAt mergedAt
	%s
	labels(first: 20) { nodes { name } }
	files(first: 50) { totalCount nodes { path } }
	reviews(first: %[2]d) { totalCount pageInfo { hasNextPage endCursor } nodes { %[3]s } }
	reviewThreads(first: %[2]d) { totalCount pageInfo { hasNextPage endCursor } nodes { %[4]s } }
	comments(first: %[2]d) { totalCount pageInfo { hasNextPage endCursor } nodes { %[5]s } }`,
	actorFields, nestedPage, reviewFields, threadFields, commentFields)

var pageQuery = `query($owner: String!, $name: String!, $first: Int!, $after: String, $states: [PullRequestState!]) {
  rateLimit { cost remaining resetAt }
  repository(owner: $owner, name: $name) {
    pullRequests(first: $first, after: $after, states: $states, orderBy: {field: UPDATED_AT, direction: DESC}) {
      pageInfo { hasNextPage endCursor }
      nodes { ` + prFields + ` }
    }
  }
}`

func nestedQuery(field, nodeFields string) string {
	return fmt.Sprintf(`query($owner: String!, $name: String!, $number: Int!, $after: String) {
  rateLimit { cost remaining resetAt }
  repository(owner: $owner, name: $name) {
    pullRequest(number: $number) {
      %s(first: %d, after: $after) { pageInfo { hasNextPage endCursor } nodes { %s } }
    }
  }
}`, field, nestedPage, nodeFields)
}

type FetchOptions struct {
	Owner, Name string
	States      []string
	Since       time.Time
	MaxPRs      int // 0 = unlimited
	PageSize    int
	// Full discards the saved state and fetches everything again.
	Full bool
}

func (o FetchOptions) key() string {
	since := ""
	if !o.Since.IsZero() {
		since = o.Since.UTC().Format(time.RFC3339)
	}
	return strings.Join([]string{o.Owner, o.Name, strings.Join(o.States, ","), since}, "|")
}

// FetchState is persisted after every page so an interrupted fetch resumes
// from the last cursor.
type FetchState struct {
	Key       string    `json:"key"`
	Cursor    string    `json:"cursor"`
	Pages     int       `json:"pages"`
	PRs       int       `json:"prs"`
	Done      bool      `json:"done"`
	UpdatedAt time.Time `json:"updatedAt"`
	// Watermark is the newest PR updatedAt stored, taken from GitHub's own
	// timestamps so local clock skew cannot open a gap. Refreshes fetch only
	// PRs updated at or after it.
	Watermark   time.Time     `json:"watermark,omitzero"`
	LastRefresh time.Time     `json:"lastRefresh,omitzero"`
	Refresh     *RefreshState `json:"refresh,omitempty"`
}

// RefreshState tracks an in-progress refresh so an interrupted one resumes.
// The watermark only advances once the refresh has reached it.
type RefreshState struct {
	Cursor       string    `json:"cursor"`
	NewWatermark time.Time `json:"newWatermark"`
	PRs          int       `json:"prs"`
}

const stateFile = "fetch_state.json"

func RawDir(dir string) string { return filepath.Join(dir, "raw") }

// LoadState reads the fetch state for a repo cache dir.
func LoadState(dir string) (FetchState, error) {
	var st FetchState
	_, err := store.ReadJSON(filepath.Join(dir, stateFile), &st)
	return st, err
}

// Fetch pages through the repo's PRs (most recently updated first) into
// dir/raw/page-NNNNN.jsonl, resuming from dir/fetch_state.json. Once the
// initial fetch is complete (or has reached MaxPRs), each call refreshes:
// it fetches only PRs updated since the watermark.
func (c *Client) Fetch(ctx context.Context, dir string, opts FetchOptions) (FetchState, error) {
	statePath := filepath.Join(dir, stateFile)
	var st FetchState
	if _, err := store.ReadJSON(statePath, &st); err != nil {
		return st, err
	}
	if st.Key != opts.key() || opts.Full {
		if st.Key != "" && !opts.Full {
			c.logf("fetch: options changed since the last fetch, starting over")
		}
		if err := os.RemoveAll(RawDir(dir)); err != nil {
			return st, err
		}
		st = FetchState{Key: opts.key()}
	}
	if st.Done || (opts.MaxPRs > 0 && st.PRs >= opts.MaxPRs) {
		if st.Watermark.IsZero() {
			// State written before watermarks existed: derive it from the raw pages.
			prs, err := LoadAll(dir)
			if err != nil {
				return st, err
			}
			for _, pr := range prs {
				if pr.UpdatedAt.After(st.Watermark) {
					st.Watermark = pr.UpdatedAt
				}
			}
		}
		return st, c.refresh(ctx, dir, opts, &st)
	}
	if st.PRs > 0 {
		c.logf("fetch: resuming after %d PRs", st.PRs)
	}

	pageSize := opts.PageSize
	for {
		page, err := c.fetchPage(ctx, opts, st.Cursor, &pageSize)
		if err != nil {
			return st, err
		}

		var prs []PullRequest
		stopSince, stopMax := false, false
		for _, node := range page.Nodes {
			if !opts.Since.IsZero() && node.UpdatedAt.Before(opts.Since) {
				stopSince = true
				break
			}
			pr := node.toPR()
			if err := c.completePR(ctx, opts, &node, &pr); err != nil {
				return st, fmt.Errorf("PR #%d: %w", pr.Number, err)
			}
			prs = append(prs, pr)
			if pr.UpdatedAt.After(st.Watermark) {
				st.Watermark = pr.UpdatedAt
			}
			if opts.MaxPRs > 0 && st.PRs+len(prs) >= opts.MaxPRs {
				stopMax = true
				break
			}
		}

		if err := writePage(dir, &st, prs); err != nil {
			return st, err
		}
		st.PRs += len(prs)
		// When we stop mid-page for --max-prs, keep the old cursor: a later run
		// with a higher limit refetches this page and digest dedupes by number.
		if !stopMax {
			st.Cursor = page.PageInfo.EndCursor
		}
		st.Done = stopSince || !page.PageInfo.HasNextPage
		st.UpdatedAt = time.Now().UTC()
		if st.Done || stopMax {
			st.LastRefresh = st.UpdatedAt
		}
		if err := store.WriteJSON(statePath, st); err != nil {
			return st, err
		}
		c.logf("fetch: %d PRs so far (page %d)", st.PRs, st.Pages)
		if st.Done || stopMax {
			return st, nil
		}
	}
}

// refresh fetches PRs updated at or after the watermark, newest first. The
// PR exactly at the watermark is fetched again on purpose: two PRs can share
// a timestamp, and digest drops unchanged PRs anyway.
func (c *Client) refresh(ctx context.Context, dir string, opts FetchOptions, st *FetchState) error {
	statePath := filepath.Join(dir, stateFile)
	if st.Refresh == nil {
		st.Refresh = &RefreshState{NewWatermark: st.Watermark}
		c.logf("fetch: checking for PRs updated since %s", st.Watermark.Format(time.RFC3339))
	} else {
		c.logf("fetch: resuming refresh after %d PRs", st.Refresh.PRs)
	}
	r := st.Refresh
	pageSize := opts.PageSize
	for {
		page, err := c.fetchPage(ctx, opts, r.Cursor, &pageSize)
		if err != nil {
			return err
		}
		var prs []PullRequest
		reached := false
		for _, node := range page.Nodes {
			if node.UpdatedAt.Before(st.Watermark) || (!opts.Since.IsZero() && node.UpdatedAt.Before(opts.Since)) {
				reached = true
				break
			}
			pr := node.toPR()
			if err := c.completePR(ctx, opts, &node, &pr); err != nil {
				return fmt.Errorf("PR #%d: %w", pr.Number, err)
			}
			prs = append(prs, pr)
			if pr.UpdatedAt.After(r.NewWatermark) {
				r.NewWatermark = pr.UpdatedAt
			}
		}
		if err := writePage(dir, st, prs); err != nil {
			return err
		}
		r.PRs += len(prs)
		r.Cursor = page.PageInfo.EndCursor
		done := reached || !page.PageInfo.HasNextPage
		if done {
			c.logf("fetch: %d PR(s) updated since the last fetch", r.PRs)
			st.Watermark = r.NewWatermark
			st.Refresh = nil
			st.LastRefresh = time.Now().UTC()
		}
		st.UpdatedAt = time.Now().UTC()
		if err := store.WriteJSON(statePath, st); err != nil {
			return err
		}
		if done {
			return nil
		}
		c.logf("fetch: refresh: %d PRs so far", r.PRs)
	}
}

// fetchPage queries one page of PRs after cursor, halving *pageSize while
// GitHub times out and recovering it gradually afterwards.
func (c *Client) fetchPage(ctx context.Context, opts FetchOptions, cursor string, pageSize *int) (conn[gqlPR], error) {
	if *pageSize <= 0 {
		*pageSize = 25
	}
	full := opts.PageSize
	if full <= 0 {
		full = 25
	}
	for {
		var resp struct {
			Repository *struct {
				PullRequests conn[gqlPR] `json:"pullRequests"`
			} `json:"repository"`
		}
		vars := map[string]any{
			"owner":  opts.Owner,
			"name":   opts.Name,
			"first":  *pageSize,
			"states": opts.States,
			"after":  nil,
		}
		if cursor != "" {
			vars["after"] = cursor
		}
		err := c.Query(ctx, pageQuery, vars, &resp)
		var te *TransientError
		if errors.As(err, &te) && *pageSize > minPageSize {
			*pageSize = max(*pageSize/2, minPageSize)
			c.logf("fetch: GitHub timing out, shrinking page size to %d", *pageSize)
			continue
		}
		if err != nil {
			return conn[gqlPR]{}, err
		}
		if resp.Repository == nil {
			return conn[gqlPR]{}, fmt.Errorf("repository %s/%s not found or not accessible with this token", opts.Owner, opts.Name)
		}
		if *pageSize < full {
			*pageSize = min(*pageSize*2, full)
		}
		return resp.Repository.PullRequests, nil
	}
}

func writePage(dir string, st *FetchState, prs []PullRequest) error {
	if len(prs) == 0 {
		return nil
	}
	path := filepath.Join(RawDir(dir), fmt.Sprintf("page-%05d.jsonl", st.Pages+1))
	if err := store.WriteJSONL(path, prs); err != nil {
		return err
	}
	st.Pages++
	return nil
}

// completePR fetches the remaining pages of reviews, threads and comments
// for PRs whose first page was not the whole connection.
func (c *Client) completePR(ctx context.Context, opts FetchOptions, node *gqlPR, pr *PullRequest) error {
	if node.Reviews.PageInfo.HasNextPage {
		more, err := fetchNested[Review](ctx, c, opts, pr.Number, "reviews", reviewFields, node.Reviews.PageInfo.EndCursor)
		if err != nil {
			return err
		}
		pr.Reviews = append(pr.Reviews, more...)
	}
	if node.ReviewThreads.PageInfo.HasNextPage {
		more, err := fetchNested[gqlThread](ctx, c, opts, pr.Number, "reviewThreads", threadFields, node.ReviewThreads.PageInfo.EndCursor)
		if err != nil {
			return err
		}
		for _, t := range more {
			pr.Threads = append(pr.Threads, t.toThread())
		}
	}
	if node.Comments.PageInfo.HasNextPage {
		more, err := fetchNested[gqlComment](ctx, c, opts, pr.Number, "comments", commentFields, node.Comments.PageInfo.EndCursor)
		if err != nil {
			return err
		}
		for _, cm := range more {
			pr.Comments = append(pr.Comments, cm.toComment())
		}
	}
	return nil
}

func fetchNested[T any](ctx context.Context, c *Client, opts FetchOptions, number int, field, nodeFields, cursor string) ([]T, error) {
	q := nestedQuery(field, nodeFields)
	var out []T
	for {
		var resp struct {
			Repository struct {
				PullRequest map[string]conn[T] `json:"pullRequest"`
			} `json:"repository"`
		}
		vars := map[string]any{"owner": opts.Owner, "name": opts.Name, "number": number, "after": cursor}
		if err := c.Query(ctx, q, vars, &resp); err != nil {
			return nil, err
		}
		page := resp.Repository.PullRequest[field]
		out = append(out, page.Nodes...)
		if !page.PageInfo.HasNextPage {
			return out, nil
		}
		cursor = page.PageInfo.EndCursor
	}
}

// LoadAll reads every fetched PR from dir/raw.
func LoadAll(dir string) ([]PullRequest, error) {
	files, err := filepath.Glob(filepath.Join(RawDir(dir), "page-*.jsonl"))
	if err != nil {
		return nil, err
	}
	var all []PullRequest
	for _, f := range files {
		prs, err := store.ReadJSONL[PullRequest](f)
		if err != nil {
			return nil, err
		}
		all = append(all, prs...)
	}
	return all, nil
}
