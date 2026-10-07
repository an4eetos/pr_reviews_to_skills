package output

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/an4eetos/pr_reviews_to_skills/internal/rules"
)

var day = time.Date(2025, 6, 1, 0, 0, 0, 0, time.UTC)

func sampleRule(title string, tier rules.Tier, conf float64) rules.Rule {
	return rules.Rule{
		Title: title, Rule: title + ".", Category: "testing", Kind: "do", Tier: tier, Confidence: conf,
		AppliesWhen: "when testing",
		Evidence:    []rules.Evidence{{PR: 1, Ref: "1.1", URL: "u", Author: "a", Role: "maintainer", Date: day, Quote: "q", Outcome: "accepted"}},
		Members:     []string{"c1"},
	}
}

func TestFinalize(t *testing.T) {
	ev := func(ref, role, outcome string, thumbs int, d time.Time) rules.Evidence {
		return rules.Evidence{PR: 1, Ref: ref, URL: "u", Role: role, Outcome: outcome, ThumbsUp: thumbs, Date: d}
	}
	many := sampleRule("Use table tests!", rules.TierConsider, 0.9)
	many.Evidence = []rules.Evidence{
		ev("a", "contributor", "ignored", 0, day),
		ev("b", "maintainer", "accepted", 0, day),
		ev("c", "contributor", "accepted", 5, day),
		ev("d", "maintainer", "accepted", 0, day.AddDate(0, 1, 0)),
		ev("e", "contributor", "disputed", 0, day),
		ev("f", "contributor", "unclear", 0, day),
	}
	in := []rules.Rule{
		many,
		sampleRule("Use table tests", rules.TierGolden, 0.6),
		sampleRule("Old idea", rules.TierRejected, 0.9),
		sampleRule("Use  table tests", rules.TierGolden, 0.8),
	}
	out := Finalize(in, false)
	if len(out) != 3 {
		t.Fatalf("got %d rules, want rejected dropped", len(out))
	}
	ids := []string{out[0].ID, out[1].ID, out[2].ID}
	if !slices.Equal(ids, []string{"use-table-tests", "use-table-tests-2", "use-table-tests-3"}) {
		t.Errorf("ids %v", ids)
	}
	if out[0].Confidence != 0.8 || out[2].Tier != rules.TierConsider {
		t.Errorf("order: %+v", out)
	}
	top := out[2].Evidence
	var refs []string
	for _, e := range top {
		refs = append(refs, e.Ref)
	}
	if !slices.Equal(refs, []string{"d", "b", "c", "f", "a"}) {
		t.Errorf("evidence order %v", refs)
	}
	if out[0].Members != nil || out[0].Scope.Paths == nil {
		t.Errorf("intermediate fields not cleaned: %+v", out[0])
	}
	if got := len(Finalize(in, true)); got != 4 {
		t.Errorf("keepRejected kept %d", got)
	}
}

func TestSlug(t *testing.T) {
	if got := Slug("  Don't use `panic` in libraries!! "); got != "don-t-use-panic-in-libraries" {
		t.Errorf("Slug = %q", got)
	}
	long := Slug(strings.Repeat("abc ", 40))
	if len(long) > 60 || strings.HasSuffix(long, "-") {
		t.Errorf("long slug %q", long)
	}
}

func TestValidate(t *testing.T) {
	good := Report{SchemaVersion: SchemaVersion, Repo: "o/r", Rules: Finalize([]rules.Rule{sampleRule("A", rules.TierGolden, 0.9)}, false)}
	if err := Validate(good); err != nil {
		t.Fatalf("valid report rejected: %v", err)
	}
	bad := good
	bad.Rules = []rules.Rule{good.Rules[0], good.Rules[0]}
	bad.Rules[1].Tier = rules.TierConditional
	bad.Rules[1].AppliesWhen = ""
	bad.Rules[1].Confidence = 2
	err := Validate(bad)
	if err == nil {
		t.Fatal("invalid report accepted")
	}
	for _, want := range []string{"duplicate id", "out of [0,1]", "conditional rule without applies_when"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("missing %q in %v", want, err)
		}
	}
}

// TestSchemaMatchesOutput keeps schema/rules.schema.json and the Go structs
// in sync: every field the code emits is declared, and every required field
// is emitted.
func TestSchemaMatchesOutput(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "schema", "rules.schema.json"))
	if err != nil {
		t.Fatal(err)
	}
	var schema map[string]any
	if err := json.Unmarshal(raw, &schema); err != nil {
		t.Fatal(err)
	}
	r := sampleRule("A", rules.TierGolden, 0.9)
	r.Evidence[0].ThumbsUp = 1
	rep := Report{SchemaVersion: 1, Repo: "o/r", GeneratedAt: day, Model: "m",
		Stats: Stats{ByTier: map[string]int{}}, Rules: Finalize([]rules.Rule{r}, false)}
	data, _ := json.Marshal(rep)
	var doc map[string]any
	json.Unmarshal(data, &doc)

	rule := schema["$defs"].(map[string]any)["rule"].(map[string]any)
	ruleProps := rule["properties"].(map[string]any)
	check := func(name string, obj map[string]any, s map[string]any) {
		t.Helper()
		props := s["properties"].(map[string]any)
		for k := range obj {
			if _, ok := props[k]; !ok {
				t.Errorf("%s: field %q not in schema", name, k)
			}
		}
		for _, k := range s["required"].([]any) {
			if _, ok := obj[k.(string)]; !ok {
				t.Errorf("%s: required field %q not emitted", name, k)
			}
		}
	}
	check("report", doc, schema)
	check("stats", doc["stats"].(map[string]any), schema["properties"].(map[string]any)["stats"].(map[string]any))
	outRule := doc["rules"].([]any)[0].(map[string]any)
	check("rule", outRule, rule)
	check("metrics", outRule["metrics"].(map[string]any), ruleProps["metrics"].(map[string]any))
	check("scope", outRule["scope"].(map[string]any), ruleProps["scope"].(map[string]any))
	check("evidence", outRule["evidence"].([]any)[0].(map[string]any), ruleProps["evidence"].(map[string]any)["items"].(map[string]any))

	// Enums in the schema match the Go lists.
	enum := func(m map[string]any) []string {
		var out []string
		for _, v := range m["enum"].([]any) {
			out = append(out, v.(string))
		}
		return out
	}
	if !slices.Equal(enum(ruleProps["category"].(map[string]any)), rules.Categories) {
		t.Error("category enum out of sync")
	}
	if !slices.Equal(enum(ruleProps["kind"].(map[string]any)), rules.Kinds) {
		t.Error("kind enum out of sync")
	}
}
