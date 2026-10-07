package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
	"github.com/anthropics/anthropic-sdk-go/shared/constant"
)

// Models that accept the server-side refusal fallback ("fallbacks": "default").
var fallbackModels = map[string]bool{
	"claude-fable-5-1":  true,
	"claude-opus-5-5":   true,
	"claude-opus-5":     true,
	"claude-sonnet-5-5": true,
}

type Anthropic struct {
	client    anthropic.Client
	model     string
	fallbacks bool
}

// NewAnthropic builds a client from the environment (ANTHROPIC_API_KEY or an
// `ant auth login` profile). fallbacks enables server-side refusal fallback
// on models that support it.
func NewAnthropic(model string, fallbacks bool) *Anthropic {
	return &Anthropic{
		client:    anthropic.NewClient(option.WithMaxRetries(6)),
		model:     model,
		fallbacks: fallbacks && fallbackModels[model],
	}
}

func (a *Anthropic) betas() []anthropic.AnthropicBeta {
	if a.fallbacks {
		return []anthropic.AnthropicBeta{anthropic.AnthropicBetaServerSideFallback2026_07_01}
	}
	return nil
}

func (a *Anthropic) fallbackParam() anthropic.BetaFallbacksParamUnion {
	if a.fallbacks {
		return anthropic.BetaFallbacksParamUnion{OfDefault: constant.ValueOf[constant.Default]()}
	}
	return anthropic.BetaFallbacksParamUnion{}
}

func system(req Request) []anthropic.BetaTextBlockParam {
	// The system prompt is frozen per stage, so cache it across requests.
	return []anthropic.BetaTextBlockParam{{
		Text:         req.System,
		CacheControl: anthropic.NewBetaCacheControlEphemeralParam(),
	}}
}

func messages(req Request) []anthropic.BetaMessageParam {
	return []anthropic.BetaMessageParam{anthropic.NewBetaUserMessage(anthropic.NewBetaTextBlock(req.Prompt))}
}

func outputConfig(req Request) anthropic.BetaOutputConfigParam {
	oc := anthropic.BetaOutputConfigParam{
		Format: anthropic.BetaJSONOutputFormatParam{Schema: req.Schema},
	}
	if req.Effort != "" {
		oc.Effort = anthropic.BetaOutputConfigEffort(req.Effort)
	}
	return oc
}

func (a *Anthropic) params(req Request) anthropic.BetaMessageNewParams {
	return anthropic.BetaMessageNewParams{
		Model:        a.model,
		MaxTokens:    req.MaxTokens,
		System:       system(req),
		Messages:     messages(req),
		OutputConfig: outputConfig(req),
		Fallbacks:    a.fallbackParam(),
		Betas:        a.betas(),
	}
}

// Complete streams the response (large max_tokens would otherwise risk HTTP
// timeouts) and returns the accumulated message.
func (a *Anthropic) Complete(ctx context.Context, req Request) (Result, error) {
	stream := a.client.Beta.Messages.NewStreaming(ctx, a.params(req))
	defer stream.Close()
	var msg anthropic.BetaMessage
	for stream.Next() {
		if err := msg.Accumulate(stream.Current()); err != nil {
			return Result{}, fmt.Errorf("accumulating stream: %w", err)
		}
	}
	if err := stream.Err(); err != nil {
		return Result{}, err
	}
	return toResult(req.ID, &msg), nil
}

func toResult(id string, msg *anthropic.BetaMessage) Result {
	var text strings.Builder
	for _, b := range msg.Content {
		if b.Type == "text" {
			text.WriteString(b.Text)
		}
	}
	r := Result{
		ID:           id,
		Text:         text.String(),
		Model:        string(msg.Model),
		StopReason:   string(msg.StopReason),
		InputTokens:  msg.Usage.InputTokens + msg.Usage.CacheCreationInputTokens + msg.Usage.CacheReadInputTokens,
		OutputTokens: msg.Usage.OutputTokens,
		CacheRead:    msg.Usage.CacheReadInputTokens,
		CompletedAt:  time.Now().UTC(),
	}
	switch msg.StopReason {
	case anthropic.BetaStopReasonEndTurn, anthropic.BetaStopReasonStopSequence:
	case anthropic.BetaStopReasonRefusal:
		r.Error = fmt.Sprintf("refused (category %q): %s", msg.StopDetails.Category, msg.StopDetails.Explanation)
	case anthropic.BetaStopReasonMaxTokens:
		r.Error = "output truncated at max_tokens"
	default:
		r.Error = fmt.Sprintf("unexpected stop_reason %q", msg.StopReason)
	}
	return r
}

func (a *Anthropic) SubmitBatch(ctx context.Context, reqs []Request) (string, error) {
	items := make([]anthropic.BetaMessageBatchNewParamsRequest, len(reqs))
	for i, req := range reqs {
		items[i] = anthropic.BetaMessageBatchNewParamsRequest{
			CustomID: req.ID,
			Params: anthropic.BetaMessageBatchNewParamsRequestParams{
				Model:        a.model,
				MaxTokens:    req.MaxTokens,
				System:       system(req),
				Messages:     messages(req),
				OutputConfig: outputConfig(req),
				Fallbacks:    a.fallbackParam(),
			},
		}
	}
	b, err := a.client.Beta.Messages.Batches.New(ctx, anthropic.BetaMessageBatchNewParams{
		Requests: items,
		Betas:    a.betas(),
	})
	if err != nil {
		return "", err
	}
	return b.ID, nil
}

func (a *Anthropic) BatchStatus(ctx context.Context, batchID string) (BatchStatus, error) {
	b, err := a.client.Beta.Messages.Batches.Get(ctx, batchID, anthropic.BetaMessageBatchGetParams{})
	if err != nil {
		return BatchStatus{}, err
	}
	c := b.RequestCounts
	return BatchStatus{
		Ended:      b.ProcessingStatus == anthropic.BetaMessageBatchProcessingStatusEnded,
		Processing: c.Processing,
		Succeeded:  c.Succeeded,
		Errored:    c.Errored,
		Expired:    c.Expired,
		Canceled:   c.Canceled,
	}, nil
}

func (a *Anthropic) BatchResults(ctx context.Context, batchID string) ([]Result, error) {
	stream := a.client.Beta.Messages.Batches.ResultsStreaming(ctx, batchID, anthropic.BetaMessageBatchResultsParams{})
	defer stream.Close()
	var out []Result
	for stream.Next() {
		item := stream.Current()
		switch item.Result.Type {
		case "succeeded":
			msg := item.Result.Message
			out = append(out, toResult(item.CustomID, &msg))
		case "errored":
			raw, _ := json.Marshal(item.Result.Error)
			out = append(out, Result{ID: item.CustomID, Error: "errored: " + string(raw)})
		default:
			out = append(out, Result{ID: item.CustomID, Error: item.Result.Type})
		}
	}
	return out, stream.Err()
}

func (a *Anthropic) CountTokens(ctx context.Context, req Request) (int64, error) {
	res, err := a.client.Beta.Messages.CountTokens(ctx, anthropic.BetaMessageCountTokensParams{
		Model:        a.model,
		System:       anthropic.BetaMessageCountTokensParamsSystemUnion{OfBetaTextBlockArray: system(req)},
		Messages:     messages(req),
		OutputConfig: outputConfig(req),
	})
	if err != nil {
		return 0, err
	}
	return res.InputTokens, nil
}
