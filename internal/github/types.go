package github

import "time"

// Actor is a GraphQL Actor; Type is __typename ("User", "Bot", ...).
type Actor struct {
	Login string `json:"login"`
	Type  string `json:"__typename"`
}

// PullRequest is the normalized PR as stored in raw/page-*.jsonl. Nested
// connections are fully paginated (except comments inside a single review
// thread, which are capped at threadCommentsPage).
type PullRequest struct {
	Number            int            `json:"number"`
	Title             string         `json:"title"`
	Body              string         `json:"body"`
	URL               string         `json:"url"`
	State             string         `json:"state"`
	Merged            bool           `json:"merged"`
	CreatedAt         time.Time      `json:"createdAt"`
	UpdatedAt         time.Time      `json:"updatedAt"`
	MergedAt          *time.Time     `json:"mergedAt,omitempty"`
	Author            *Actor         `json:"author,omitempty"`
	AuthorAssociation string         `json:"authorAssociation"`
	Labels            []string       `json:"labels,omitempty"`
	Files             []string       `json:"files,omitempty"`
	FilesTotal        int            `json:"filesTotal"`
	Reviews           []Review       `json:"reviews,omitempty"`
	Threads           []ReviewThread `json:"threads,omitempty"`
	Comments          []Comment      `json:"comments,omitempty"`
}

type Review struct {
	ID                string    `json:"id"`
	URL               string    `json:"url"`
	State             string    `json:"state"`
	Body              string    `json:"body"`
	SubmittedAt       time.Time `json:"submittedAt"`
	Author            *Actor    `json:"author,omitempty"`
	AuthorAssociation string    `json:"authorAssociation"`
}

type ReviewThread struct {
	ID            string    `json:"id"`
	Path          string    `json:"path"`
	Line          int       `json:"line"`
	IsResolved    bool      `json:"isResolved"`
	IsOutdated    bool      `json:"isOutdated"`
	CommentsTotal int       `json:"commentsTotal"`
	Comments      []Comment `json:"comments"`
}

// Comment is either a review-thread comment (DiffHunk set) or an issue
// comment on the PR conversation.
type Comment struct {
	ID                string    `json:"id"`
	URL               string    `json:"url"`
	Body              string    `json:"body"`
	CreatedAt         time.Time `json:"createdAt"`
	DiffHunk          string    `json:"diffHunk,omitempty"`
	Author            *Actor    `json:"author,omitempty"`
	AuthorAssociation string    `json:"authorAssociation"`
	ThumbsUp          int       `json:"thumbsUp,omitempty"`
	ThumbsDown        int       `json:"thumbsDown,omitempty"`
}

// --- GraphQL wire shapes ---------------------------------------------------

type pageInfo struct {
	HasNextPage bool   `json:"hasNextPage"`
	EndCursor   string `json:"endCursor"`
}

type conn[T any] struct {
	TotalCount int      `json:"totalCount"`
	PageInfo   pageInfo `json:"pageInfo"`
	Nodes      []T      `json:"nodes"`
}

type count struct {
	TotalCount int `json:"totalCount"`
}

type gqlComment struct {
	ID                string    `json:"id"`
	URL               string    `json:"url"`
	Body              string    `json:"body"`
	CreatedAt         time.Time `json:"createdAt"`
	DiffHunk          string    `json:"diffHunk"`
	Author            *Actor    `json:"author"`
	AuthorAssociation string    `json:"authorAssociation"`
	ThumbsUp          count     `json:"thumbsUp"`
	ThumbsDown        count     `json:"thumbsDown"`
}

func (c gqlComment) toComment() Comment {
	return Comment{
		ID:                c.ID,
		URL:               c.URL,
		Body:              c.Body,
		CreatedAt:         c.CreatedAt,
		DiffHunk:          c.DiffHunk,
		Author:            c.Author,
		AuthorAssociation: c.AuthorAssociation,
		ThumbsUp:          c.ThumbsUp.TotalCount,
		ThumbsDown:        c.ThumbsDown.TotalCount,
	}
}

type gqlThread struct {
	ID         string           `json:"id"`
	Path       string           `json:"path"`
	Line       *int             `json:"line"`
	IsResolved bool             `json:"isResolved"`
	IsOutdated bool             `json:"isOutdated"`
	Comments   conn[gqlComment] `json:"comments"`
}

func (t gqlThread) toThread() ReviewThread {
	out := ReviewThread{
		ID:            t.ID,
		Path:          t.Path,
		IsResolved:    t.IsResolved,
		IsOutdated:    t.IsOutdated,
		CommentsTotal: t.Comments.TotalCount,
	}
	if t.Line != nil {
		out.Line = *t.Line
	}
	for _, c := range t.Comments.Nodes {
		out.Comments = append(out.Comments, c.toComment())
	}
	return out
}

type gqlPR struct {
	Number            int        `json:"number"`
	Title             string     `json:"title"`
	Body              string     `json:"body"`
	URL               string     `json:"url"`
	State             string     `json:"state"`
	Merged            bool       `json:"merged"`
	CreatedAt         time.Time  `json:"createdAt"`
	UpdatedAt         time.Time  `json:"updatedAt"`
	MergedAt          *time.Time `json:"mergedAt"`
	Author            *Actor     `json:"author"`
	AuthorAssociation string     `json:"authorAssociation"`
	Labels            struct {
		Nodes []struct {
			Name string `json:"name"`
		} `json:"nodes"`
	} `json:"labels"`
	Files struct {
		TotalCount int `json:"totalCount"`
		Nodes      []struct {
			Path string `json:"path"`
		} `json:"nodes"`
	} `json:"files"`
	Reviews       conn[Review]     `json:"reviews"`
	ReviewThreads conn[gqlThread]  `json:"reviewThreads"`
	Comments      conn[gqlComment] `json:"comments"`
}

func (p gqlPR) toPR() PullRequest {
	out := PullRequest{
		Number:            p.Number,
		Title:             p.Title,
		Body:              p.Body,
		URL:               p.URL,
		State:             p.State,
		Merged:            p.Merged,
		CreatedAt:         p.CreatedAt,
		UpdatedAt:         p.UpdatedAt,
		MergedAt:          p.MergedAt,
		Author:            p.Author,
		AuthorAssociation: p.AuthorAssociation,
		FilesTotal:        p.Files.TotalCount,
		Reviews:           p.Reviews.Nodes,
	}
	for _, l := range p.Labels.Nodes {
		out.Labels = append(out.Labels, l.Name)
	}
	for _, f := range p.Files.Nodes {
		out.Files = append(out.Files, f.Path)
	}
	for _, t := range p.ReviewThreads.Nodes {
		out.Threads = append(out.Threads, t.toThread())
	}
	for _, c := range p.Comments.Nodes {
		out.Comments = append(out.Comments, c.toComment())
	}
	return out
}

type rateLimit struct {
	Cost      int       `json:"cost"`
	Remaining int       `json:"remaining"`
	ResetAt   time.Time `json:"resetAt"`
}
