// Package digest turns raw PRs into compact discussion units: noise and bot
// comments removed, author roles and thread outcomes annotated, and every
// kept comment given a short ref ("812.3") that the model cites as evidence.
package digest

import (
	"fmt"
	"path"
	"sort"
	"strings"
	"time"

	"github.com/an4eetos/pr_reviews_to_skills/internal/github"
)

const (
	maxBodyChars        = 1500
	maxDescriptionChars = 1200
	maxHunkLines        = 12
	maxAreas            = 8
)

type Options struct {
	IncludeBots bool
}

// Unit is one PR's reviewable discussion.
type Unit struct {
	PR          int          `json:"pr"`
	URL         string       `json:"url"`
	Title       string       `json:"title"`
	State       string       `json:"state"` // merged | closed | open
	Author      string       `json:"author"`
	UpdatedAt   time.Time    `json:"updated_at"`
	Labels      []string     `json:"labels,omitempty"`
	Areas       []string     `json:"areas,omitempty"`
	Description string       `json:"description,omitempty"`
	Threads     []Thread     `json:"threads,omitempty"`
	Reviews     []UnitReview `json:"reviews,omitempty"`
	Comments    []Comment    `json:"comments,omitempty"`
}

type Thread struct {
	Path     string    `json:"path"`
	Line     int       `json:"line,omitempty"`
	DiffHunk string    `json:"diff_hunk,omitempty"`
	Outcome  string    `json:"outcome"`
	Comments []Comment `json:"comments"`
}

type UnitReview struct {
	State   string  `json:"state"`
	Comment Comment `json:"comment"`
}

// Comment is a kept comment. Ref is unique across the whole digest.
type Comment struct {
	Ref      string    `json:"ref"`
	URL      string    `json:"url"`
	Author   string    `json:"author"`
	Role     string    `json:"role"` // maintainer | contributor | pr-author | bot
	Date     time.Time `json:"date"`
	Body     string    `json:"body"`
	ThumbsUp int       `json:"thumbs_up,omitempty"`
}

type Stats struct {
	PRsIn        int `json:"prs_in"`
	PRsKept      int `json:"prs_kept"`
	Threads      int `json:"threads"`
	Comments     int `json:"comments"`
	DroppedNoise int `json:"dropped_noise"`
	DroppedBots  int `json:"dropped_bots"`
}

// Build dedupes PRs by number (keeping the latest fetch), filters noise and
// returns units for PRs that still have discussion from someone other than
// the PR author, most recently updated first.
func Build(prs []github.PullRequest, opts Options) ([]Unit, Stats) {
	latest := map[int]github.PullRequest{}
	for _, pr := range prs {
		if cur, ok := latest[pr.Number]; !ok || pr.UpdatedAt.After(cur.UpdatedAt) {
			latest[pr.Number] = pr
		}
	}
	ordered := make([]github.PullRequest, 0, len(latest))
	for _, pr := range latest {
		ordered = append(ordered, pr)
	}
	sort.Slice(ordered, func(i, j int) bool {
		if !ordered[i].UpdatedAt.Equal(ordered[j].UpdatedAt) {
			return ordered[i].UpdatedAt.After(ordered[j].UpdatedAt)
		}
		return ordered[i].Number > ordered[j].Number
	})

	st := Stats{PRsIn: len(ordered)}
	var units []Unit
	for _, pr := range ordered {
		u, ok := buildUnit(pr, opts, &st)
		if !ok {
			continue
		}
		units = append(units, u)
		st.PRsKept++
		st.Threads += len(u.Threads)
	}
	return units, st
}

type builder struct {
	pr       github.PullRequest
	opts     Options
	st       *Stats
	seq      int
	external bool // a kept comment from someone other than the PR author
}

func (b *builder) role(a *github.Actor, assoc string) string {
	switch {
	case IsBot(a):
		return "bot"
	case a != nil && b.pr.Author != nil && strings.EqualFold(a.Login, b.pr.Author.Login):
		return "pr-author"
	case assoc == "OWNER" || assoc == "MEMBER" || assoc == "COLLABORATOR":
		return "maintainer"
	default:
		return "contributor"
	}
}

