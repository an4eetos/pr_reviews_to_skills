package github

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

var now = time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

type fakeGitHub struct {
	mu        sync.Mutex
	fail502   int // fail this many page queries with 502 first
	requests  []map[string]any
	remaining int
	prs       []map[string]any // newest first
}

func pr(number int, updated time.Time, threadsHasNext bool) map[string]any {
	comment := map[string]any{
		"id": "c", "url": "https://x/c", "body": "use errors.Is", "createdAt": updated, "diffHunk": "@@\n+x",
		"author":            map[string]any{"login": "alice", "__typename": "User"},
		"authorAssociation": "MEMBER",
		"thumbsUp":          map[string]any{"totalCount": 1},
		"thumbsDown":        map[string]any{"totalCount": 0},
	}
	thread := map[string]any{
		"id": "t", "path": "a.go", "line": 3, "isResolved": true, "isOutdated": false,
		"comments": map[string]any{"totalCount": 1, "nodes": []any{comment}},
	}
	empty := map[string]any{"totalCount": 0, "pageInfo": map[string]any{"hasNextPage": false, "endCursor": ""}, "nodes": []any{}}
	return map[string]any{
		"number": number, "title": "PR", "body": "", "url": "https://x/pr", "state": "MERGED", "merged": true,
		"createdAt": updated, "updatedAt": updated, "mergedAt": updated,
		"author": map[string]any{"login": "bob", "__typename": "User"}, "authorAssociation": "CONTRIBUTOR",
		"labels":  map[string]any{"nodes": []any{map[string]any{"name": "bug"}}},
		"files":   map[string]any{"totalCount": 1, "nodes": []any{map[string]any{"path": "a.go"}}},
		"reviews": empty,
		"reviewThreads": map[string]any{
			"totalCount": 2, "pageInfo": map[string]any{"hasNextPage": threadsHasNext, "endCursor": "t1"},
			"nodes": []any{thread},
		},
		"comments": empty,
	}
}

