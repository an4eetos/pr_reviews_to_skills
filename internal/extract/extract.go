// Package extract is the map stage: one structured-output request per digest
// chunk, returning candidate rules whose evidence refs are validated against
// the digest so the model cannot cite comments that do not exist.
package extract

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"strings"

	"github.com/an4eetos/pr_reviews_to_skills/internal/digest"
	"github.com/an4eetos/pr_reviews_to_skills/internal/llm"
	"github.com/an4eetos/pr_reviews_to_skills/internal/rules"
	"github.com/an4eetos/pr_reviews_to_skills/prompts"
)

const (
	MaxTokens     = 32000
	maxQuoteRunes = 200
)

func Schema() json.RawMessage {
	str := map[string]any{"type": "string"}
	strList := map[string]any{"type": "array", "items": str}
	evidence := map[string]any{
		"type":                 "object",
		"additionalProperties": false,
		"required":             []string{"ref", "quote", "outcome"},
		"properties": map[string]any{
			"ref":     str,
			"quote":   str,
			"outcome": map[string]any{"type": "string", "enum": rules.Outcomes},
		},
	}
	candidate := map[string]any{
		"type":                 "object",
		"additionalProperties": false,
		"required": []string{"title", "statement", "rationale", "category", "kind", "applies_when",
			"paths", "languages", "bad_example", "good_example", "evidence"},
		"properties": map[string]any{
			"title":        str,
			"statement":    str,
			"rationale":    str,
			"category":     map[string]any{"type": "string", "enum": rules.Categories},
			"kind":         map[string]any{"type": "string", "enum": rules.Kinds},
			"applies_when": str,
			"paths":        strList,
			"languages":    strList,
			"bad_example":  str,
			"good_example": str,
			"evidence":     map[string]any{"type": "array", "items": evidence},
		},
	}
	s := map[string]any{
		"type":                 "object",
		"additionalProperties": false,
		"required":             []string{"candidates"},
		"properties": map[string]any{
			"candidates": map[string]any{"type": "array", "items": candidate},
		},
	}
	data, _ := json.Marshal(s)
	return data
}

// RequestID is content-addressed so a re-digest that changes a chunk's text
// never reuses a stale stored result.
func RequestID(prefix, content string) string {
	sum := sha256.Sum256([]byte(content))
	return prefix + "-" + hex.EncodeToString(sum[:])[:10]
}

func BuildRequests(repo string, chunks []digest.Chunk, effort string) []llm.Request {
	schema := Schema()
	reqs := make([]llm.Request, len(chunks))
	for i, c := range chunks {
		prompt := fmt.Sprintf("Repository: %s\n\n<discussions>\n%s</discussions>\n\nExtract the candidate rules from these discussions.", repo, c.Text)
		reqs[i] = llm.Request{
			ID:        RequestID(c.ID, prompt),
			System:    prompts.Extract,
			Prompt:    prompt,
			Schema:    schema,
			Effort:    effort,
			MaxTokens: MaxTokens,
		}
	}
	return reqs
}

type modelOutput struct {
	Candidates []struct {
		Title       string   `json:"title"`
		Statement   string   `json:"statement"`
		Rationale   string   `json:"rationale"`
		Category    string   `json:"category"`
		Kind        string   `json:"kind"`
		AppliesWhen string   `json:"applies_when"`
		Paths       []string `json:"paths"`
		Languages   []string `json:"languages"`
		BadExample  string   `json:"bad_example"`
		GoodExample string   `json:"good_example"`
		Evidence    []struct {
			Ref     string `json:"ref"`
			Quote   string `json:"quote"`
			Outcome string `json:"outcome"`
		} `json:"evidence"`
	} `json:"candidates"`
}

type Stats struct {
	Requests       int   `json:"requests"`
	Succeeded      int   `json:"succeeded"`
	Failed         int   `json:"failed"`
	Candidates     int   `json:"candidates"`
	DroppedNoProof int   `json:"dropped_no_evidence"`
	UnknownRefs    int   `json:"unknown_refs"`
	QuotesReplaced int   `json:"quotes_replaced"`
	InputTokens    int64 `json:"input_tokens"`
	OutputTokens   int64 `json:"output_tokens"`
}

