package extract

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/an4eetos/pr_reviews_to_skills/internal/digest"
	"github.com/an4eetos/pr_reviews_to_skills/internal/llm"
)

func TestSchemaIsValidJSON(t *testing.T) {
	var v map[string]any
	if err := json.Unmarshal(Schema(), &v); err != nil {
		t.Fatal(err)
	}
	if v["additionalProperties"] != false {
		t.Error("structured outputs need additionalProperties: false")
	}
}

func TestBuildRequestsContentAddressed(t *testing.T) {
	chunks := []digest.Chunk{{ID: "chunk-00001", Text: "<pr>a</pr>\n"}}
	a := BuildRequests("o/r", chunks, "medium")
	chunks[0].Text = "<pr>b</pr>\n"
	b := BuildRequests("o/r", chunks, "medium")
	if a[0].ID == b[0].ID || !strings.HasPrefix(a[0].ID, "chunk-00001-") {
		t.Errorf("ids %q %q", a[0].ID, b[0].ID)
	}
	if !strings.Contains(a[0].Prompt, "<discussions>\n<pr>a</pr>") || a[0].System == "" {
		t.Errorf("prompt %q", a[0].Prompt)
	}
}

func TestParse(t *testing.T) {
	date := time.Date(2025, 1, 2, 0, 0, 0, 0, time.UTC)
	refs := map[string]digest.RefInfo{
		"812.1": {PR: 812, Comment: digest.Comment{Ref: "812.1", URL: "u1", Author: "alice", Role: "maintainer", Date: date,
			Body: "Please wrap this error with context:\nreturn fmt.Errorf(\"load: %w\", err)"}},
		"815.2": {PR: 815, Comment: digest.Comment{Ref: "815.2", URL: "u2", Author: "bob", Role: "contributor", Date: date,
			Body: "Same here, wrap errors."}},
	}
	out := `{"candidates":[
	  {"title":"Wrap errors","statement":"Wrap errors with %w.","rationale":"context","category":"error-handling","kind":"do",
	   "applies_when":"","paths":["internal/**"," "],"languages":["go"],"bad_example":"","good_example":"",
	   "evidence":[
	     {"ref":"812.1","quote":"Please wrap this   error with context","outcome":"accepted"},
	     {"ref":"[815.2]","quote":"an invented quote","outcome":"accepted"},
	     {"ref":"812.1","quote":"dup","outcome":"accepted"},
	     {"ref":"999.9","quote":"x","outcome":"accepted"}]},
	  {"title":"Ghost","statement":"Cites nothing real","rationale":"","category":"style","kind":"do",
	   "applies_when":"","paths":[],"languages":[],"bad_example":"","good_example":"",
	   "evidence":[{"ref":"1.1","quote":"x","outcome":"accepted"}]}
	]}`
	reqs := []llm.Request{{ID: "chunk-00001-abc"}, {ID: "chunk-00002-def"}}
	results := map[string]llm.Result{
		"chunk-00001-abc": {ID: "chunk-00001-abc", Text: out, InputTokens: 100, OutputTokens: 10},
		"chunk-00002-def": {ID: "chunk-00002-def", Error: "refused"},
	}
	cands, st := Parse(reqs, results, refs)
	if st.Succeeded != 1 || st.Failed != 1 || st.UnknownRefs != 2 || st.QuotesReplaced != 1 || st.DroppedNoProof != 1 {
		t.Errorf("stats %+v", st)
	}
	if len(cands) != 1 {
		t.Fatalf("got %d candidates", len(cands))
	}
	c := cands[0]
	if c.ID != "chunk-00001.1" || c.Chunk != "chunk-00001" {
		t.Errorf("ids %q %q", c.ID, c.Chunk)
	}
	if len(c.Paths) != 1 || c.Paths[0] != "internal/**" {
		t.Errorf("paths %q", c.Paths)
	}
	if len(c.Evidence) != 2 {
		t.Fatalf("evidence %+v", c.Evidence)
	}
	e0, e1 := c.Evidence[0], c.Evidence[1]
	if e0.PR != 812 || e0.URL != "u1" || e0.Role != "maintainer" || e0.Quote != "Please wrap this   error with context" {
		t.Errorf("evidence 0 %+v", e0)
	}
	if e1.Ref != "815.2" || e1.Quote != "Same here, wrap errors." {
		t.Errorf("fabricated quote not replaced: %+v", e1)
	}
}