func (f *fakeGitHub) handler(t *testing.T) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "bearer tok" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		var body struct {
			Query     string         `json:"query"`
			Variables map[string]any `json:"variables"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		f.requests = append(f.requests, body.Variables)
		rl := map[string]any{"cost": 1, "remaining": f.remaining, "resetAt": now.Add(time.Minute)}

		if strings.Contains(body.Query, "pullRequests(") {
			if f.fail502 > 0 {
				f.fail502--
				w.WriteHeader(http.StatusBadGateway)
				return
			}
			start := 0
			if after, _ := body.Variables["after"].(string); after != "" {
				start = int(after[0] - '0')
			}
			first := int(body.Variables["first"].(float64))
			end := min(start+first, len(f.prs))
			hasNext := end < len(f.prs)
			cursor := string(rune('0' + end))
			writeJSON(w, map[string]any{"data": map[string]any{
				"rateLimit": rl,
				"repository": map[string]any{"pullRequests": map[string]any{
					"pageInfo": map[string]any{"hasNextPage": hasNext, "endCursor": cursor},
					"nodes":    f.prs[start:end],
				}},
			}})
			return
		}
		if strings.Contains(body.Query, "reviewThreads(first: 50, after: $after)") {
			thread := map[string]any{
				"id": "t2", "path": "b.go", "line": nil, "isResolved": false, "isOutdated": true,
				"comments": map[string]any{"totalCount": 0, "nodes": []any{}},
			}
			writeJSON(w, map[string]any{"data": map[string]any{
				"rateLimit": rl,
				"repository": map[string]any{"pullRequest": map[string]any{"reviewThreads": map[string]any{
					"pageInfo": map[string]any{"hasNextPage": false, "endCursor": ""},
					"nodes":    []any{thread},
				}}},
			}})
			return
		}
		t.Errorf("unexpected query: %s", body.Query)
	}
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

func newTestClient(srv *httptest.Server, slept *[]time.Duration) *Client {
	c := NewClient(srv.URL, "tok", nil)
	c.Now = func() time.Time { return now }
	c.Sleep = func(_ context.Context, d time.Duration) error {
		*slept = append(*slept, d)
		return nil
	}
	return c
}

func TestFetchPaginatesAndCompletesNested(t *testing.T) {
	f := &fakeGitHub{remaining: 5000, fail502: 3}
	for i := 0; i < 5; i++ {
		f.prs = append(f.prs, pr(100-i, now.Add(-time.Duration(i)*time.Hour), i == 0))
	}
	srv := httptest.NewServer(f.handler(t))
	defer srv.Close()
	var slept []time.Duration
	c := newTestClient(srv, &slept)
	dir := t.TempDir()

	st, err := c.Fetch(context.Background(), dir, FetchOptions{Owner: "o", Name: "r", States: []string{"MERGED"}, PageSize: 20})
	if err != nil {
		t.Fatal(err)
	}
	if !st.Done || st.PRs != 5 {
		t.Fatalf("state %+v", st)
	}
	// Three 502s exhaust the transient retries once; the page size then halves.
	if got := f.requests[3]["first"].(float64); got != 10 {
		t.Errorf("page size after 502s = %v, want 10", got)
	}
	prs, err := LoadAll(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(prs) != 5 {
		t.Fatalf("loaded %d PRs", len(prs))
	}
	first := prs[0]
	if first.Number != 100 || len(first.Threads) != 2 || first.Threads[1].Path != "b.go" || !first.Threads[1].IsOutdated {
		t.Errorf("nested threads not completed: %+v", first.Threads)
	}
	if c0 := first.Threads[0].Comments[0]; c0.ThumbsUp != 1 || c0.Author.Login != "alice" || c0.DiffHunk == "" {
		t.Errorf("comment not normalized: %+v", c0)
	}
	if first.Labels[0] != "bug" || first.Files[0] != "a.go" {
		t.Errorf("labels/files: %v %v", first.Labels, first.Files)
	}

	if !st.Watermark.Equal(now) {
		t.Errorf("watermark %v, want newest updatedAt %v", st.Watermark, now)
	}

	// A completed fetch refreshes: one page query that stops at the
	// watermark, and no new PRs stored.
	opts := FetchOptions{Owner: "o", Name: "r", States: []string{"MERGED"}, PageSize: 20}
	n := len(f.requests)
	st, err = c.Fetch(context.Background(), dir, opts)
	if err != nil {
		t.Fatal(err)
	}
	if got := len(f.requests) - n; got != 2 { // page + the newest PR's extra threads page
		t.Errorf("refresh made %d requests", got)
	}
	if st.Refresh != nil || st.LastRefresh.IsZero() {
		t.Errorf("refresh state not finished: %+v", st)
	}

	// New activity: PR 101 is new and PR 98 was updated. Only those two (plus
	// the PR sitting exactly at the old watermark) are fetched.
	f.mu.Lock()
	updated := pr(98, now.Add(2*time.Hour), false)
	f.prs = []map[string]any{updated, pr(101, now.Add(time.Hour), false), f.prs[0], f.prs[1], f.prs[3], f.prs[4]}
	f.mu.Unlock()
	pages := st.Pages
	st, err = c.Fetch(context.Background(), dir, opts)
	if err != nil {
		t.Fatal(err)
	}
	if st.Pages != pages+1 || !st.Watermark.Equal(now.Add(2*time.Hour)) {
		t.Fatalf("after refresh: %+v", st)
	}
	prs, _ = LoadAll(dir)
	var got []int
	for _, p := range prs[len(prs)-3:] {
		got = append(got, p.Number)
	}
	if len(prs) != 9 || got[0] != 98 || got[1] != 101 || got[2] != 100 {
		t.Errorf("refreshed PRs %v (total %d), want [98 101 100]", got, len(prs))
	}
}

func TestRefreshResumesAfterInterruption(t *testing.T) {
	f := &fakeGitHub{remaining: 5000}
	for i := 0; i < 4; i++ {
		f.prs = append(f.prs, pr(10-i, now.Add(-time.Duration(i)*time.Hour), false))
	}
	srv := httptest.NewServer(f.handler(t))
	defer srv.Close()
	var slept []time.Duration
	c := newTestClient(srv, &slept)
	dir := t.TempDir()
	opts := FetchOptions{Owner: "o", Name: "r", States: []string{"MERGED"}, PageSize: 2}
	if _, err := c.Fetch(context.Background(), dir, opts); err != nil {
		t.Fatal(err)
	}

	// Three new PRs arrive; the refresh is cancelled after its first page.
	f.mu.Lock()
	f.prs = append([]map[string]any{pr(13, now.Add(3*time.Hour), false), pr(12, now.Add(2*time.Hour), false), pr(11, now.Add(time.Hour), false)}, f.prs...)
	f.mu.Unlock()
	ctx, cancel := context.WithCancel(context.Background())
	c.Logf = func(format string, args ...any) {
		if strings.HasPrefix(format, "fetch: refresh: %d PRs so far") {
			cancel()
		}
	}
	if _, err := c.Fetch(ctx, dir, opts); err == nil {
		t.Fatal("want cancellation error")
	}
	st, _ := LoadState(dir)
	if st.Refresh == nil || st.Refresh.PRs != 2 || !st.Watermark.Equal(now) {
		t.Fatalf("interrupted state %+v", st)
	}
	c.Logf = nil
	st, err := c.Fetch(context.Background(), dir, opts)
	if err != nil {
		t.Fatal(err)
	}
	if st.Refresh != nil || !st.Watermark.Equal(now.Add(3*time.Hour)) {
		t.Errorf("resumed state %+v", st)
	}
	prs, _ := LoadAll(dir)
	seen := map[int]int{}
	for _, p := range prs {
		seen[p.Number]++
	}
	if seen[11] != 1 || seen[12] != 1 || seen[13] != 1 {
		t.Errorf("refreshed PRs %v", seen)
	}
}

func TestFetchSinceMaxPRsAndResume(t *testing.T) {
	f := &fakeGitHub{remaining: 5000}
	for i := 0; i < 6; i++ {
		f.prs = append(f.prs, pr(50-i, now.Add(-time.Duration(i)*24*time.Hour), false))
	}
	srv := httptest.NewServer(f.handler(t))
	defer srv.Close()
	var slept []time.Duration
	c := newTestClient(srv, &slept)
	dir := t.TempDir()
	opts := FetchOptions{Owner: "o", Name: "r", States: []string{"OPEN"}, PageSize: 2, Since: now.Add(-3*24*time.Hour - time.Hour)}

	opts.MaxPRs = 3
	st, err := c.Fetch(context.Background(), dir, opts)
	if err != nil {
		t.Fatal(err)
	}
	if st.PRs != 3 || st.Done {
		t.Fatalf("after max-prs: %+v", st)
	}

	opts.MaxPRs = 0
	st, err = c.Fetch(context.Background(), dir, opts)
	if err != nil {
		t.Fatal(err)
	}
	if !st.Done {
		t.Fatalf("not done: %+v", st)
	}
	prs, _ := LoadAll(dir)
	numbers := map[int]bool{}
	for _, p := range prs {
		numbers[p.Number] = true
	}
	// PRs 50..47 are within --since; 46 and 45 are older and stop the fetch.
	if len(numbers) != 4 || numbers[46] || numbers[45] {
		t.Errorf("fetched PRs %v, want 47-50", numbers)
	}

	// Changing options restarts from scratch.
	opts.States = []string{"OPEN", "CLOSED"}
	st, err = c.Fetch(context.Background(), dir, opts)
	if err != nil || st.Pages == 0 || !st.Done {
		t.Fatalf("refetch: %+v %v", st, err)
	}
}

func TestThrottleSleepsUntilReset(t *testing.T) {
	f := &fakeGitHub{remaining: 10}
	f.prs = []map[string]any{pr(1, now, false)}
	srv := httptest.NewServer(f.handler(t))
	defer srv.Close()
	var slept []time.Duration
	c := newTestClient(srv, &slept)
	if _, err := c.Fetch(context.Background(), t.TempDir(), FetchOptions{Owner: "o", Name: "r", States: []string{"OPEN"}}); err != nil {
		t.Fatal(err)
	}
	if len(slept) != 1 || slept[0] != time.Minute+5*time.Second {
		t.Errorf("slept %v, want one sleep of 1m5s", slept)
	}
}

func TestSecondaryRateLimitRetryAfter(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 {
			w.Header().Set("Retry-After", "7")
			w.WriteHeader(http.StatusForbidden)
			return
		}
		writeJSON(w, map[string]any{"data": map[string]any{"viewer": map[string]any{"login": "me"}}})
	}))
	defer srv.Close()
	var slept []time.Duration
	c := newTestClient(srv, &slept)
	var out struct {
		Viewer struct{ Login string }
	}
	if err := c.Query(context.Background(), "query { viewer { login } }", nil, &out); err != nil {
		t.Fatal(err)
	}
	if out.Viewer.Login != "me" || len(slept) != 1 || slept[0] != 8*time.Second {
		t.Errorf("login %q, slept %v", out.Viewer.Login, slept)
	}
}

func TestQueryReportsGraphQLErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"data": nil, "errors": []any{map[string]any{"type": "NOT_FOUND", "message": "Could not resolve to a Repository"}}})
	}))
	defer srv.Close()
	var slept []time.Duration
	c := newTestClient(srv, &slept)
	var out any
	err := c.Query(context.Background(), "query { x }", nil, &out)
	if err == nil || !strings.Contains(err.Error(), "Could not resolve") {
		t.Errorf("err = %v", err)
	}
}
