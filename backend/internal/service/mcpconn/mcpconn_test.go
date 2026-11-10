package mcpconn

import (
	"context"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/service/connections"
)

// memVault is an in-memory secret store.
type memVault struct {
	mu    sync.Mutex
	items map[string]map[string]string
}

func (m *memVault) Available() bool { return true }
func (m *memVault) Get(_ context.Context, id string) (map[string]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	v, ok := m.items[id]
	if !ok {
		return nil, connections.ErrSecretNotFound
	}
	return v, nil
}
func (m *memVault) Put(_ context.Context, id string, v map[string]string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.items[id] = v
	return nil
}
func (m *memVault) Delete(_ context.Context, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.items, id)
	return nil
}

// fakeServer is a remote MCP server with optional key or OAuth auth.
type fakeServer struct {
	t         *testing.T
	srv       *httptest.Server
	mu        sync.Mutex
	auth      string // "", "key", "oauth"
	key       string
	tokens    map[string]bool
	echo      string // a value the tools/call result repeats
	issued    int
	registers int
	sse       bool
	lastAuth  string
}

func newFakeServer(t *testing.T, auth string) *fakeServer {
	f := &fakeServer{t: t, auth: auth, key: "key-1234567890", tokens: map[string]bool{}}
	mux := http.NewServeMux()
	mux.HandleFunc("/mcp", f.mcp)
	mux.HandleFunc("/.well-known/oauth-protected-resource/mcp", func(w http.ResponseWriter, r *http.Request) {
		if f.auth != "oauth" {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"resource": f.srv.URL + "/mcp", "authorization_servers": []string{f.srv.URL}})
	})
	mux.HandleFunc("/.well-known/oauth-authorization-server", func(w http.ResponseWriter, r *http.Request) {
		if f.auth != "oauth" {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"issuer": f.srv.URL, "authorization_endpoint": f.srv.URL + "/authorize", "token_endpoint": f.srv.URL + "/token",
			"registration_endpoint": f.srv.URL + "/register", "code_challenge_methods_supported": []string{"S256"},
			"token_endpoint_auth_methods_supported": []string{"none"},
		})
	})
	mux.HandleFunc("/register", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.registers++
		f.mu.Unlock()
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{"client_id": "client-1", "token_endpoint_auth_method": "none"})
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		f.mu.Lock()
		defer f.mu.Unlock()
		switch r.Form.Get("grant_type") {
		case "authorization_code":
			if r.Form.Get("code") != "good-code" || r.Form.Get("code_verifier") == "" || r.Form.Get("resource") != f.srv.URL+"/mcp" {
				w.WriteHeader(http.StatusBadRequest)
				_ = json.NewEncoder(w).Encode(map[string]any{"error": "invalid_grant"})
				return
			}
		case "refresh_token":
			if r.Form.Get("refresh_token") != "refresh-1" {
				w.WriteHeader(http.StatusBadRequest)
				_ = json.NewEncoder(w).Encode(map[string]any{"error": "invalid_grant"})
				return
			}
		}
		f.issued++
		token := fmt.Sprintf("access-token-%d", f.issued)
		f.tokens[token] = true
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": token, "token_type": "Bearer", "expires_in": 3600, "refresh_token": "refresh-1"})
	})
	f.srv = httptest.NewTLSServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeServer) url() string { return f.srv.URL + "/mcp" }

