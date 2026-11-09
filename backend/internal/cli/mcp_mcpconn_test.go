package cli

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const mcpIndexBody = `{"version":"v1","connections":[{"name":"Sanity","prefix":"sanity","tools":[
	{"tool":"sanity__query","remote":"query","description":"[Sanity] Run a query.","inputSchema":{"type":"object","properties":{"q":{"type":"string"}},"required":["q"]}},
	{"tool":"bad name","remote":"x","description":"skipped","inputSchema":{"type":"object"}}]}]}`

// mcpConnDaemon serves the /mcp/agent routes that `ao mcp` uses.
func mcpConnDaemon(t *testing.T, index string, calls *[]mcpAgentRequest, paths *[]string) {
	t.Helper()
	cfg := setConfigEnv(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		*paths = append(*paths, r.URL.Path)
		switch r.URL.Path {
		case "/api/v1/mcp/agent/index":
			if index == "" {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			_, _ = io.WriteString(w, index)
		case "/api/v1/mcp/agent/list":
			_, _ = io.WriteString(w, `{"result":{"connections":[{"name":"Sanity","tools":1}]}}`)
		case "/api/v1/mcp/agent/tools":
			_, _ = io.WriteString(w, `{"result":"Linear is not available in this project."}`)
		case "/api/v1/mcp/agent/call":
			var request mcpAgentRequest
			_ = json.NewDecoder(r.Body).Decode(&request)
			*calls = append(*calls, request)
			_, _ = io.WriteString(w, `{"result":{"content":[{"type":"text","text":"rows: 3"},{"type":"widget"}],"isError":false}}`)
		}
	}))
	t.Cleanup(srv.Close)
	writeRunFileFor(t, cfg, srv)
}

func TestMCPShowsGeneralAndRealConnectionTools(t *testing.T) {
	var calls []mcpAgentRequest
	var paths []string
	t.Setenv("AO_SESSION_ID", "blog-1")
	mcpConnDaemon(t, mcpIndexBody, &calls, &paths)
	replies := runMCP(t,
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18"}}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`,
		mcpCall("sanity__query", map[string]any{"q": "*"}),
		mcpCall("connections_list", nil),
		mcpCall("connection_tools", map[string]any{"name": "Linear"}),
	)
	if !strings.Contains(string(replies[0].Result.Capabilities["tools"]), `"listChanged":true`) {
		t.Fatalf("capabilities = %s", replies[0].Result.Capabilities["tools"])
	}
	var names []string
	for _, tool := range replies[1].Result.Tools {
		names = append(names, tool.Name)
	}
	if strings.Join(names, " ") != "html_preview html_render connections_list connection_tools connection_call sanity__query" {
		t.Fatalf("tools = %v", names)
	}
	if r := replies[2].Result; r.IsError || len(r.Content) != 2 || r.Content[0].Text != "rows: 3" || r.Content[1].Type != "text" {
		t.Fatalf("real tool reply = %+v", replies[2])
	}
	if len(calls) != 1 || calls[0].SessionID != "blog-1" || calls[0].Name != "Sanity" || calls[0].Tool != "query" {
		t.Fatalf("calls = %+v", calls)
	}
	if !strings.Contains(replies[3].Result.Content[0].Text, `"name":"Sanity"`) {
		t.Fatalf("connections_list = %+v", replies[3])
	}
	if replies[4].Result.Content[0].Text != "Linear is not available in this project." {
		t.Fatalf("connection_tools = %+v", replies[4])
	}
}

func TestMCPHasNoConnectionToolsWhenTheDaemonHasNoIndex(t *testing.T) {
	var calls []mcpAgentRequest
	var paths []string
	mcpConnDaemon(t, "", &calls, &paths)
	replies := runMCP(t, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`, mcpCall("connections_list", nil))
	if len(replies[0].Result.Tools) != 2 {
		t.Fatalf("tools = %+v", replies[0].Result.Tools)
	}
	if replies[1].Error == nil {
		t.Fatalf("connections_list with no index = %+v", replies[1])
	}
}

func TestRemoteResultTurnsTextIntoAnError(t *testing.T) {
	r := remoteResult("Sanity did not answer in time.")
	if r["isError"] != true {
		t.Fatalf("result = %+v", r)
	}
	r = remoteResult(map[string]any{"content": []any{}, "structuredContent": map[string]any{"n": 1.0}})
	if text := r["content"].([]any)[0].(map[string]any)["text"]; text != `{"n":1}` {
		t.Fatalf("structured = %+v", r)
	}
}
