// Package rules holds the domain types shared by the extract, synthesize and
// output stages: candidate rules mined from single chunks, merged rules, their
// evidence metrics and tiers.
package rules

import "time"

// Categories a rule can belong to. Kept in sync with the JSON schemas in the
// prompts and schema/rules.schema.json.
var Categories = []string{
	"architecture",
	"style",
	"naming",
	"correctness",
	"error-handling",
	"security",
	"performance",
	"testing",
	"api-design",
	"concurrency",
	"process",
	"recurring-mistake",
}

// Kinds says whether a rule asks for something, forbids it, or prefers it.
var Kinds = []string{"do", "dont", "prefer"}

// Outcomes describe what happened to a review comment cited as evidence.
var Outcomes = []string{"accepted", "disputed", "ignored", "unclear"}

type Tier string

const (
	TierGolden      Tier = "golden"
	TierConditional Tier = "conditional"
	TierConsider    Tier = "consider"
	TierRejected    Tier = "rejected"
)

var Tiers = []Tier{TierGolden, TierConditional, TierConsider, TierRejected}

// Rank orders tiers for sorting: golden first, rejected last.
func (t Tier) Rank() int {
	for i, x := range Tiers {
		if x == t {
			return i
		}
	}
	return len(Tiers)
}

// Evidence is one review comment backing (or opposing) a rule, resolved from
// the comment ref the model cited.
type Evidence struct {
	PR       int       `json:"pr"`
	Ref      string    `json:"ref"`
	URL      string    `json:"url"`
	Author   string    `json:"author"`
	Role     string    `json:"role"`
	Date     time.Time `json:"date"`
	Quote    string    `json:"quote"`
	Outcome  string    `json:"outcome"`
	ThumbsUp int       `json:"thumbs_up,omitempty"`
	// Repo is set in combined (multi-repo) rule sets only.
	Repo string `json:"repo,omitempty"`
}

// Candidate is a rule as extracted from a single chunk of PR discussions,
// before cross-chunk merging.
type Candidate struct {
	ID          string     `json:"id"`
	Chunk       string     `json:"chunk"`
	Repo        string     `json:"repo,omitempty"`
	Title       string     `json:"title"`
	Statement   string     `json:"statement"`
	Rationale   string     `json:"rationale"`
	Category    string     `json:"category"`
	Kind        string     `json:"kind"`
	AppliesWhen string     `json:"applies_when"`
	Paths       []string   `json:"paths"`
	Languages   []string   `json:"languages"`
	BadExample  string     `json:"bad_example"`
	GoodExample string     `json:"good_example"`
	Evidence    []Evidence `json:"evidence"`
}

// Metrics are computed deterministically from a rule's evidence; the scoring
// model sees them and the tier floors are enforced against them.
type Metrics struct {
	DistinctPRs        int       `json:"distinct_prs"`
	DistinctRepos      int       `json:"distinct_repos,omitempty"`
	DistinctReviewers  int       `json:"distinct_reviewers"`
	MaintainerEndorsed bool      `json:"maintainer_endorsed"`
	Accepted           int       `json:"accepted"`
	Disputed           int       `json:"disputed"`
	Ignored            int       `json:"ignored"`
	Opposing           int       `json:"opposing"`
	ThumbsUp           int       `json:"thumbs_up"`
	FirstSeen          time.Time `json:"first_seen"`
	LastSeen           time.Time `json:"last_seen"`
	RecentShare        float64   `json:"recent_share"`
}

// Rule is a merged, scored rule as written to rules.json.
type Rule struct {
	ID          string     `json:"id"`
	Title       string     `json:"title"`
	Rule        string     `json:"rule"`
	Rationale   string     `json:"rationale"`
	Category    string     `json:"category"`
	Kind        string     `json:"kind"`
	Tier        Tier       `json:"tier"`
	Confidence  float64    `json:"confidence"`
	TierReason  string     `json:"tier_reason"`
	AppliesWhen string     `json:"applies_when"`
	Scope       Scope      `json:"scope"`
	Repos       []string   `json:"repos,omitempty"`
	Examples    Examples   `json:"examples"`
	Metrics     Metrics    `json:"metrics"`
	Evidence    []Evidence `json:"evidence"`
	// Members and Opposing are the candidate IDs merged into this rule; kept in
	// the intermediate drafts file for traceability, not in rules.json.
	Members  []string `json:"members,omitempty"`
	Opposing []string `json:"opposing,omitempty"`
}

type Scope struct {
	Paths     []string `json:"paths"`
	Languages []string `json:"languages"`
}

type Examples struct {
	Bad  string `json:"bad"`
	Good string `json:"good"`
}