func (f *fakeServer) mcp(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	auth, key, sse, echo := f.auth, f.key, f.sse, f.echo
	f.lastAuth = r.Header.Get("Authorization")
	allowed := auth == "" || (auth == "key" && r.Header.Get("Authorization") == "Bearer "+key) ||
		(auth == "oauth" && f.tokens[strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")])
	f.mu.Unlock()
	if !allowed {
		if auth == "oauth" {
			w.Header().Set("WWW-Authenticate", fmt.Sprintf(`Bearer resource_metadata="%s/.well-known/oauth-protected-resource/mcp", scope="read write"`, f.srv.URL))
		}
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	var msg map[string]any
	_ = json.NewDecoder(r.Body).Decode(&msg)
	id, hasID := msg["id"]
	if !hasID {
		w.WriteHeader(http.StatusAccepted)
		return
	}
	var result any
	switch msg["method"] {
	case "initialize":
		w.Header().Set("Mcp-Session-Id", "sess-1")
		result = map[string]any{"protocolVersion": "2025-06-18", "capabilities": map[string]any{}}
	case "tools/list":
		params, _ := msg["params"].(map[string]any)
		if params["cursor"] == nil {
			result = map[string]any{"tools": []any{
				map[string]any{"name": "echo", "description": "Echo the text.", "inputSchema": map[string]any{"type": "object", "properties": map[string]any{"text": map[string]any{"type": "string"}}, "required": []any{"text"}}},
				map[string]any{"name": "pick", "description": "Pick one.", "inputSchema": map[string]any{"type": "object", "oneOf": []any{map[string]any{"type": "object"}}}},
			}, "nextCursor": "page-2"}
		} else {
			result = map[string]any{"tools": []any{map[string]any{"name": "fail", "description": "Always fails."}}}
		}
	case "tools/call":
		params, _ := msg["params"].(map[string]any)
		if params["name"] == "fail" {
			writeReply(w, sse, map[string]any{"jsonrpc": "2.0", "id": id, "error": map[string]any{"code": -32000, "message": "boom " + echo}})
			return
		}
		args, _ := params["arguments"].(map[string]any)
		text := fmt.Sprint(args["text"]) + " " + echo
		result = map[string]any{"content": []any{map[string]any{"type": "text", "text": strings.TrimSpace(text)}}}
	}
	writeReply(w, sse, map[string]any{"jsonrpc": "2.0", "id": id, "result": result})
}

func writeReply(w http.ResponseWriter, sse bool, reply map[string]any) {
	raw, _ := json.Marshal(reply)
	if sse {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprintf(w, "event: ping\r\ndata: {}\r\n\r\n: comment\nevent: message\ndata: %s\n\n", raw)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(raw)
}

func testPolicy(servers ...*httptest.Server) GuardPolicy {
	pool := x509.NewCertPool()
	for _, s := range servers {
		pool.AddCert(s.Certificate())
	}
	return GuardPolicy{
		Resolve: func(context.Context, string) ([]netip.Addr, error) {
			return []netip.Addr{netip.MustParseAddr("127.0.0.1")}, nil
		},
		Refused: func(netip.Addr) bool { return false },
		RootCAs: pool,
	}
}

func newTestService(t *testing.T, servers ...*httptest.Server) (*Service, string) {
	t.Helper()
	dir := t.TempDir()
	svc := New(dir, &memVault{items: map[string]map[string]string{}}, Deps{
		Projects:    func(context.Context) []string { return []string{"blog", "shop"} },
		RedirectURI: func() string { return "http://127.0.0.1:3001/api/v1/mcp-oauth/callback" },
		HTTP:        NewGuardedHTTP(testPolicy(servers...)),
	})
	return svc, dir
}

func TestCheckURL(t *testing.T) {
	for raw, want := range map[string]GuardCode{
		"http://example.com/mcp":          GuardURLNotHTTPS,
		"https://user:pw@example.com/mcp": GuardURLCredentials,
		"https://example.com/mcp#x":       GuardURLInvalid,
		"example.com/mcp":                 GuardURLInvalid,
		"":                                GuardURLInvalid,
	} {
		_, err := CheckURL(raw)
		var guard *GuardError
		if !errors.As(err, &guard) || guard.Code != want {
			t.Errorf("CheckURL(%q) = %v, want %s", raw, err, want)
		}
	}
	if _, err := CheckURL("https://mcp.example.com/mcp?x=1"); err != nil {
		t.Fatal(err)
	}
}

func TestRefusedAddress(t *testing.T) {
	for addr, refused := range map[string]bool{
		"127.0.0.1": true, "10.1.2.3": true, "192.168.0.105": true, "100.101.102.103": true, "169.254.1.1": true,
		"::1": true, "fd7a:115c:a1e0::1": true, "::ffff:10.0.0.1": true, "fe80::1": true,
		"8.8.8.8": false, "1.1.1.1": false, "2606:4700::1111": false, "::ffff:8.8.8.8": false,
	} {
		if got := RefusedAddress(netip.MustParseAddr(addr)); got != refused {
			t.Errorf("RefusedAddress(%s) = %v, want %v", addr, got, refused)
		}
	}
}

func TestProductionGuardRefusesLocalServer(t *testing.T) {
	f := newFakeServer(t, "")
	svc, _ := newTestService(t)
	svc.client.http = NewGuardedHTTP(ProductionPolicy)
	svc.oauth.http = svc.client.http
	r := svc.Check(context.Background(), CheckInput{URL: f.url()})
	if r.OK || r.Field != "url" || !strings.Contains(r.Error, "private or local network") {
		t.Fatalf("Check = %+v", r)
	}
	if len(svc.store.List(context.Background())) != 0 {
		t.Fatal("a refused URL made a row")
	}
}

func TestOpenServerFlow(t *testing.T) {
	f := newFakeServer(t, "")
	f.sse = true
	svc, _ := newTestService(t, f.srv)
	ctx := context.Background()

	r := svc.Check(ctx, CheckInput{URL: f.url(), Name: "Docs Server"})
	if !r.OK || r.Outcome != "open" || len(r.Connection.Tools) != 3 || r.Connection.Health != HealthDraft {
		t.Fatalf("Check = %+v", r)
	}
	if index := svc.Index(ctx, "blog"); len(index.Connections) != 0 {
		t.Fatalf("a draft is in the index: %+v", index)
	}
	if p := svc.SetProjects(ctx, r.ID, Projects{All: true}); !p.OK || p.Connection.Health != HealthOK {
		t.Fatalf("SetProjects = %+v", p)
	}
	index := svc.Index(ctx, "blog")
	if len(index.Connections) != 1 || index.Connections[0].Prefix != "docs_server" || len(index.Connections[0].Tools) != 3 {
		t.Fatalf("index = %+v", index)
	}
	names := map[string]RealTool{}
	for _, tool := range index.Connections[0].Tools {
		names[tool.Tool] = tool
	}
	echo, ok := names["docs_server__echo"]
	if !ok || !strings.HasPrefix(echo.Description, "[Docs Server] Echo the text.") || !strings.HasSuffix(echo.Description, dataNote) {
		t.Fatalf("echo tool = %+v", echo)
	}
	pick := names["docs_server__pick"]
	if !strings.Contains(pick.Description, schemaNote) || fmt.Sprint(pick.InputSchema) != "map[type:object]" {
		t.Fatalf("pick tool must get an open schema: %+v", pick)
	}
	if again := svc.Index(ctx, "blog"); again.Version != index.Version {
		t.Fatal("the version changed with no change")
	}

	result, err := svc.Call(ctx, "blog", "docs server", "echo", map[string]any{"text": "hi"})
	if err != nil || !strings.Contains(fmt.Sprint(result), "hi") {
		t.Fatalf("Call = %v, %v", result, err)
	}
	text, _ := svc.Call(ctx, "blog", "Docs Server", "nope", nil)
	if text != "Docs Server has no tool called nope. Call connection_tools first." {
		t.Fatalf("unknown tool = %v", text)
	}
	text, _ = svc.Call(ctx, "blog", "Docs Server", "fail", nil)
	if s, _ := text.(string); !strings.HasPrefix(s, "Docs Server returned an error: boom") || !strings.Contains(s, "(code -32000)") {
		t.Fatalf("rpc error = %v", text)
	}
	if list := svc.List(ctx, "shop"); len(list) != 1 || list[0].Tools != 3 || list[0].ToolPrefix != "docs_server" {
		t.Fatalf("List = %+v", list)
	}
	if text := svc.PromptLines(ctx, "blog"); !strings.Contains(text, "- Docs Server: tools named docs_server__<tool>") || !strings.Contains(text, "call connections_list") {
		t.Fatalf("PromptLines = %q", text)
	}
}

func TestPromptRuleStaysWithNoConnection(t *testing.T) {
	svc, _ := newTestService(t)
	text := svc.PromptLines(context.Background(), "blog")
	if !strings.Contains(text, "Before you say that a service is not connected, or ask the owner for a key, call connections_list.") {
		t.Fatalf("PromptLines = %q", text)
	}
}

func TestKeyServerFlow(t *testing.T) {
	f := newFakeServer(t, "key")
	svc, dir := newTestService(t, f.srv)
	ctx := context.Background()

	r := svc.Check(ctx, CheckInput{URL: f.url()})
	if !r.OK || r.Outcome != "needs_key" || r.Connection.AuthKind != AuthHeaders {
		t.Fatalf("Check = %+v", r)
	}
	if p := svc.SetProjects(ctx, r.ID, Projects{All: true}); p.OK || p.Error != "Save a key or headers first." {
		t.Fatalf("SetProjects before a key = %+v", p)
	}
	bad := svc.SaveCredentials(ctx, r.ID, CredentialsInput{Key: "wrong-key-000000"})
	if bad.OK || bad.Field != "key" || !strings.Contains(bad.Error, "did not accept") {
		t.Fatalf("wrong key = %+v", bad)
	}
	for _, h := range []Header{{Name: "Cookie", Value: "x"}, {Name: "Bad Name", Value: "x"}, {Name: "X-Key", Value: "a\nb"}} {
		if res := svc.SaveCredentials(ctx, r.ID, CredentialsInput{Headers: []Header{h}}); res.OK || res.Field != "header" {
			t.Errorf("header %+v = %+v", h, res)
		}
	}
	good := svc.SaveCredentials(ctx, r.ID, CredentialsInput{Key: f.key})
	if !good.OK || good.Connection.Secrets["header.Authorization"] != "set" || len(good.Connection.Tools) != 3 {
		t.Fatalf("good key = %+v", good)
	}
	if p := svc.SetProjects(ctx, r.ID, Projects{List: []string{"blog"}}); !p.OK {
		t.Fatalf("SetProjects = %+v", p)
	}
	if p := svc.SetProjects(ctx, r.ID, Projects{List: []string{"nope"}}); p.OK {
		t.Fatal("an unknown project was taken")
	}
	if len(svc.Index(ctx, "shop").Connections) != 0 || len(svc.Index(ctx, "").Connections) != 0 {
		t.Fatal("another project or no project sees the connection")
	}
	text, _ := svc.Call(ctx, "shop", r.Connection.Name, "echo", map[string]any{"text": "x"})
	if !strings.HasSuffix(fmt.Sprint(text), "is not available in this project.") {
		t.Fatalf("shop call = %v", text)
	}

	// The server repeats the key in its result. The agent must not see it.
	f.mu.Lock()
	f.echo = "the key is " + f.key
	f.mu.Unlock()
	result, err := svc.Call(ctx, "blog", r.Connection.Name, "echo", map[string]any{"text": "hello"})
	raw, _ := json.Marshal(result)
	if err != nil || strings.Contains(string(raw), f.key) || !strings.Contains(string(raw), Redacted) {
		t.Fatalf("Call = %s, %v", raw, err)
	}
	text, _ = svc.Call(ctx, "blog", r.Connection.Name, "fail", nil)
	if strings.Contains(fmt.Sprint(text), f.key) {
		t.Fatalf("an error text holds the key: %v", text)
	}

	file, _ := os.ReadFile(dir + "/mcp-connections.json")
	if strings.Contains(string(file), f.key) || strings.Contains(string(file), "127.0.0.1") {
		t.Fatal("the store file holds plain text")
	}
	overview, _ := json.Marshal(svc.Overview(ctx))
	if strings.Contains(string(overview), f.key) {
		t.Fatal("the overview holds the key")
	}

	// Another key (a new keychain) cannot read the rows.
	other := NewStore(dir, &memVault{items: map[string]map[string]string{}})
	if _, err := other.sealKey(ctx); err == nil {
		t.Fatal("a new seal key was made over an existing file")
	}
	if ok, msg := svc.Remove(r.ID); !ok || msg != "" {
		t.Fatalf("Remove = %v %q", ok, msg)
	}
}

func TestOAuthFlow(t *testing.T) {
	f := newFakeServer(t, "oauth")
	svc, _ := newTestService(t, f.srv)
	ctx := context.Background()

	r := svc.Check(ctx, CheckInput{URL: f.url(), Name: "Sanity"})
	if !r.OK || r.Outcome != "oauth" || r.Connection.AuthKind != AuthOAuth {
		t.Fatalf("Check = %+v", r)
	}
	start := svc.StartOAuth(ctx, r.ID, nil)
	if !start.OK {
		t.Fatalf("StartOAuth = %+v", start)
	}
	auth, _ := url.Parse(start.URL)
	q := auth.Query()
	if auth.Path != "/authorize" || q.Get("client_id") != "client-1" || q.Get("code_challenge_method") != "S256" ||
		q.Get("redirect_uri") != "http://127.0.0.1:3001/api/v1/mcp-oauth/callback" || q.Get("scope") != "read write" || q.Get("resource") != f.url() {
		t.Fatalf("authorize URL = %s", start.URL)
	}
	state := q.Get("state")

	if fin := svc.FinishOAuth(ctx, url.Values{"state": {"made-up"}, "code": {"good-code"}}); fin.OK {
		t.Fatal("a made-up state finished")
	}
	fin := svc.FinishOAuth(ctx, url.Values{"state": {state}, "code": {"good-code"}, "iss": {f.srv.URL}})
	if !fin.OK || fin.ID != r.ID {
		t.Fatalf("FinishOAuth = %+v", fin)
	}
	if again := svc.FinishOAuth(ctx, url.Values{"state": {state}, "code": {"good-code"}}); again.OK || !strings.Contains(again.Message, "used already") {
		t.Fatalf("a state worked twice: %+v", again)
	}
	row, _ := svc.store.Get(ctx, r.ID)
	if row.OAuth == nil || row.OAuth.Status != oauthStatusOK || len(row.Tools) != 3 || row.Secrets[secretAccessToken] != "access-token-1" {
		t.Fatalf("row after sign-in = %+v", row)
	}
	if p := svc.SetProjects(ctx, r.ID, Projects{All: true}); !p.OK {
		t.Fatalf("SetProjects = %+v", p)
	}

	// The token ends within 60 s, so the next call refreshes it first.
	if _, _, err := svc.store.Update(ctx, r.ID, func(c *Connection) {
		soon := time.Now().Add(10 * time.Second).UnixMilli()
		c.OAuth.AccessTokenExpiresAt = &soon
	}); err != nil {
		t.Fatal(err)
	}
	result, err := svc.Call(ctx, "blog", "Sanity", "echo", map[string]any{"text": "ok"})
	if err != nil || !strings.Contains(fmt.Sprint(result), "ok") {
		t.Fatalf("Call = %v, %v", result, err)
	}
	f.mu.Lock()
	lastAuth := f.lastAuth
	f.mu.Unlock()
	if lastAuth != "Bearer access-token-2" {
		t.Fatalf("the call used %q, want the refreshed token", lastAuth)
	}

	// The server forgets every token: one 401 forces a refresh, and the refused
	// refresh token asks the owner to sign in again.
	f.mu.Lock()
	f.tokens = map[string]bool{}
	f.mu.Unlock()
	if _, _, err := svc.store.Update(ctx, r.ID, func(c *Connection) { c.Secrets[secretRefreshToken] = "revoked" }); err != nil {
		t.Fatal(err)
	}
	text, _ := svc.Call(ctx, "blog", "Sanity", "echo", map[string]any{"text": "x"})
	if text != "Sanity needs the owner to sign in again in Settings > Connections." {
		t.Fatalf("Call after revoke = %v", text)
	}
	if index := svc.Index(ctx, "blog"); index.Connections[0].Health != HealthNeedsSignIn {
		t.Fatalf("health = %s", index.Connections[0].Health)
	}
	if text := svc.PromptLines(ctx, "blog"); !strings.Contains(text, "the owner must sign in again") {
		t.Fatalf("PromptLines = %q", text)
	}
}

func TestRedirectDropsCredentialsToAnotherOrigin(t *testing.T) {
	var seen string
	other := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r.Header.Get("Authorization") + "|" + r.Header.Get("X-Api-Key")
		_, _ = io.WriteString(w, "{}")
	}))
	defer other.Close()
	first := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL+"/landing", http.StatusFound)
	}))
	defer first.Close()
	do := NewGuardedHTTP(testPolicy(first, other))
	res, err := do(context.Background(), GuardedRequest{URL: first.URL, Timeout: 5 * time.Second,
		Headers: map[string]string{"Authorization": "Bearer secret-1", "X-Api-Key": "secret-2"}, CredentialHeaders: []string{"X-Api-Key"}})
	if err != nil {
		t.Fatal(err)
	}
	res.Close()
	if seen != "|" {
		t.Fatalf("the other origin got %q", seen)
	}

	loop := httptest.NewTLSServer(nil)
	loop.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, "/again", http.StatusFound) })
	defer loop.Close()
	_, err = NewGuardedHTTP(testPolicy(loop))(context.Background(), GuardedRequest{URL: loop.URL, Timeout: 5 * time.Second})
	var guard *GuardError
	if !errors.As(err, &guard) || guard.Code != GuardTooManyRedirects {
		t.Fatalf("redirect loop = %v", err)
	}
}

