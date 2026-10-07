package pipeline

import (
	"fmt"
	"strings"

	"github.com/an4eetos/pr_reviews_to_skills/internal/digest"
	"github.com/an4eetos/pr_reviews_to_skills/internal/llm"
)

// USD per million tokens (input, output), first-party API list prices.
var pricing = map[string][2]float64{
	"claude-fable-5-1":  {10, 50},
	"claude-opus-5-5":   {4, 20},
	"claude-opus-5":     {5, 25},
	"claude-sonnet-5-5": {2, 10},
	"claude-sonnet-5":   {2, 10},
	"claude-haiku-4-5":  {1, 5},
}

const (
	// Rough output per extraction request, thinking included.
	outputPerExtract = 5000
	// Synthesis (merge + score over the candidates) relative to extraction;
	// it reads compact candidates, not raw discussions.
	synthShare = 0.15
)

type Estimate struct {
	Model        string
	Requests     int
	InputTokens  int64
	OutputTokens int64
	ExtractUSD   float64
	SynthUSD     float64
	Batch        bool
	KnownPrice   bool
}

func (e Estimate) Total() float64 { return e.ExtractUSD + e.SynthUSD }

func estimate(model string, reqs []llm.Request, ratio float64, batch bool) Estimate {
	e := Estimate{Model: model, Requests: len(reqs), Batch: batch}
	for _, q := range reqs {
		e.InputTokens += int64(float64(digest.EstimateTokens(q.System+q.Prompt+string(q.Schema))) * ratio)
	}
	e.OutputTokens = int64(len(reqs)) * outputPerExtract
	price, ok := pricing[model]
	e.KnownPrice = ok
	if !ok {
		return e
	}
	in := float64(e.InputTokens) / 1e6 * price[0]
	out := float64(e.OutputTokens) / 1e6 * price[1]
	e.ExtractUSD = in + out
	if batch {
		e.ExtractUSD *= 0.5
	}
	// Synthesis always runs as direct calls (few, large, streamed).
	e.SynthUSD = (in + out) * synthShare
	return e
}

func (e Estimate) String() string {
	var b strings.Builder
	fmt.Fprintf(&b, "\nCost estimate (%s):\n", e.Model)
	mode := "direct calls"
	if e.Batch {
		mode = "Batch API, 50% off"
	}
	fmt.Fprintf(&b, "  extract:    %d requests, ~%s input + ~%s output tokens (%s)\n",
		e.Requests, human(e.InputTokens), human(e.OutputTokens), mode)
	if !e.KnownPrice {
		fmt.Fprintf(&b, "  no price table entry for %s; token counts only\n\n", e.Model)
		return b.String()
	}
	fmt.Fprintf(&b, "              ~$%.2f\n", e.ExtractUSD)
	fmt.Fprintf(&b, "  synthesize: ~$%.2f (rough)\n", e.SynthUSD)
	fmt.Fprintf(&b, "  total:      ~$%.2f  (estimate; output length varies, prompt caching may lower it)\n\n", e.Total())
	return b.String()
}

func human(n int64) string {
	switch {
	case n >= 1_000_000:
		return fmt.Sprintf("%.1fM", float64(n)/1e6)
	case n >= 1_000:
		return fmt.Sprintf("%.0fk", float64(n)/1e3)
	default:
		return fmt.Sprint(n)
	}
}
