package connections

import (
	"encoding/json"
	"net/url"
	"regexp"
	"slices"
	"strings"
)

// The refusal policy runs before a call leaves the daemon. It is a port of
// Swarm Online's src/shared/connection-policy.ts. On 2026-10-14 an agent there
// added its own SSH key to a repository through github_api and pushed around
// every check. A connection calls the API as the owner, so the policy refuses
// the GitHub writes that change credentials, repository contents or settings.
//
// Swarm 3 differs from Online in one rule: it does not refuse a pull request,
// because Swarm 3 workers open their own pull requests with `gh pr create`.

type refusalFamily string

const (
	familyCredentials refusalFamily = "credentials"
	familyRepository  refusalFamily = "repository"
	familyRelease     refusalFamily = "release"
	familyRepoAdmin   refusalFamily = "repo-admin"
	familyGraphQL     refusalFamily = "graphql"
)

var refusalText = map[refusalFamily][2]string{
	familyCredentials: {"create or change credentials, keys, tokens, secrets, webhooks, collaborators or permissions", "Those are the owner's; ask the user if the project needs one."},
	familyRepository:  {"write the repository", "Commit and push with git in your worktree, then open the pull request with `gh pr create`."},
	familyRelease:     {"create, change or delete a release", "Releases are the owner's; ask the user."},
	familyRepoAdmin:   {"create, delete, transfer or configure a repository", "Ask the user."},
	familyGraphQL:     {"run a GraphQL mutation", "Use the REST path of an allowed write."},
}

// RefusalPrefix starts every refusal text.
const RefusalPrefix = "REFUSED: "

type policyRule struct {
	// path segments: "{x}" and "*" match one segment; a segment that ends in
	// "*" matches a segment that starts with the rest.
	path   string
	family refusalFamily
	// methods the rule refuses. Empty: every method other than GET and HEAD.
	methods []string
	// exact: only this path. Otherwise this path and every deeper path.
	exact bool
}

var githubRules = func() []policyRule {
	var rules []policyRule
	for _, p := range []string{
		"/repos/{o}/{r}/keys", "/user/keys", "/user/gpg_keys", "/user/ssh_signing_keys",
		"/repos/{o}/{r}/actions/secrets", "/repos/{o}/{r}/actions/variables",
		"/repos/{o}/{r}/environments/*/secrets", "/repos/{o}/{r}/environments/*/variables",
		"/repos/{o}/{r}/dependabot/secrets", "/repos/{o}/{r}/codespaces/secrets",
		"/orgs/{o}/actions/secrets", "/orgs/{o}/dependabot/secrets",
		"/repos/{o}/{r}/hooks", "/orgs/{o}/hooks", "/repos/{o}/{r}/collaborators", "/repos/{o}/{r}/invitations",
		"/repos/{o}/{r}/branches/*/protection", "/repos/{o}/{r}/rulesets", "/orgs/{o}/rulesets",
		"/orgs/{o}/members", "/orgs/{o}/memberships", "/orgs/{o}/teams", "/teams", "/orgs/{o}/personal-access-token*",
		"/authorizations", "/applications", "/app", "/user/installations", "/installations",
		"/repos/{o}/{r}/autolinks", "/repos/{o}/{r}/pages", "/user/emails",
		"/repos/{o}/{r}/actions/runners", "/orgs/{o}/actions/runners", "/repos/{o}/{r}/actions/permissions",
		"/repos/{o}/{r}/environments", "/orgs/{o}/actions/variables", "/repos/{o}/{r}/codespaces", "/user/codespaces",
	} {
		rules = append(rules, policyRule{path: p, family: familyCredentials})
	}
	rules = append(rules, policyRule{path: "/user", family: familyCredentials, methods: []string{"PATCH"}, exact: true})
	for _, p := range []string{
		"/repos/{o}/{r}/git/refs", "/repos/{o}/{r}/git/commits", "/repos/{o}/{r}/git/trees", "/repos/{o}/{r}/git/blobs",
		"/repos/{o}/{r}/git/tags", "/repos/{o}/{r}/contents", "/repos/{o}/{r}/merges", "/repos/{o}/{r}/merge-upstream",
		"/repos/{o}/{r}/import", "/repos/{o}/{r}/branches/*/rename",
	} {
		rules = append(rules, policyRule{path: p, family: familyRepository})
	}
	return append(rules,
		policyRule{path: "/repos/{o}/{r}/releases", family: familyRelease},
		policyRule{path: "/repos/{o}/{r}", family: familyRepoAdmin, methods: []string{"DELETE", "PATCH"}, exact: true},
		policyRule{path: "/repos/{o}/{r}/transfer", family: familyRepoAdmin},
		policyRule{path: "/repos/{o}/{r}/forks", family: familyRepoAdmin},
		policyRule{path: "/repos/{o}/{r}/generate", family: familyRepoAdmin, methods: []string{"POST"}},
		policyRule{path: "/user/repos", family: familyRepoAdmin, methods: []string{"POST"}},
		policyRule{path: "/orgs/{o}/repos", family: familyRepoAdmin, methods: []string{"POST"}},
	)
}()