func TestPasteKeepsNoHeaderValue(t *testing.T) {
	svc, dir := newTestService(t)
	text := `{"mcpServers":{"remote":{"url":"https://mcp.example.com/mcp?key=abc12345678","headers":{"X-Api-Key":"value-9876543210"}},
		"local":{"command":"npx","args":["thing"]},"plain":{"http":"x"}}}`
	r := svc.Paste(context.Background(), text)
	if !r.OK || len(r.Drafts) != 1 || len(r.Refused) != 2 {
		t.Fatalf("Paste = %+v", r)
	}
	d := r.Drafts[0]
	if d.URL != "https://mcp.example.com/mcp?key=••••" || !d.URLHasSecret || d.AuthKind != AuthHeaders || d.HeaderNames[0] != "X-Api-Key" {
		t.Fatalf("draft = %+v", d)
	}
	raw, _ := json.Marshal(r)
	file, _ := os.ReadFile(dir + "/mcp-connections.json")
	if strings.Contains(string(raw), "value-9876543210") || strings.Contains(string(raw), "abc12345678") || strings.Contains(string(file), "value-9876543210") {
		t.Fatal("a pasted value was kept or shown")
	}
}

func TestToolIndexNamesAndLimits(t *testing.T) {
	long := strings.Repeat("x", 80)
	index := buildToolIndex([]indexRow{
		{Name: "Sanity", Tools: []Tool{{Name: "query", InputSchema: map[string]any{"type": "object"}}, {Name: long, InputSchema: map[string]any{"type": "object"}}}},
		{Name: "sanity!", Tools: []Tool{{Name: "query", InputSchema: map[string]any{"type": "object", "properties": map[string]any{"q": map[string]any{"type": "string", "$comment": "remote text", "description": "Query\u200b text"}}}}}},
	})
	a, b := index.Connections[0], index.Connections[1]
	if a.Prefix != "sanity" || b.Prefix == "sanity" || !strings.HasPrefix(b.Prefix, "sanity_") {
		t.Fatalf("prefixes = %q, %q", a.Prefix, b.Prefix)
	}
	for _, c := range index.Connections {
		for _, tool := range c.Tools {
			if len(tool.Tool) > maxToolName || !strings.Contains(tool.Tool, "__") {
				t.Errorf("tool name %q", tool.Tool)
			}
		}
	}
	prop := b.Tools[0].InputSchema.(map[string]any)["properties"].(map[string]any)["q"].(map[string]any)
	if _, kept := prop["$comment"]; kept || prop["description"] != "Query  text" {
		t.Fatalf("schema = %+v", prop)
	}
}

func TestScrubSecrets(t *testing.T) {
	got := scrubSecrets(map[string]any{"a": "x Bearer tok-123456789 y", "tok-123456789": []any{"tok-123456789"}, "n": 1.0}, []string{"Bearer tok-123456789", "short"})
	raw, _ := json.Marshal(got)
	if strings.Contains(string(raw), "tok-123456789") || !strings.Contains(string(raw), Redacted) {
		t.Fatalf("scrub = %s", raw)
	}
	if out := cleanString("abcdefghijXY", []string{"abcdefghij", "efghijXY"}); out != Redacted {
		t.Fatalf("overlap = %q", out)
	}
}
