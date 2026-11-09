package mcpconn

import (
	"encoding/json"
	"fmt"
	"net/url"
	"slices"
	"sort"
	"strings"
)

// App is a known remote MCP server. A card fills the URL and shows the guidance.
type App struct {
	Key         string `json:"key"`
	Name        string `json:"name"`
	Description string `json:"description"`
	URL         string `json:"url"`
	// Auth is the form that the add flow opens first: "key" or "oauth".
	Auth     string `json:"auth"`
	Guidance string `json:"guidance"`
	KeyURL   string `json:"keyUrl,omitempty"`
	Tint     string `json:"tint"`
}

const (
	doTokens   = "https://cloud.digitalocean.com/account/api/tokens" //nolint:gosec // A web page address, not a credential.
	doGuidance = "Create a DigitalOcean personal access token with only the scopes that agents need. Swarm sends it as a Bearer key."
)

// apps is Swarm Online's list of known servers (src/shared/mcp-apps.ts).
var apps = []App{
	{Key: "do-apps", Name: "DigitalOcean Apps", Description: "DigitalOcean App Platform tools.", URL: "https://apps.mcp.digitalocean.com/mcp", Auth: "key", Guidance: doGuidance, KeyURL: doTokens, Tint: "#0080ff"},
	{Key: "do-databases", Name: "DigitalOcean Databases", Description: "DigitalOcean managed database tools.", URL: "https://databases.mcp.digitalocean.com/mcp", Auth: "key", Guidance: doGuidance, KeyURL: doTokens, Tint: "#0080ff"},
	{Key: "do-droplets", Name: "DigitalOcean Droplets", Description: "DigitalOcean Droplet tools.", URL: "https://droplets.mcp.digitalocean.com/mcp", Auth: "key", Guidance: doGuidance, KeyURL: doTokens, Tint: "#0080ff"},
	{Key: "do-docs", Name: "DigitalOcean Docs", Description: "Search the DigitalOcean documentation.", URL: "https://docs.mcp.digitalocean.com/mcp", Auth: "oauth", Guidance: "Sign in with DigitalOcean in your browser.", Tint: "#0080ff"},
	{Key: "do-accounts", Name: "DigitalOcean Account", Description: "DigitalOcean account tools.", URL: "https://accounts.mcp.digitalocean.com/mcp", Auth: "key", Guidance: doGuidance, KeyURL: doTokens, Tint: "#0080ff"},
	{Key: "github", Name: "GitHub", Description: "Repositories, issues and pull requests through the GitHub MCP server.", URL: "https://api.githubcopilot.com/mcp/", Auth: "key", Guidance: "Create a fine-grained token that can use only the repositories that agents need.", KeyURL: "https://github.com/settings/personal-access-tokens/new", Tint: "#e6edf3"},
	{Key: "cloudflare", Name: "Cloudflare", Description: "Cloudflare's hosted MCP server.", URL: "https://mcp.cloudflare.com/mcp", Auth: "oauth", Guidance: "Sign in with Cloudflare in your browser, or use a Cloudflare API token instead.", KeyURL: "https://dash.cloudflare.com/profile/api-tokens", Tint: "#f6821f"},
}

// Apps returns the known servers.
func Apps() []App { return slices.Clone(apps) }

func appByKey(key string) *App {
	for i := range apps {
		if apps[i].Key == key {
			return &apps[i]
		}
	}
	return nil
}

// describe is the one line that agents read about a connection.
func describe(row Connection) string {
	if app := appByKey(row.AppKey); app != nil {
		return app.Description
	}
	if u, err := url.Parse(row.URL); err == nil && u.Host != "" {
		return "MCP server at " + u.Host
	}
	return "MCP server"
}

// localServerRefused is the text for a pasted command (stdio) server.
const localServerRefused = "Local MCP servers that run a command are not available here. Add them with `claude mcp add` instead."

// PastedEntry is one url entry of a pasted config.
type PastedEntry struct {
	Name        string
	URL         string
	HeaderNames []string
}

// Refusal is one pasted entry that Swarm did not take.
type Refusal struct {
	Name  string `json:"name"`
	Error string `json:"error"`
}

const (
	maxPasteText    = 64 * 1024
	maxPasteEntries = 20
	maxHeaders      = 20
)