// keep cleans a comment and reports whether it is worth showing the model.
func (b *builder) keep(c github.Comment) (Comment, bool) {
	if IsBot(c.Author) && !b.opts.IncludeBots {
		b.st.DroppedBots++
		return Comment{}, false
	}
	body := CleanBody(c.Body)
	if IsNoise(body) {
		b.st.DroppedNoise++
		return Comment{}, false
	}
	b.seq++
	out := Comment{
		Ref:      fmt.Sprintf("%d.%d", b.pr.Number, b.seq),
		URL:      c.URL,
		Author:   login(c.Author),
		Role:     b.role(c.Author, c.AuthorAssociation),
		Date:     c.CreatedAt,
		Body:     truncateRunes(body, maxBodyChars),
		ThumbsUp: c.ThumbsUp,
	}
	if out.Role != "pr-author" {
		b.external = true
	}
	b.st.Comments++
	return out, true
}

func buildUnit(pr github.PullRequest, opts Options, st *Stats) (Unit, bool) {
	b := &builder{pr: pr, opts: opts, st: st}
	u := Unit{
		PR:          pr.Number,
		URL:         pr.URL,
		Title:       pr.Title,
		State:       prState(pr),
		Author:      login(pr.Author),
		UpdatedAt:   pr.UpdatedAt,
		Labels:      pr.Labels,
		Areas:       areas(pr.Files),
		Description: truncateRunes(CleanBody(pr.Body), maxDescriptionChars),
	}

	for _, t := range pr.Threads {
		th := Thread{Path: t.Path, Line: t.Line}
		acked := false
		for i, c := range t.Comments {
			if i == 0 {
				th.DiffHunk = lastLines(c.DiffHunk, maxHunkLines)
			}
			// A PR-author "done"/"fixed" reply is a strong acceptance signal even
			// though the comment itself is dropped as noise.
			if i > 0 && b.role(c.Author, c.AuthorAssociation) == "pr-author" && IsAck(CleanBody(c.Body)) {
				acked = true
			}
			if kc, ok := b.keep(c); ok {
				th.Comments = append(th.Comments, kc)
			}
		}
		if len(th.Comments) == 0 {
			continue
		}
		th.Outcome = threadOutcome(t, acked)
		u.Threads = append(u.Threads, th)
	}

	for _, r := range pr.Reviews {
		if r.State != "CHANGES_REQUESTED" && r.State != "COMMENTED" && r.State != "APPROVED" {
			continue
		}
		kc, ok := b.keep(github.Comment{
			ID: r.ID, URL: r.URL, Body: r.Body, CreatedAt: r.SubmittedAt,
			Author: r.Author, AuthorAssociation: r.AuthorAssociation,
		})
		if ok {
			u.Reviews = append(u.Reviews, UnitReview{State: r.State, Comment: kc})
		}
	}

	for _, c := range pr.Comments {
		if kc, ok := b.keep(c); ok {
			u.Comments = append(u.Comments, kc)
		}
	}

	if !b.external {
		return Unit{}, false
	}
	return u, true
}

func threadOutcome(t github.ReviewThread, acked bool) string {
	var parts []string
	if t.IsResolved {
		parts = append(parts, "resolved")
	}
	if t.IsOutdated {
		// The commented lines changed after the comment: likely addressed.
		parts = append(parts, "code-changed")
	}
	if acked {
		parts = append(parts, "author-acknowledged")
	}
	if len(parts) == 0 {
		return "unresolved"
	}
	return strings.Join(parts, ",")
}

func prState(pr github.PullRequest) string {
	switch {
	case pr.Merged:
		return "merged"
	case pr.State == "OPEN":
		return "open"
	default:
		return "closed"
	}
}

// areas returns the distinct top-level (two segments deep) directories the PR
// touched, in order of first appearance.
func areas(files []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, f := range files {
		dir := path.Dir(f)
		if dir == "." {
			dir = "/"
		} else if parts := strings.SplitN(dir, "/", 3); len(parts) > 2 {
			dir = parts[0] + "/" + parts[1]
		}
		if !seen[dir] {
			seen[dir] = true
			out = append(out, dir)
			if len(out) == maxAreas {
				break
			}
		}
	}
	return out
}

func login(a *github.Actor) string {
	if a == nil || a.Login == "" {
		return "ghost"
	}
	return a.Login
}

func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + " [...]"
}

func lastLines(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

// RefIndex maps comment refs to the comments they name, across all units.
func RefIndex(units []Unit) map[string]RefInfo {
	idx := map[string]RefInfo{}
	add := func(pr int, c Comment) { idx[c.Ref] = RefInfo{PR: pr, Comment: c} }
	for _, u := range units {
		for _, t := range u.Threads {
			for _, c := range t.Comments {
				add(u.PR, c)
			}
		}
		for _, r := range u.Reviews {
			add(u.PR, r.Comment)
		}
		for _, c := range u.Comments {
			add(u.PR, c)
		}
	}
	return idx
}

type RefInfo struct {
	PR      int
	Comment Comment
}