// Parse turns model outputs into candidates. Evidence refs are resolved
// against the digest; unknown refs are dropped, quotes that do not occur in
// the cited comment are replaced with the comment's opening, and candidates
// left without evidence are discarded.
func Parse(reqs []llm.Request, results map[string]llm.Result, refs map[string]digest.RefInfo) ([]rules.Candidate, Stats) {
	st := Stats{Requests: len(reqs)}
	var out []rules.Candidate
	for _, q := range reqs {
		res, ok := results[q.ID]
		if !ok || !res.OK() {
			st.Failed++
			continue
		}
		st.InputTokens += res.InputTokens
		st.OutputTokens += res.OutputTokens
		var mo modelOutput
		if err := llm.Decode(res, &mo); err != nil {
			st.Failed++
			continue
		}
		st.Succeeded++
		chunkID := chunkOf(q.ID)
		for i, c := range mo.Candidates {
			cand := rules.Candidate{
				ID:          fmt.Sprintf("%s.%d", chunkID, i+1),
				Chunk:       chunkID,
				Title:       strings.TrimSpace(c.Title),
				Statement:   strings.TrimSpace(c.Statement),
				Rationale:   strings.TrimSpace(c.Rationale),
				Category:    c.Category,
				Kind:        c.Kind,
				AppliesWhen: strings.TrimSpace(c.AppliesWhen),
				Paths:       nonEmpty(c.Paths),
				Languages:   nonEmpty(c.Languages),
				BadExample:  c.BadExample,
				GoodExample: c.GoodExample,
			}
			if !slices.Contains(rules.Categories, cand.Category) {
				cand.Category = "process"
			}
			if !slices.Contains(rules.Kinds, cand.Kind) {
				cand.Kind = "do"
			}
			seen := map[string]bool{}
			for _, e := range c.Evidence {
				ref := strings.Trim(strings.TrimSpace(e.Ref), "[]")
				info, ok := refs[ref]
				if !ok {
					st.UnknownRefs++
					continue
				}
				if seen[ref] {
					continue
				}
				seen[ref] = true
				quote := strings.TrimSpace(e.Quote)
				if !containsNormalized(info.Comment.Body, quote) {
					st.QuotesReplaced++
					quote = truncate(info.Comment.Body, maxQuoteRunes)
				}
				outcome := e.Outcome
				if !slices.Contains(rules.Outcomes, outcome) {
					outcome = "unclear"
				}
				cand.Evidence = append(cand.Evidence, rules.Evidence{
					PR:       info.PR,
					Ref:      ref,
					URL:      info.Comment.URL,
					Author:   info.Comment.Author,
					Role:     info.Comment.Role,
					Date:     info.Comment.Date,
					Quote:    quote,
					Outcome:  outcome,
					ThumbsUp: info.Comment.ThumbsUp,
				})
			}
			if len(cand.Evidence) == 0 || cand.Statement == "" {
				st.DroppedNoProof++
				continue
			}
			out = append(out, cand)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	st.Candidates = len(out)
	return out, st
}

// chunkOf strips the content hash from a request ID.
func chunkOf(id string) string {
	if i := strings.LastIndex(id, "-"); i > 0 {
		return id[:i]
	}
	return id
}

func nonEmpty(xs []string) []string {
	out := []string{}
	for _, x := range xs {
		if x = strings.TrimSpace(x); x != "" {
			out = append(out, x)
		}
	}
	return out
}

func normalize(s string) string {
	return strings.ToLower(strings.Join(strings.Fields(s), " "))
}

func containsNormalized(body, quote string) bool {
	q := normalize(strings.Trim(quote, `"'.… `))
	if q == "" {
		return false
	}
	return strings.Contains(normalize(body), q)
}

func truncate(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}
