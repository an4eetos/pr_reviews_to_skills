package llm

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/an4eetos/pr_reviews_to_skills/internal/store"
)

type fakeClient struct {
	mu        sync.Mutex
	completed []string
	failIDs   map[string]bool
	batches   map[string][]Request
	polls     map[string]int
	submitted int
}

func (f *fakeClient) result(q Request) Result {
	if f.failIDs[q.ID] {
		return Result{ID: q.ID, Error: "refused"}
	}
	return Result{ID: q.ID, Text: fmt.Sprintf(`{"echo":%q}`, q.ID), StopReason: "end_turn"}
}

func (f *fakeClient) Complete(_ context.Context, q Request) (Result, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.completed = append(f.completed, q.ID)
	if q.ID == "boom" {
		return Result{}, errors.New("network down")
	}
	return f.result(q), nil
}

func (f *fakeClient) SubmitBatch(_ context.Context, reqs []Request) (string, error) {
	f.submitted++
	id := fmt.Sprintf("batch_%d", f.submitted)
	f.batches[id] = reqs
	return id, nil
}

func (f *fakeClient) BatchStatus(_ context.Context, id string) (BatchStatus, error) {
	f.polls[id]++
	if f.polls[id] < 2 {
		return BatchStatus{Processing: int64(len(f.batches[id]))}, nil
	}
	return BatchStatus{Ended: true, Succeeded: int64(len(f.batches[id]))}, nil
}

func (f *fakeClient) BatchResults(_ context.Context, id string) ([]Result, error) {
	var out []Result
	for _, q := range f.batches[id] {
		out = append(out, f.result(q))
	}
	return out, nil
}

func (f *fakeClient) CountTokens(context.Context, Request) (int64, error) { return 0, nil }

func newFake() *fakeClient {
	return &fakeClient{failIDs: map[string]bool{}, batches: map[string][]Request{}, polls: map[string]int{}}
}

func reqs(ids ...string) []Request {
	out := make([]Request, len(ids))
	for i, id := range ids {
		out[i] = Request{ID: id, Prompt: "p"}
	}
	return out
}

func TestRunSyncPersistsAndSkipsDone(t *testing.T) {
	f := newFake()
	f.failIDs["c"] = true
	r := &Runner{Client: f, Dir: t.TempDir(), Concurrency: 3}

	res, err := r.RunSync(context.Background(), reqs("a", "b", "c", "boom"))
	if err != nil {
		t.Fatal(err)
	}
	if !res["a"].OK() || !res["b"].OK() || res["c"].OK() || res["boom"].Error != "network down" {
		t.Fatalf("results %+v", res)
	}
	if fails := Failures(res); len(fails) != 2 || fails[0].ID != "boom" || fails[1].ID != "c" {
		t.Errorf("failures %+v", fails)
	}
	var v struct{ Echo string }
	if err := Decode(res["a"], &v); err != nil || v.Echo != "a" {
		t.Errorf("decode: %v %+v", err, v)
	}

	// Second run only re-sends the failures.
	f.completed = nil
	delete(f.failIDs, "c")
	res, err = r.RunSync(context.Background(), reqs("a", "b", "c", "boom"))
	if err != nil {
		t.Fatal(err)
	}
	if len(f.completed) != 2 || !res["c"].OK() || !res["a"].OK() {
		t.Errorf("re-run sent %v; results %+v", f.completed, res)
	}
}

func TestRunBatchSubmitsPollsAndResumes(t *testing.T) {
	f := newFake()
	f.failIDs["y"] = true
	dir := t.TempDir()
	r := &Runner{Client: f, Dir: dir, PollInterval: time.Millisecond}

	res, err := r.RunBatch(context.Background(), reqs("x", "y", "z"))
	if err != nil {
		t.Fatal(err)
	}
	if f.submitted != 1 || f.polls["batch_1"] != 2 {
		t.Errorf("submitted %d, polls %v", f.submitted, f.polls)
	}
	if !res["x"].OK() || res["y"].OK() || !res["z"].OK() {
		t.Errorf("results %+v", res)
	}

	// Resume: a batch recorded but not yet collected (process killed while
	// polling) is polled again rather than resubmitted.
	f2 := newFake()
	f2.batches["batch_9"] = reqs("p", "q")
	r2 := &Runner{Client: f2, Dir: t.TempDir(), PollInterval: time.Millisecond}
	if err := writeState(r2, batchState{Batches: []batchRecord{{ID: "batch_9", RequestIDs: []string{"p", "q"}}}}); err != nil {
		t.Fatal(err)
	}
	res, err = r2.RunBatch(context.Background(), reqs("p", "q", "s"))
	if err != nil {
		t.Fatal(err)
	}
	if f2.submitted != 1 || len(f2.batches["batch_1"]) != 1 || f2.batches["batch_1"][0].ID != "s" {
		t.Errorf("resume resubmitted in-flight requests: %+v", f2.batches)
	}
	if !res["p"].OK() || !res["q"].OK() || !res["s"].OK() {
		t.Errorf("results %+v", res)
	}

	// Everything stored: nothing is submitted at all.
	f2.submitted = 0
	if _, err := r2.RunBatch(context.Background(), reqs("p", "q", "s")); err != nil {
		t.Fatal(err)
	}
	if f2.submitted != 0 {
		t.Errorf("submitted %d batches with all results stored", f2.submitted)
	}
}

func TestCollectMaxWaitLeavesBatchesPending(t *testing.T) {
	f := newFake()
	r := &Runner{Client: f, Dir: t.TempDir(), PollInterval: time.Hour}
	if n, err := r.Submit(context.Background(), reqs("a", "b")); err != nil || n != 2 {
		t.Fatalf("submit: %d %v", n, err)
	}
	f.polls["batch_1"] = -100 // keep it processing for a while
	// The first poll sees the batch processing; maxWait runs out before the
	// next one instead of sleeping the full poll interval.
	start := time.Now()
	res, err := r.Collect(context.Background(), reqs("a", "b"), 10*time.Millisecond)
	if !errors.Is(err, ErrPending) || len(res) != 0 || time.Since(start) > time.Second {
		t.Fatalf("collect: %v %v after %s", res, err, time.Since(start))
	}
	if n, _ := r.InFlight(); n != 1 {
		t.Errorf("in flight %d", n)
	}
	// The next run collects without resubmitting.
	f.polls["batch_1"] = 1
	if n, err := r.Submit(context.Background(), reqs("a", "b")); err != nil || n != 0 {
		t.Fatalf("resubmitted %d %v", n, err)
	}
	res, err = r.Collect(context.Background(), reqs("a", "b"), 10*time.Millisecond)
	if err != nil || !res["a"].OK() || !res["b"].OK() || f.submitted != 1 {
		t.Errorf("second collect: %+v %v, submitted %d", res, err, f.submitted)
	}
	if n, _ := r.InFlight(); n != 0 {
		t.Errorf("in flight %d after collect", n)
	}
}

func writeState(r *Runner, st batchState) error {
	return store.WriteJSON(r.statePath(), st)
}

func TestGroupForBatch(t *testing.T) {
	var many []Request
	for i := 0; i < maxBatchRequests+10; i++ {
		many = append(many, Request{ID: fmt.Sprint(i)})
	}
	groups := groupForBatch(many)
	if len(groups) != 2 || len(groups[0]) != maxBatchRequests || len(groups[1]) != 10 {
		t.Errorf("groups of %d and %d", len(groups[0]), len(groups[len(groups)-1]))
	}
}
