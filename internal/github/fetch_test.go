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

	// A completed fetch is not repeated.
	n := len(f.requests)
	if _, err := c.Fetch(context.Background(), dir, FetchOptions{Owner: "o", Name: "r", States: []string{"MERGED"}, PageSize: 20}); err != nil {
		t.Fatal(err)
	}
	if len(f.requests) != n {
		t.Errorf("completed fetch re-queried GitHub")
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
