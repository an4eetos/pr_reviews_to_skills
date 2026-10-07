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

// RunBatch submits pending requests through the Message Batches API, polls
// until every batch ends, and collects results. Batch IDs are persisted
// before polling, so a killed process resumes polling instead of
// resubmitting.
func (r *Runner) RunBatch(ctx context.Context, reqs []Request) (map[string]Result, error) {
	results, pending := r.split(reqs)
	var st batchState
	if _, err := store.ReadJSON(r.statePath(), &st); err != nil {
		return results, err
	}

	inflight := map[string]bool{}
	for _, b := range st.Batches {
		if !b.Collected {
			for _, id := range b.RequestIDs {
				inflight[id] = true
			}
		}
	}
	var toSubmit []Request
	for _, q := range pending {
		if !inflight[q.ID] {
			toSubmit = append(toSubmit, q)
		}
	}

	for _, group := range groupForBatch(toSubmit) {
		id, err := r.Client.SubmitBatch(ctx, group)
		if err != nil {
			return results, fmt.Errorf("submitting batch: %w", err)
		}
		rec := batchRecord{ID: id, SubmittedAt: time.Now().UTC()}
		for _, q := range group {
			rec.RequestIDs = append(rec.RequestIDs, q.ID)
		}
		st.Batches = append(st.Batches, rec)
		if err := store.WriteJSON(r.statePath(), st); err != nil {
			return results, err
		}
		r.logf("llm: submitted batch %s with %d requests", id, len(group))
	}

	poll := r.PollInterval
	if poll <= 0 {
		poll = time.Minute
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
				// Only report results for requests in this run's set.
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
		t := time.NewTimer(poll)
		select {
		case <-ctx.Done():
			t.Stop()
			return results, ctx.Err()
		case <-t.C:
		}
	}

	wanted := map[string]bool{}
	for _, q := range reqs {
		wanted[q.ID] = true
	}
	for id := range results {
		if !wanted[id] {
			delete(results, id)
		}
	}
	return results, nil
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
