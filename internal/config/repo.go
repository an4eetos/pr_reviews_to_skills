package config

import (
	"fmt"
	"net/url"
	"regexp"
	"strings"
)

// Repo identifies a GitHub (or GitHub Enterprise) repository.
type Repo struct {
	Host  string
	Owner string
	Name  string
}

func (r Repo) String() string { return r.Owner + "/" + r.Name }

var segmentRe = regexp.MustCompile(`^[A-Za-z0-9_.-]+$`)

// ParseRepo accepts "owner/name", "https://github.com/owner/name(.git)",
// "github.com/owner/name/pull/12", "git@github.com:owner/name.git" and the
// same forms on a GitHub Enterprise host.
func ParseRepo(s string) (Repo, error) {
	in := s
	s = strings.TrimSpace(s)
	host := "github.com"
	path := s

	switch {
	case strings.HasPrefix(s, "git@"):
		hostPath, ok := strings.CutPrefix(s, "git@")
		h, p, found := strings.Cut(hostPath, ":")
		if !ok || !found {
			return Repo{}, fmt.Errorf("invalid repo %q", in)
		}
		host, path = h, p
	case strings.Contains(s, "://"):
		u, err := url.Parse(s)
		if err != nil {
			return Repo{}, fmt.Errorf("invalid repo URL %q: %w", in, err)
		}
		host, path = u.Host, u.Path
	default:
		first, rest, found := strings.Cut(s, "/")
		if found && strings.Contains(first, ".") {
			host, path = first, rest
		}
	}

	var parts []string
	for _, p := range strings.Split(path, "/") {
		if p != "" {
			parts = append(parts, p)
		}
	}
	if len(parts) < 2 {
		return Repo{}, fmt.Errorf("invalid repo %q: want owner/name or a GitHub URL", in)
	}
	owner, name := parts[0], strings.TrimSuffix(parts[1], ".git")
	if !segmentRe.MatchString(owner) || !segmentRe.MatchString(name) {
		return Repo{}, fmt.Errorf("invalid repo %q: bad owner or name", in)
	}
	return Repo{Host: strings.ToLower(host), Owner: owner, Name: name}, nil
}

// GraphQLEndpoint returns the GraphQL URL for the repo's host. apiURL, when
// set (GITHUB_API_URL), wins: it may be the REST base or the GraphQL URL itself.
func GraphQLEndpoint(r Repo, apiURL string) string {
	if apiURL != "" {
		apiURL = strings.TrimSuffix(apiURL, "/")
		if strings.HasSuffix(apiURL, "/graphql") {
			return apiURL
		}
		// GHES REST bases end in /api/v3; GraphQL lives at /api/graphql.
		if base, ok := strings.CutSuffix(apiURL, "/v3"); ok {
			return base + "/graphql"
		}
		return apiURL + "/graphql"
	}
	if r.Host == "" || r.Host == "github.com" || r.Host == "www.github.com" {
		return "https://api.github.com/graphql"
	}
	return "https://" + r.Host + "/api/graphql"
}
