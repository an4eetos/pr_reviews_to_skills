package config

import "testing"

func TestParseRepo(t *testing.T) {
	cases := []struct {
		in   string
		want Repo
	}{
		{"golang/go", Repo{"github.com", "golang", "go"}},
		{"  golang/go  ", Repo{"github.com", "golang", "go"}},
		{"https://github.com/golang/go", Repo{"github.com", "golang", "go"}},
		{"https://github.com/golang/go.git", Repo{"github.com", "golang", "go"}},
		{"https://github.com/golang/go/pull/123", Repo{"github.com", "golang", "go"}},
		{"github.com/golang/go/", Repo{"github.com", "golang", "go"}},
		{"git@github.com:golang/go.git", Repo{"github.com", "golang", "go"}},
		{"https://ghe.corp.example/team/svc", Repo{"ghe.corp.example", "team", "svc"}},
		{"my-org/my.repo_name", Repo{"github.com", "my-org", "my.repo_name"}},
	}
	for _, c := range cases {
		got, err := ParseRepo(c.in)
		if err != nil {
			t.Errorf("ParseRepo(%q) error: %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("ParseRepo(%q) = %+v, want %+v", c.in, got, c.want)
		}
	}
	for _, bad := range []string{"", "golang", "https://github.com/golang", "a/b c", "git@github.com"} {
		if _, err := ParseRepo(bad); err == nil {
			t.Errorf("ParseRepo(%q) succeeded, want error", bad)
		}
	}
}

func TestGraphQLEndpoint(t *testing.T) {
	gh := Repo{Host: "github.com", Owner: "a", Name: "b"}
	ghe := Repo{Host: "ghe.corp.example", Owner: "a", Name: "b"}
	cases := []struct {
		repo   Repo
		apiURL string
		want   string
	}{
		{gh, "", "https://api.github.com/graphql"},
		{ghe, "", "https://ghe.corp.example/api/graphql"},
		{gh, "https://api.github.com", "https://api.github.com/graphql"},
		{ghe, "https://ghe.corp.example/api/v3", "https://ghe.corp.example/api/graphql"},
		{ghe, "https://ghe.corp.example/api/graphql/", "https://ghe.corp.example/api/graphql"},
	}
	for _, c := range cases {
		if got := GraphQLEndpoint(c.repo, c.apiURL); got != c.want {
			t.Errorf("GraphQLEndpoint(%v, %q) = %q, want %q", c.repo, c.apiURL, got, c.want)
		}
	}
}

func TestParseStates(t *testing.T) {
	got, err := ParseStates("open, Merged,merged")
	if err != nil || len(got) != 2 || got[0] != "OPEN" || got[1] != "MERGED" {
		t.Fatalf("ParseStates = %v, %v", got, err)
	}
	if _, err := ParseStates("draft"); err == nil {
		t.Fatal("want error for unknown state")
	}
}
