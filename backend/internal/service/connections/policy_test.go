package connections

import (
	"strings"
	"testing"
)

func TestGitHubPolicy(t *testing.T) {
	refused := []struct{ method, path, body, family string }{
		{"POST", "/repos/o/r/keys", "", "credentials"},
		{"post", "/repos/O/R/keys/", "", "credentials"},
		{"POST", "/repos/o/r/hooks", "", "credentials"},
		{"PUT", "/repos/o/r/actions/secrets/TOKEN", "", "credentials"},
		{"PUT", "/repos/o/r/collaborators/someone", "", "credentials"},
		{"PUT", "/repos/o/r/branches/feature/x/protection", "", "credentials"},
		{"PUT", "/repos/o/r/branches/feature%2Fx/protection", "", "credentials"},
		{"POST", "/orgs/acme/personal-access-tokens/1", "", "credentials"},
		{"PATCH", "/user", "", "credentials"},
		{"POST", "/repositories/123/keys", "", "credentials"},
		{"POST", "/repos/o/r/pulls/../keys", "", "credentials"},
		{"POST", "/repos/o/r/%6b%65%79%73", "", "credentials"},
		{"POST", "/repos%2Fo%2Fr%2Fkeys", "", "credentials"},
		{"POST", `\repos\o\r\keys`, "", "credentials"},
		{"POST", "/repos/o/r/ke\tys", "", "credentials"},
		{"PUT", "/repos/o/r/contents/README.md", "", "write the repository"},
		{"POST", "/repos/o/r/git/refs", "", "write the repository"},
		{"POST", "/repos/o/r/merges", "", "write the repository"},
		{"POST", "/repos/o/r/releases", "", "release"},
		{"DELETE", "/repos/o/r", "", "configure a repository"},
		{"POST", "/user/repos", "", "configure a repository"},
		{"POST", "/graphql", `{"query":"mutation { addDeployKey }"}`, "GraphQL mutation"},
		{"POST", "/graphql", `{"query":"mutation { x }"}`, "GraphQL mutation"},
	}
	for _, tc := range refused {
		got := refusal("github", tc.method, tc.path, tc.body)
		if !strings.HasPrefix(got, RefusalPrefix) || !strings.Contains(got, tc.family) {
			t.Errorf("%s %s = %q, want a %s refusal", tc.method, tc.path, got, tc.family)
		}
	}
	allowed := []struct{ method, path, body string }{
		{"GET", "/repos/o/r/keys", ""},
		{"", "/repos/o/r/hooks", ""},
		{"GET", "/user", ""},
		{"POST", "/repos/o/r/pulls", ""},
		{"PUT", "/repos/o/r/pulls/7/merge", ""},
		{"POST", "/repos/o/r/issues/7/comments", ""},
		{"PATCH", "/repos/o/r/issues/7", ""},
		{"GET", "/repos/o/r/releases", ""},
		{"POST", "/graphql", `{"query":"{ viewer { login } }"}`},
		{"POST", "/repos/o/r/keysmith", ""},
	}
	for _, tc := range allowed {
		if got := refusal("github", tc.method, tc.path, tc.body); got != "" {
			t.Errorf("%s %s refused: %q", tc.method, tc.path, got)
		}
	}
	if got := refusal("cloudflare", "POST", "/repos/o/r/keys", ""); got != "" {
		t.Errorf("a rule applied to cloudflare: %q", got)
	}
}
