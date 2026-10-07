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
}

const stateFile = "fetch_state.json"

func RawDir(dir string) string { return filepath.Join(dir, "raw") }

// Fetch pages through the repo's PRs (most recently updated first) into
// dir/raw/page-NNNNN.jsonl, resuming from dir/fetch_state.json.
func (c *Client) Fetch(ctx context.Context, dir string, opts FetchOptions) (FetchState, error) {
	statePath := filepath.Join(dir, stateFile)
	var st FetchState
	if _, err := store.ReadJSON(statePath, &st); err != nil {
		return st, err
	}
	if st.Key != opts.key() {
		if st.Key != "" {
			c.logf("fetch: options changed since the last fetch, starting over")
		}
		if err := os.RemoveAll(RawDir(dir)); err != nil {
			return st, err
		}
		st = FetchState{Key: opts.key()}
	}
	if st.Done {
		c.logf("fetch: already complete (%d PRs in %d pages); delete %s to refetch", st.PRs, st.Pages, statePath)
		return st, nil
	}
	if opts.MaxPRs > 0 && st.PRs >= opts.MaxPRs {
		c.logf("fetch: already have %d PRs (--max-prs %d)", st.PRs, opts.MaxPRs)
		return st, nil
	}
	if st.PRs > 0 {
		c.logf("fetch: resuming after %d PRs", st.PRs)
	}

	pageSize := opts.PageSize
	if pageSize <= 0 {
		pageSize = 25
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
			"first":  pageSize,
			"states": opts.States,
			"after":  nil,
		}
		if st.Cursor != "" {
			vars["after"] = st.Cursor
		}
		err := c.Query(ctx, pageQuery, vars, &resp)
		var te *TransientError
		if errors.As(err, &te) && pageSize > minPageSize {
			pageSize = max(pageSize/2, minPageSize)
			c.logf("fetch: GitHub timing out, shrinking page size to %d", pageSize)
			continue
		}
		if err != nil {
			return st, err
		}
		if resp.Repository == nil {
			return st, fmt.Errorf("repository %s/%s not found or not accessible with this token", opts.Owner, opts.Name)
		}
		page := resp.Repository.PullRequests

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
			if opts.MaxPRs > 0 && st.PRs+len(prs) >= opts.MaxPRs {
				stopMax = true
				break
			}
		}

		if len(prs) > 0 {
			path := filepath.Join(RawDir(dir), fmt.Sprintf("page-%05d.jsonl", st.Pages+1))
			if err := store.WriteJSONL(path, prs); err != nil {
				return st, err
			}
			st.Pages++
			st.PRs += len(prs)
		}
		// When we stop mid-page for --max-prs, keep the old cursor: a later run
		// with a higher limit refetches this page and digest dedupes by number.
		if !stopMax {
			st.Cursor = page.PageInfo.EndCursor
		}
		st.Done = stopSince || !page.PageInfo.HasNextPage
		st.UpdatedAt = time.Now().UTC()
		if err := store.WriteJSON(statePath, st); err != nil {
			return st, err
		}
		c.logf("fetch: %d PRs so far (page %d)", st.PRs, st.Pages)
		if st.Done || stopMax {
			return st, nil
		}
		// Recover the page size gradually after a timeout spell.
		if pageSize < opts.PageSize {
			pageSize = min(pageSize*2, opts.PageSize)
		}
	}
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
