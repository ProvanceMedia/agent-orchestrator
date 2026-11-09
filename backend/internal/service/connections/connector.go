// Package connections holds the API keys of third-party services (Cloudflare,
// GitHub, Stripe, OpenAI and others) and makes calls to those services for
// agents. Each connected service gives the agents one tool, for example
// cloudflare_api. The agent sends a path; the daemon adds the key and makes the
// call. The key never goes into the agent's context, its environment, or an
// API response.
//
// The design follows Swarm Online (swarm-coder src/main/connections.ts). The
// difference is the store: here the keys are in the macOS login keychain, in
// items that only the ao binary can read without a prompt.
package connections

import (
	"encoding/base64"
	"fmt"
	"net/url"
	"slices"
	"strings"
)

// BodyType is how a connector encodes a request body.
type BodyType string

// The body types.
const (
	BodyJSON BodyType = "json"
	BodyForm BodyType = "form"
	BodyNone BodyType = "none"
)

// Field is one value that the user gives for a connector.
type Field struct {
	Key         string `json:"key"`
	Label       string `json:"label"`
	Placeholder string `json:"placeholder,omitempty"`
	// Secret values are masked on screen and never leave the daemon after save.
	Secret   bool `json:"secret,omitempty"`
	Optional bool `json:"optional,omitempty"`
}

// TestCall is a cheap authenticated call that proves a key when it is saved.
type TestCall struct {
	Path   string
	Method string
	Body   string
}

// Connector is one service that Swarm can hold a key for.
type Connector struct {
	ID    string
	Name  string
	Tool  string
	Tint  string
	Blurb string
	// KeyURL is where the user makes the key.
	KeyURL string
	Fields []Field
	// Base is the API host and base path. BaseFor replaces it when the user
	// gives the host or the region.
	Base     string
	BaseFor  func(values map[string]string) (string, error)
	Auth     func(values map[string]string) map[string]string
	Methods  []string
	BodyType BodyType
	Test     *TestCall
	// Guide is the tool description that the agent reads.
	Guide string
}

// Catalogue returns every connector, in display order.
func Catalogue() []Connector {
	return slices.Clone(catalogue)
}

// Lookup finds a connector by its id.
func Lookup(id string) (Connector, bool) {
	for _, c := range catalogue {
		if c.ID == id {
			return c, true
		}
	}
	return Connector{}, false
}

// LookupTool finds the connector that owns an agent tool name.
func LookupTool(tool string) (Connector, bool) {
	for _, c := range catalogue {
		if c.Tool == tool {
			return c, true
		}
	}
	return Connector{}, false
}

func (c Connector) baseURL(values map[string]string) (string, error) {
	base := c.Base
	if c.BaseFor != nil {
		var err error
		if base, err = c.BaseFor(values); err != nil {
			return "", err
		}
	}
	return strings.TrimRight(base, "/"), nil
}

func (c Connector) allows(method string) bool {
	return slices.Contains(c.Methods, method)
}

func (c Connector) secretKeys() []string {
	var keys []string
	for _, f := range c.Fields {
		if f.Secret {
			keys = append(keys, f.Key)
		}
	}
	return keys
}

func bearerAuth(v map[string]string) map[string]string {
	return map[string]string{"Authorization": "Bearer " + v["key"]}
}

// rawAuth sends the key as the whole Authorization value (Linear).
func rawAuth(v map[string]string) map[string]string {
	return map[string]string{"Authorization": v["key"]}
}

func githubAuth(v map[string]string) map[string]string {
	return map[string]string{
		"Authorization":        "Bearer " + v["key"],
		"Accept":               "application/vnd.github+json",
		"X-GitHub-Api-Version": "2022-11-28",
	}
}

func anthropicAuth(v map[string]string) map[string]string {
	return map[string]string{"x-api-key": v["key"], "anthropic-version": "2023-06-01"}
}

func headerAuth(name string) func(map[string]string) map[string]string {
	return func(v map[string]string) map[string]string { return map[string]string{name: v["key"]} }
}

// basicAuth is HTTP Basic with a fixed user name and the key as the password.
func basicAuth(user string) func(map[string]string) map[string]string {
	return func(v map[string]string) map[string]string {
		token := base64.StdEncoding.EncodeToString([]byte(user + ":" + v["key"]))
		return map[string]string{"Authorization": "Basic " + token}
	}
}

func mailgunBase(v map[string]string) (string, error) {
	if strings.EqualFold(strings.TrimSpace(v["region"]), "eu") {
		return "https://api.eu.mailgun.net", nil
	}
	return "https://api.mailgun.net", nil
}

const posthogDefaultHost = "https://app.posthog.com"

// posthogBase accepts only an https origin, so a typing error cannot send the
// key in clear text or to a path on another service.
func posthogBase(v map[string]string) (string, error) {
	host := strings.TrimSpace(v["host"])
	if host == "" {
		return posthogDefaultHost, nil
	}
	u, err := url.Parse(host)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil ||
		strings.Trim(u.Path, "/") != "" || u.RawQuery != "" || u.Fragment != "" {
		return "", fmt.Errorf("the PostHog host must be an https address with no path, for example https://eu.posthog.com")
	}
	return "https://" + u.Host, nil
}
