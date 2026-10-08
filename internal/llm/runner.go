package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/an4eetos/pr_reviews_to_skills/internal/store"
)

// Batch API limits are 100k requests / 256 MB per batch; stay well under.
const (
	maxBatchRequests = 5000
	maxBatchBytes    = 150 << 20
)

// Runner executes request sets and persists each successful Result as
// Dir/results/<id>.json. A request with a stored result is never re-sent, so
// any stage can be interrupted and re-run, in either mode.
type Runner struct {
	Client       Client
	Dir          string
	Concurrency  int
	PollInterval time.Duration
	Logf         func(format string, args ...any)
}

func (r *Runner) logf(format string, args ...any) {
	if r.Logf != nil {
		r.Logf(format, args...)
	}
}

func (r *Runner) resultPath(id string) string {
	return filepath.Join(r.Dir, "results", id+".json")
}

// Stored returns the persisted result for id, if any.
func (r *Runner) Stored(id string) (Result, bool) {
	var res Result
	ok, err := store.ReadJSON(r.resultPath(id), &res)
	if err != nil || !ok {
		return Result{}, false
	}
	return res, true
}

func (r *Runner) save(res Result) error {
	if !res.OK() {
		return nil
	}
	return store.WriteJSON(r.resultPath(res.ID), res)
}

func (r *Runner) split(reqs []Request) (done map[string]Result, pending []Request) {
	done = map[string]Result{}
	for _, q := range reqs {
		if res, ok := r.Stored(q.ID); ok {
			done[q.ID] = res
		} else {
			pending = append(pending, q)
		}
	}
	return done, pending
}

// RunSync sends pending requests directly with bounded concurrency. Failed
// requests come back with Error set; they are retried on the next run.
func (r *Runner) RunSync(ctx context.Context, reqs []Request) (map[string]Result, error) {
	results, pending := r.split(reqs)
	if len(pending) == 0 {
		return results, nil
	}
	r.logf("llm: %d requests (%d already done), concurrency %d", len(pending), len(results), max(r.Concurrency, 1))

	var (
		mu       sync.Mutex
		wg       sync.WaitGroup
		firstErr error
		finished int
	)
	sem := make(chan struct{}, max(r.Concurrency, 1))
	for _, q := range pending {
		if ctx.Err() != nil {
			break
		}
		sem <- struct{}{}
		wg.Add(1)
		go func(q Request) {
			defer wg.Done()
			defer func() { <-sem }()
			res, err := r.Client.Complete(ctx, q)
			if err != nil {
				res = Result{ID: q.ID, Error: err.Error()}
			}
			res.ID = q.ID
			saveErr := r.save(res)
			mu.Lock()
			defer mu.Unlock()
			results[q.ID] = res
			finished++
			if saveErr != nil && firstErr == nil {
				firstErr = saveErr
			}
			if !res.OK() {
				r.logf("llm: %s failed: %s", q.ID, res.Error)
			}
			if finished%10 == 0 || finished == len(pending) {
				r.logf("llm: %d/%d done", finished, len(pending))
			}
		}(q)
	}
	wg.Wait()
	if ctx.Err() != nil {
		return results, ctx.Err()
	}
	return results, firstErr
}

type batchRecord struct {
	ID          string    `json:"id"`
	RequestIDs  []string  `json:"request_ids"`
	SubmittedAt time.Time `json:"submitted_at"`
	Collected   bool      `json:"collected"`
}

type batchState struct {
	Batches []batchRecord `json:"batches"`
}

func (r *Runner) statePath() string { return filepath.Join(r.Dir, "batches.json") }

// ErrPending is returned by Collect when batches are still processing after
// maxWait; their IDs are saved, so a later Collect picks them up.
var ErrPending = errors.New("batches still processing")

// RunBatch submits pending requests through the Message Batches API, polls
// until every batch ends, and collects results. Batch IDs are persisted
// before polling, so a killed process resumes polling instead of
// resubmitting.
func (r *Runner) RunBatch(ctx context.Context, reqs []Request) (map[string]Result, error) {
	if _, err := r.Submit(ctx, reqs); err != nil {
		return nil, err
	}
	return r.Collect(ctx, reqs, 0)
}

// Submit sends every request that has no stored result and is not already
// in an uncollected batch. It returns how many requests it submitted.
func (r *Runner) Submit(ctx context.Context, reqs []Request) (int, error) {
	toSubmit, err := r.Unsent(reqs)
	if err != nil {
		return 0, err
	}
	st, err := r.loadState()
	if err != nil {
		return 0, err
	}
	for _, group := range groupForBatch(toSubmit) {
		id, err := r.Client.SubmitBatch(ctx, group)
		if err != nil {
			return 0, fmt.Errorf("submitting batch: %w", err)
		}
		rec := batchRecord{ID: id, SubmittedAt: time.Now().UTC()}
		for _, q := range group {
			rec.RequestIDs = append(rec.RequestIDs, q.ID)
		}
		st.Batches = append(st.Batches, rec)
		if err := store.WriteJSON(r.statePath(), st); err != nil {
			return 0, err
		}
		r.logf("llm: submitted batch %s with %d requests", id, len(group))
	}
	return len(toSubmit), nil
}

