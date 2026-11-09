package cli

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// connectionsDaemon serves the connection routes `ao mcp` uses.
func connectionsDaemon(t *testing.T, tools string, calls *[]connectionCallRequest) {
	t.Helper()
	cfg := setConfigEnv(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/connections/agent-tools":
			_, _ = io.WriteString(w, tools)
		case "/api/v1/connections/call":
			var request connectionCallRequest
			_ = json.NewDecoder(r.Body).Decode(&request)
			*calls = append(*calls, request)
			if request.Path == "/fail" {
				w.WriteHeader(http.StatusForbidden)
				_, _ = io.WriteString(w, `{"error":"forbidden","code":"CONNECTION_KEYCHAIN_DENIED","message":"the keychain refused access","requestId":"r"}`)
				return
			}
			_, _ = io.WriteString(w, `{"text":"GET /user -> 200 OK\n{\"login\":\"me\"}"}`)
		}
	}))
	t.Cleanup(srv.Close)
	writeRunFileFor(t, cfg, srv)
}

const connectionToolsBody = `{"tools":[{"name":"github_api","description":"Call GitHub as the user.","inputSchema":{"type":"object","properties":{"path":{"type":"string"}},"required":["path"]}}]}`

func TestMCPOffersTheConnectedServicesTools(t *testing.T) {
	var calls []connectionCallRequest
	connectionsDaemon(t, connectionToolsBody, &calls)
	listed := runMCP(t, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)[0].Result.Tools
	if len(listed) != 3 || listed[2].Name != "github_api" || listed[2].InputSchema.Required[0] != "path" {
		t.Fatalf("tools = %+v", listed)
	}
	if listed[2].Annotations["readOnlyHint"] != false || listed[2].Annotations["destructiveHint"] != true {
		t.Fatalf("annotations = %+v", listed[2].Annotations)
	}
}

func TestMCPConnectionCallGoesThroughTheDaemon(t *testing.T) {
	var calls []connectionCallRequest
	connectionsDaemon(t, connectionToolsBody, &calls)
	// A harness can call a tool before it asks for the list.
	replies := runMCP(t,
		mcpCall("github_api", map[string]any{"path": "/user", "method": "GET", "fields": "login"}),
		mcpCall("github_api", map[string]any{"path": "/graphql", "method": "POST", "body": map[string]any{"query": "{ viewer { login } }"}}),
		mcpCall("github_api", map[string]any{"path": "/fail"}),
		mcpCall("gitlab_api", map[string]any{"path": "/user"}),
	)
	if r := replies[0].Result; r.IsError || !strings.HasPrefix(r.Content[0].Text, "GET /user -> 200 OK") {
		t.Fatalf("first reply = %+v", replies[0])
	}
	if len(calls) != 3 || calls[0].Tool != "github_api" || calls[0].Fields != "login" {
		t.Fatalf("calls = %+v", calls)
	}
	if calls[1].Body != `{"query":"{ viewer { login } }"}` {
		t.Fatalf("an object body was sent as %q", calls[1].Body)
	}
	if r := replies[2].Result; !r.IsError || !strings.Contains(r.Content[0].Text, "keychain refused access") {
		t.Fatalf("daemon error reply = %+v", replies[2])
	}
	if replies[3].Error == nil || !strings.Contains(replies[3].Error.Message, `unknown tool "gitlab_api"`) {
		t.Fatalf("unknown tool reply = %+v", replies[3])
	}
}
