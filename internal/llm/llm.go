// Package llm wraps the Claude API behind a small interface so the pipeline
// stages can be tested with a fake, and runs request sets either directly
// (concurrent streaming calls) or through the Message Batches API, persisting
// every successful result so interrupted runs resume without re-paying.
package llm

import (
	"context"
	"encoding/json"
	"time"
)

// Request is one structured-output call: a frozen system prompt, a user
// prompt, and the JSON schema the reply must satisfy.
type Request struct {
	ID        string          `json:"id"`
	System    string          `json:"-"`
	Prompt    string          `json:"-"`
	Schema    json.RawMessage `json:"-"`
	Effort    string          `json:"-"`
	MaxTokens int64           `json:"-"`
}

// Result is a completed call. Error is set (and Text unusable) when the call
// failed, was refused, or was cut off by max_tokens.
type Result struct {
	ID           string    `json:"id"`
	Text         string    `json:"text"`
	Model        string    `json:"model"`
	StopReason   string    `json:"stop_reason"`
	InputTokens  int64     `json:"input_tokens"`
	OutputTokens int64     `json:"output_tokens"`
	CacheRead    int64     `json:"cache_read_input_tokens"`
	Error        string    `json:"error,omitempty"`
	CompletedAt  time.Time `json:"completed_at"`
}

func (r Result) OK() bool { return r.Error == "" }

type BatchStatus struct {
	Ended      bool
	Processing int64
	Succeeded  int64
	Errored    int64
	Expired    int64
	Canceled   int64
}

// Client is the subset of the Claude API the pipeline uses.
type Client interface {
	Complete(ctx context.Context, req Request) (Result, error)
	SubmitBatch(ctx context.Context, reqs []Request) (string, error)
	BatchStatus(ctx context.Context, batchID string) (BatchStatus, error)
	BatchResults(ctx context.Context, batchID string) ([]Result, error)
	CountTokens(ctx context.Context, req Request) (int64, error)
}