// Unsent returns the requests that have neither a stored result nor a place
// in an uncollected batch: what the next Submit or RunSync would pay for.
func (r *Runner) Unsent(reqs []Request) ([]Request, error) {
	_, pending := r.split(reqs)
	st, err := r.loadState()
	if err != nil {
		return nil, err
	}
	inflight := st.inflight()
	var out []Request
	for _, q := range pending {
		if !inflight[q.ID] {
			out = append(out, q)
		}
	}
	return out, nil
}

// InFlight reports how many submitted batches have not been collected yet.
func (r *Runner) InFlight() (int, error) {
	st, err := r.loadState()
	if err != nil {
		return 0, err
	}
	n := 0
	for _, b := range st.Batches {
		if !b.Collected {
			n++
		}
	}
	return n, nil
}

// Collect polls every uncollected batch, storing results as batches end,
// until none is left or maxWait (0 = no limit) runs out, in which case it
// returns what it has together with ErrPending. Only results for reqs are
// returned.
func (r *Runner) Collect(ctx context.Context, reqs []Request, maxWait time.Duration) (map[string]Result, error) {
	results, _ := r.split(reqs)
	st, err := r.loadState()
	if err != nil {
		return results, err
	}
	poll := r.PollInterval
	if poll <= 0 {
		poll = time.Minute
	}
	var deadline time.Time
	if maxWait > 0 {
		deadline = time.Now().Add(maxWait)
	}
	for {
		open := 0
		for i := range st.Batches {
			b := &st.Batches[i]
			if b.Collected {
				continue
			}
			status, err := r.Client.BatchStatus(ctx, b.ID)
			if err != nil {
				return results, fmt.Errorf("batch %s status: %w", b.ID, err)
			}
			if !status.Ended {
				open++
				r.logf("llm: batch %s: %d processing, %d succeeded, %d errored", b.ID, status.Processing, status.Succeeded, status.Errored)
				continue
			}
			batchResults, err := r.Client.BatchResults(ctx, b.ID)
			if err != nil {
				return results, fmt.Errorf("batch %s results: %w", b.ID, err)
			}
			failed := 0
			for _, res := range batchResults {
				if err := r.save(res); err != nil {
					return results, err
				}
				if !res.OK() {
					failed++
					r.logf("llm: %s failed: %s", res.ID, res.Error)
				}
				results[res.ID] = res
			}
			b.Collected = true
			if err := store.WriteJSON(r.statePath(), st); err != nil {
				return results, err
			}
			r.logf("llm: batch %s collected (%d results, %d failed)", b.ID, len(batchResults), failed)
		}
		if open == 0 {
			break
		}
		wait := poll
		if !deadline.IsZero() {
			left := time.Until(deadline)
			if left <= 0 {
				r.logf("llm: %d batch(es) still processing; the next run collects them", open)
				return onlyWanted(results, reqs), ErrPending
			}
			wait = min(wait, left)
		}
		t := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			t.Stop()
			return onlyWanted(results, reqs), ctx.Err()
		case <-t.C:
		}
	}
	return onlyWanted(results, reqs), nil
}

func onlyWanted(results map[string]Result, reqs []Request) map[string]Result {
	wanted := map[string]bool{}
	for _, q := range reqs {
		wanted[q.ID] = true
	}
	for id := range results {
		if !wanted[id] {
			delete(results, id)
		}
	}
	return results
}

func (r *Runner) loadState() (batchState, error) {
	var st batchState
	_, err := store.ReadJSON(r.statePath(), &st)
	return st, err
}

func (st batchState) inflight() map[string]bool {
	m := map[string]bool{}
	for _, b := range st.Batches {
		if !b.Collected {
			for _, id := range b.RequestIDs {
				m[id] = true
			}
		}
	}
	return m
}

func groupForBatch(reqs []Request) [][]Request {
	var groups [][]Request
	var cur []Request
	size := 0
	for _, q := range reqs {
		n := len(q.System) + len(q.Prompt) + len(q.Schema) + 512
		if len(cur) > 0 && (len(cur) >= maxBatchRequests || size+n > maxBatchBytes) {
			groups = append(groups, cur)
			cur, size = nil, 0
		}
		cur = append(cur, q)
		size += n
	}
	if len(cur) > 0 {
		groups = append(groups, cur)
	}
	return groups
}

// Failures lists failed results sorted by ID.
func Failures(results map[string]Result) []Result {
	var out []Result
	for _, r := range results {
		if !r.OK() {
			out = append(out, r)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// Decode unmarshals a successful result's JSON text into v.
func Decode(res Result, v any) error {
	if !res.OK() {
		return errors.New(res.Error)
	}
	if err := json.Unmarshal([]byte(res.Text), v); err != nil {
		return fmt.Errorf("%s: invalid JSON from model: %w", res.ID, err)
	}
	return nil
}

// Reset removes persisted results and batch state (used when inputs change).
func (r *Runner) Reset() error {
	return os.RemoveAll(r.Dir)
}
