package digest

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"strings"
)

// structuralTagRe matches our own wrapper tags so untrusted PR text cannot
// close a <pr>/<thread> element early and pose as structure.
var structuralTagRe = regexp.MustCompile(`(?i)<(/?)(pr|thread|review|conversation|discussions)\b`)

func neutralize(s string) string {
	return structuralTagRe.ReplaceAllString(s, "‹$1$2")
}

// Chunk is one extraction request's worth of rendered discussion units.
type Chunk struct {
	ID        string `json:"id"`
	PRs       []int  `json:"prs"`
	Text      string `json:"text"`
	EstTokens int    `json:"est_tokens"`
}

// EstimateTokens is a deliberately conservative local estimate (code and
// markdown run ~3-4 chars/token); --dry-run calibrates it with count_tokens.
func EstimateTokens(s string) int { return len(s)/3 + 1 }

// renderBlocks renders a unit as a header plus independent blocks (threads,
// reviews, conversation comments) so oversized PRs can be split.
func renderBlocks(u Unit) (header string, blocks []string) {
	var h strings.Builder
	fmt.Fprintf(&h, "title: %s\n", neutralize(oneLine(u.Title)))
	if len(u.Labels) > 0 {
		fmt.Fprintf(&h, "labels: %s\n", strings.Join(u.Labels, ", "))
	}
	if len(u.Areas) > 0 {
		fmt.Fprintf(&h, "areas: %s\n", strings.Join(u.Areas, ", "))
	}
	if u.Description != "" {
		fmt.Fprintf(&h, "description:\n%s\n", neutralize(u.Description))
	}
	header = h.String()

	for _, t := range u.Threads {
		var b strings.Builder
		loc := t.Path
		if t.Line > 0 {
			loc = fmt.Sprintf("%s:%d", t.Path, t.Line)
		}
		fmt.Fprintf(&b, "<thread file=%q outcome=%q>\n", loc, t.Outcome)
		if t.DiffHunk != "" {
			fmt.Fprintf(&b, "```diff\n%s\n```\n", neutralize(t.DiffHunk))
		}
		for _, c := range t.Comments {
			writeComment(&b, c)
		}
		b.WriteString("</thread>\n")
		blocks = append(blocks, b.String())
	}
	for _, r := range u.Reviews {
		var b strings.Builder
		fmt.Fprintf(&b, "<review verdict=%q>\n", r.State)
		writeComment(&b, r.Comment)
		b.WriteString("</review>\n")
		blocks = append(blocks, b.String())
	}
	for _, c := range u.Comments {
		var b strings.Builder
		b.WriteString("<conversation>\n")
		writeComment(&b, c)
		b.WriteString("</conversation>\n")
		blocks = append(blocks, b.String())
	}
	return header, blocks
}

func writeComment(b *strings.Builder, c Comment) {
	fmt.Fprintf(b, "[%s] %s @%s %s", c.Ref, c.Role, c.Author, c.Date.Format("2006-01-02"))
	if c.ThumbsUp > 0 {
		fmt.Fprintf(b, " +%d", c.ThumbsUp)
	}
	b.WriteString(":\n")
	b.WriteString(neutralize(c.Body))
	b.WriteString("\n")
}

func openTag(u Unit, part, parts int) string {
	partAttr := ""
	if parts > 1 {
		partAttr = fmt.Sprintf(" part=\"%d/%d\"", part, parts)
	}
	return fmt.Sprintf("<pr number=\"%d\" state=%q author=\"@%s\" updated=%q%s>\n",
		u.PR, u.State, u.Author, u.UpdatedAt.Format("2006-01-02"), partAttr)
}

// ContentHash identifies a unit's reviewable content: title, description,
// threads with their outcomes, reviews and comments. It leaves out the PR's
// updatedAt and state, so a CI push, label edit or merge that leaves the
// discussion unchanged does not make the PR look new.
func ContentHash(u Unit) string {
	header, blocks := renderBlocks(u)
	h := sha256.New()
	fmt.Fprintf(h, "%d\n%s", u.PR, header)
	for _, b := range blocks {
		h.Write([]byte(b))
	}
	return hex.EncodeToString(h.Sum(nil))[:16]
}

// Render renders a whole unit; RenderParts splits it to fit maxTokens.
func Render(u Unit) string {
	parts := RenderParts(u, 0)
	return strings.Join(parts, "")
}

// RenderParts renders u as one or more <pr> elements each within maxTokens
// (0 = no limit). Every part repeats the header so it stands alone.
func RenderParts(u Unit, maxTokens int) []string {
	header, blocks := renderBlocks(u)
	if maxTokens <= 0 {
		return []string{openTag(u, 1, 1) + header + strings.Join(blocks, "") + "</pr>\n"}
	}
	// Group blocks greedily, then emit with part numbers.
	budget := maxTokens - EstimateTokens(header) - 40
	var groups [][]string
	var cur []string
	curTok := 0
	for _, blk := range blocks {
		t := EstimateTokens(blk)
		if len(cur) > 0 && curTok+t > budget {
			groups = append(groups, cur)
			cur, curTok = nil, 0
		}
		cur = append(cur, blk)
		curTok += t
	}
	if len(cur) > 0 || len(groups) == 0 {
		groups = append(groups, cur)
	}
	out := make([]string, len(groups))
	for i, g := range groups {
		out[i] = openTag(u, i+1, len(groups)) + header + strings.Join(g, "") + "</pr>\n"
	}
	return out
}

// Pack renders units into chunks of at most maxTokens (estimated), keeping
// each PR's parts together where possible.
func Pack(units []Unit, maxTokens int) []Chunk {
	var chunks []Chunk
	var cur strings.Builder
	var curPRs []int
	curTok := 0
	flush := func() {
		if cur.Len() == 0 {
			return
		}
		chunks = append(chunks, Chunk{
			ID:        fmt.Sprintf("chunk-%05d", len(chunks)+1),
			PRs:       curPRs,
			Text:      cur.String(),
			EstTokens: curTok,
		})
		cur.Reset()
		curPRs = nil
		curTok = 0
	}
	for _, u := range units {
		for _, part := range RenderParts(u, maxTokens) {
			t := EstimateTokens(part)
			if curTok > 0 && curTok+t > maxTokens {
				flush()
			}
			cur.WriteString(part)
			curTok += t
			if len(curPRs) == 0 || curPRs[len(curPRs)-1] != u.PR {
				curPRs = append(curPRs, u.PR)
			}
		}
	}
	flush()
	return chunks
}

func oneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}
