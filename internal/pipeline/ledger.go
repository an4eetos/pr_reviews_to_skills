package pipeline

import (
	"fmt"
	"os"
	"time"

	"github.com/an4eetos/pr_reviews_to_skills/internal/digest"
	"github.com/an4eetos/pr_reviews_to_skills/internal/rules"
	"github.com/an4eetos/pr_reviews_to_skills/internal/store"
)

// Generations make runs incremental. Each digest packs only the PRs whose
// discussion is new or changed since the last sealed generation into a new
// generation with its own chunks, units snapshot and (once extracted)
// candidates:
//
//	open       rebuilt by every digest until its requests are sent
//	submitted  sealed: its PRs are claimed, later changes go to a new generation
//	extracted  candidates written; synthesis folds them in
const (
	genOpen      = "open"
	genSubmitted = "submitted"
	genExtracted = "extracted"
)

// maxExtractAttempts bounds how often a generation with failing requests is
// retried before it is accepted without them.
const maxExtractAttempts = 3

type Gen struct {
	ID     int    `json:"id"`
	Status string `json:"status"`
	// PRs maps each PR in the generation to its content hash.
	PRs         map[int]string `json:"prs"`
	Chunks      int            `json:"chunks"`
	EstTokens   int            `json:"est_tokens"`
	Candidates  int            `json:"candidates"`
	Attempts    int            `json:"attempts,omitempty"`
	CreatedAt   time.Time      `json:"created_at"`
	ExtractedAt time.Time      `json:"extracted_at,omitzero"`
}

// Key names the generation in the synthesis state.
func (g *Gen) Key() string { return fmt.Sprintf("g%04d", g.ID) }

type Ledger struct {
	// PRs maps each PR to the content hash of its latest sealed generation.
	PRs  map[int]string `json:"prs"`
	Gens []*Gen         `json:"gens"`
}

func (p *Pipeline) genDir(id int, parts ...string) string {
	return p.dir(append([]string{"gens", fmt.Sprintf("g%04d", id)}, parts...)...)
}

func (p *Pipeline) loadLedger() (*Ledger, error) {
	l := &Ledger{}
	ok, err := store.ReadJSON(p.dir("ledger.json"), l)
	if err != nil {
		return nil, err
	}
	if l.PRs == nil {
		l.PRs = map[int]string{}
	}
	if !ok {
		if err := p.migrate(l); err != nil {
			return nil, fmt.Errorf("migrating cache to generations: %w", err)
		}
	}
	return l, nil
}

func (p *Pipeline) saveLedger(l *Ledger) error {
	return store.WriteJSON(p.dir("ledger.json"), l)
}

// migrate adopts a cache written before generations existed: its extracted
// candidates become generation 0, so an upgraded install doesn't pay for the
// same PRs again.
func (p *Pipeline) migrate(l *Ledger) error {
	if !store.Exists(p.dir("candidates.jsonl")) || !store.Exists(p.dir("units.jsonl")) {
		return nil
	}
	units, err := store.ReadJSONL[digest.Unit](p.dir("units.jsonl"))
	if err != nil {
		return err
	}
	cands, err := store.ReadJSONL[rules.Candidate](p.dir("candidates.jsonl"))
	if err != nil {
		return err
	}
	g := &Gen{ID: 0, Status: genExtracted, PRs: map[int]string{}, Candidates: len(cands), CreatedAt: p.now().UTC(), ExtractedAt: p.now().UTC()}
	for _, u := range units {
		h := digest.ContentHash(u)
		g.PRs[u.PR] = h
		l.PRs[u.PR] = h
	}
	if chunks, err := store.ReadJSONL[digest.Chunk](p.dir("chunks.jsonl")); err == nil {
		g.Chunks = len(chunks)
	}
	if err := store.WriteJSONL(p.genDir(0, "units.jsonl"), units); err != nil {
		return err
	}
	if err := store.WriteJSONL(p.genDir(0, "candidates.jsonl"), cands); err != nil {
		return err
	}
	l.Gens = append(l.Gens, g)
	if err := p.saveLedger(l); err != nil {
		return err
	}
	p.Logf("migrated the existing cache: %d PRs and %d candidates are now generation 0", len(units), len(cands))
	return nil
}

// dropOpen removes a not-yet-sent generation so digest can rebuild it.
func (p *Pipeline) dropOpen(l *Ledger) error {
	if n := len(l.Gens); n > 0 && l.Gens[n-1].Status == genOpen {
		if err := os.RemoveAll(p.genDir(l.Gens[n-1].ID)); err != nil {
			return err
		}
		l.Gens = l.Gens[:n-1]
	}
	return nil
}

func (l *Ledger) nextID() int {
	if len(l.Gens) == 0 {
		return 1
	}
	return l.Gens[len(l.Gens)-1].ID + 1
}

// seal claims an open generation's PRs right before its requests are sent.
func (l *Ledger) seal(g *Gen) {
	if g.Status != genOpen {
		return
	}
	g.Status = genSubmitted
	for pr, h := range g.PRs {
		l.PRs[pr] = h
	}
}

func (l *Ledger) pending() []*Gen {
	var out []*Gen
	for _, g := range l.Gens {
		if g.Status != genExtracted {
			out = append(out, g)
		}
	}
	return out
}

func (l *Ledger) extracted() []*Gen {
	var out []*Gen
	for _, g := range l.Gens {
		if g.Status == genExtracted {
			out = append(out, g)
		}
	}
	return out
}

func (l *Ledger) totalChunks() int {
	n := 0
	for _, g := range l.Gens {
		n += g.Chunks
	}
	return n
}

// CandidateSet is every extracted candidate, with evidence from PRs that a
// later generation re-extracted removed: the latest extraction of a PR wins.
type CandidateSet struct {
	All []rules.Candidate
	// ByGen holds the same candidates keyed by generation, which synthesis
	// uses to tell already-folded candidates from new ones.
	ByGen map[string][]rules.Candidate
}

// Candidates loads the candidate set from every extracted generation.
func (p *Pipeline) Candidates() (CandidateSet, error) {
	cs := CandidateSet{ByGen: map[string][]rules.Candidate{}}
	l, err := p.loadLedger()
	if err != nil {
		return cs, err
	}
	latest := map[int]int{}
	for _, g := range l.extracted() {
		for pr := range g.PRs {
			latest[pr] = g.ID
		}
	}
	for _, g := range l.extracted() {
		cands, err := store.ReadJSONL[rules.Candidate](p.genDir(g.ID, "candidates.jsonl"))
		if err != nil {
			return cs, err
		}
		for _, c := range cands {
			var ev []rules.Evidence
			for _, e := range c.Evidence {
				if latest[e.PR] == g.ID {
					ev = append(ev, e)
				}
			}
			if len(ev) == 0 {
				continue
			}
			c.Evidence = ev
			cs.All = append(cs.All, c)
			cs.ByGen[g.Key()] = append(cs.ByGen[g.Key()], c)
		}
	}
	return cs, nil
}
