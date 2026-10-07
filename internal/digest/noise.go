package digest

import (
	"regexp"
	"strings"
	"unicode"

	"github.com/an4eetos/pr_reviews_to_skills/internal/github"
)

var knownBots = map[string]bool{
	"dependabot":                    true,
	"renovate":                      true,
	"codecov":                       true,
	"github-actions":                true,
	"sonarcloud":                    true,
	"netlify":                       true,
	"vercel":                        true,
	"k8s-ci-robot":                  true,
	"k8s-triage-robot":              true,
	"coveralls":                     true,
	"changeset-bot":                 true,
	"allcontributors":               true,
	"stale":                         true,
	"mergify":                       true,
	"pre-commit-ci":                 true,
	"gemini-code-assist":            true,
	"coderabbitai":                  true,
	"copilot-pull-request-reviewer": true,
}

// IsBot reports whether the actor is an app/bot account.
func IsBot(a *github.Actor) bool {
	if a == nil {
		return false
	}
	if a.Type == "Bot" {
		return true
	}
	login := strings.ToLower(a.Login)
	if strings.HasSuffix(login, "[bot]") || strings.HasSuffix(login, "-bot") {
		return true
	}
	return knownBots[login]
}

var (
	htmlCommentRe = regexp.MustCompile(`(?s)<!--.*?-->`)
	blankLinesRe  = regexp.MustCompile(`\n{3,}`)
	nonWordRe     = regexp.MustCompile(`[^a-z0-9+]+`)
	ackRe         = regexp.MustCompile(`^(?:(?:lgtm|sgtm|ptal|looks good|looks good to me|lg|thanks|thank you|thank you so much|thanks a lot|thx|ty|done|fixed|good catch|nice|nice catch|great|\+1|ack|ok|okay|will do|updated|addressed|resolved|agreed|makes sense|yes|yep|yeah|sure|approved|ship it)(?: |$))+$`)
)

// CleanBody strips HTML comments (PR templates), quoted reply lines and
// excess blank lines.
func CleanBody(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = htmlCommentRe.ReplaceAllString(s, "")
	var lines []string
	for _, l := range strings.Split(s, "\n") {
		if strings.HasPrefix(strings.TrimSpace(l), ">") {
			continue
		}
		lines = append(lines, strings.TrimRight(l, " \t"))
	}
	s = strings.Join(lines, "\n")
	s = blankLinesRe.ReplaceAllString(s, "\n\n")
	return strings.TrimSpace(s)
}

// IsAck reports whether a (cleaned) body is just an acknowledgement like
// "done", "fixed, thanks!" or "LGTM".
func IsAck(body string) bool {
	n := strings.TrimSpace(nonWordRe.ReplaceAllString(strings.ToLower(body), " "))
	return n != "" && ackRe.MatchString(n)
}

// IsNoise reports whether a cleaned comment body carries no reviewable
// content: empty, an acknowledgement, emoji/punctuation only, or a bot
// command such as "/retest" or "/lgtm".
func IsNoise(body string) bool {
	b := strings.TrimSpace(body)
	if b == "" || IsAck(b) {
		return true
	}
	if isCommandBlock(b) {
		return true
	}
	for _, r := range b {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			return false
		}
	}
	return true
}

var commandLineRe = regexp.MustCompile(`^/[a-z][a-z0-9_-]*(\s.*)?$`)

// isCommandBlock is true for bodies made only of bot slash-command lines
// ("/lgtm\n/approve", "/assign @bob"), not paths like "/api/v1 ...".
func isCommandBlock(b string) bool {
	for _, l := range strings.Split(b, "\n") {
		l = strings.TrimSpace(l)
		if l != "" && !commandLineRe.MatchString(l) {
			return false
		}
	}
	return true
}