// parsePastedConfig reads mcpServers. A url (or endpoint) entry becomes a
// draft with its header names. The pasted header values are never read: the
// owner types each value on the next step.
func parsePastedConfig(text string) ([]PastedEntry, []Refusal, string) {
	if strings.TrimSpace(text) == "" {
		return nil, nil, "Paste a JSON config with an mcpServers object."
	}
	if len(text) > maxPasteText {
		return nil, nil, "The config is larger than 64 KB."
	}
	var parsed map[string]json.RawMessage
	if err := json.Unmarshal([]byte(text), &parsed); err != nil {
		return nil, nil, "The text is not valid JSON."
	}
	var servers map[string]json.RawMessage
	if raw, ok := parsed["mcpServers"]; !ok || json.Unmarshal(raw, &servers) != nil || servers == nil {
		return nil, nil, "The JSON has no mcpServers object."
	}
	names := make([]string, 0, len(servers))
	for name := range servers {
		names = append(names, name)
	}
	// JSON object order is lost in a Go map; keep a stable order.
	sort.Strings(names)
	var entries []PastedEntry
	var refused []Refusal
	if len(names) > maxPasteEntries {
		refused = append(refused, Refusal{Name: "…", Error: fmt.Sprintf("Only the first %d servers were read.", maxPasteEntries)})
		names = names[:maxPasteEntries]
	}
	for _, rawName := range names {
		name := cleanName(rawName)
		if name == "" {
			name = "server"
		}
		var entry map[string]json.RawMessage
		_ = json.Unmarshal(servers[rawName], &entry)
		if _, ok := entry["command"]; ok {
			refused = append(refused, Refusal{Name: name, Error: localServerRefused})
			continue
		}
		pick := func(key string) string {
			var s string
			if raw, ok := entry[key]; ok && json.Unmarshal(raw, &s) == nil {
				return strings.TrimSpace(s)
			}
			return ""
		}
		u := pick("url")
		if u == "" {
			u = pick("endpoint")
		}
		if u == "" {
			refused = append(refused, Refusal{Name: name, Error: "This entry has no url."})
			continue
		}
		if len(u) > maxURLLength {
			refused = append(refused, Refusal{Name: name, Error: fmt.Sprintf("The url is longer than %d characters.", maxURLLength)})
			continue
		}
		// Only the keys are read. A value is never touched.
		var headers map[string]json.RawMessage
		if raw, ok := entry["headers"]; ok {
			_ = json.Unmarshal(raw, &headers)
		}
		keys := make([]string, 0, len(headers))
		for k := range headers {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		var headerNames []string
		refusal := ""
		if len(keys) > maxHeaders {
			refusal = fmt.Sprintf("The entry has more than %d headers.", maxHeaders)
		}
		for _, h := range keys {
			if refusal != "" {
				break
			}
			if refusal = checkHeaderName(h); refusal != "" {
				break
			}
			if !slices.ContainsFunc(headerNames, func(n string) bool { return strings.EqualFold(n, h) }) {
				headerNames = append(headerNames, h)
			}
		}
		if refusal != "" {
			refused = append(refused, Refusal{Name: name, Error: refusal})
			continue
		}
		entries = append(entries, PastedEntry{Name: name, URL: u, HeaderNames: headerNames})
	}
	return entries, refused, ""
}

// grantAllows: nil (the add flow has not finished) never allows; all allows
// every session.
func grantAllows(projects *Projects, project string) bool {
	if projects == nil {
		return false
	}
	if projects.All {
		return true
	}
	return project != "" && slices.Contains(projects.List, project)
}

// cleanProjects checks the owner's answer to "Which projects can use this?".
func cleanProjects(input Projects, known []string) (Projects, error) {
	if input.All {
		return Projects{All: true}, nil
	}
	seen := map[string]bool{}
	var out []string
	for _, p := range input.List {
		if !slices.Contains(known, p) {
			return Projects{}, ownerMessage("A chosen project is not a Swarm project.")
		}
		if !seen[p] {
			seen[p] = true
			out = append(out, p)
		}
	}
	sort.Strings(out)
	if out == nil {
		out = []string{}
	}
	return Projects{List: out}, nil
}