// pathSegments returns the path as the server would see it, in lower-case
// segments. It removes the query and the fragment, tabs and newlines, turns
// `\` into `/`, and resolves `.` and `..`. Then it percent-decodes each segment
// and splits it again, so an encoded `/` cannot hide a segment.
func pathSegments(raw string) []string {
	text := strings.Map(func(r rune) rune {
		if r == '\t' || r == '\n' || r == '\r' {
			return -1
		}
		return r
	}, strings.TrimSpace(raw))
	if i := strings.IndexAny(text, "?#"); i >= 0 {
		text = text[:i]
	}
	text = strings.ReplaceAll(text, `\`, "/")
	var segments []string
	for _, part := range strings.Split(text, "/") {
		decoded, err := url.PathUnescape(part)
		if err != nil {
			decoded = part
		}
		for _, piece := range strings.FieldsFunc(decoded, func(r rune) bool { return r == '/' || r == '\\' }) {
			s := strings.ToLower(strings.TrimSpace(piece))
			switch s {
			case "", ".":
			case "..":
				if len(segments) > 0 {
					segments = segments[:len(segments)-1]
				}
			default:
				segments = append(segments, s)
			}
		}
	}
	return segments
}

var numericID = regexp.MustCompile(`^\d+$`)

// routeSegments makes two forms of one route into one: /repositories/{id}/…
// is /repos/{o}/{r}/…, and a branch name can hold a slash.
func routeSegments(in []string) []string {
	segments := slices.Clone(in)
	if len(segments) >= 2 && segments[0] == "repositories" && numericID.MatchString(segments[1]) {
		segments = append([]string{"repos", "_", "_"}, segments[2:]...)
	}
	if len(segments) > 4 && segments[0] == "repos" && segments[3] == "branches" {
		end := -1
		for i := 5; i < len(segments); i++ {
			if segments[i] == "protection" || segments[i] == "rename" {
				end = i
				break
			}
		}
		if end > 5 {
			joined := strings.Join(segments[4:end], "/")
			segments = append(append(slices.Clone(segments[:4]), joined), segments[end:]...)
		}
	}
	return segments
}

var placeholderSegment = regexp.MustCompile(`^\{[a-z]+\}$`)

func segmentMatches(pattern, segment string) bool {
	if pattern == "*" || placeholderSegment.MatchString(pattern) {
		return true
	}
	if prefix, ok := strings.CutSuffix(pattern, "*"); ok {
		return strings.HasPrefix(segment, prefix)
	}
	return pattern == segment
}

func (r policyRule) matches(method string, segments []string) bool {
	if len(r.methods) > 0 {
		if !slices.Contains(r.methods, method) {
			return false
		}
	} else if method == "GET" || method == "HEAD" {
		return false
	}
	pattern := strings.FieldsFunc(r.path, func(c rune) bool { return c == '/' })
	if r.exact && len(segments) != len(pattern) || !r.exact && len(segments) < len(pattern) {
		return false
	}
	for i, p := range pattern {
		if !segmentMatches(p, segments[i]) {
			return false
		}
	}
	return true
}

var mutationWord = regexp.MustCompile(`\bmutation\b`)

// graphqlMutation reports a GraphQL document that holds a mutation. A mutation
// is never anonymous, so the keyword is in the text. JSON escapes are decoded
// first.
func graphqlMutation(body string) bool {
	if body == "" {
		return false
	}
	text := body
	var parsed any
	if err := json.Unmarshal([]byte(body), &parsed); err == nil {
		if encoded, err := json.Marshal(parsed); err == nil {
			text = string(encoded)
		}
	}
	return mutationWord.MatchString(text)
}

// refusal returns the refusal text for one call, or "" when the call may go.
// Only GitHub has rules.
func refusal(id, method, path, body string) string {
	if id != "github" {
		return ""
	}
	m := strings.ToUpper(strings.TrimSpace(method))
	if m == "" {
		m = "GET"
	}
	segments := routeSegments(pathSegments(path))
	var family refusalFamily
	if m == "POST" && len(segments) == 1 && segments[0] == "graphql" && graphqlMutation(body) {
		family = familyGraphQL
	} else {
		for _, rule := range githubRules {
			if rule.matches(m, segments) {
				family = rule.family
				break
			}
		}
	}
	if family == "" {
		return ""
	}
	text := refusalText[family]
	return RefusalPrefix + "github_api does not " + text[0] + ". " + text[1]
}
