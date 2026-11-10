package connections

import (
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

type memStore struct {
	mu     sync.Mutex
	items  map[string]map[string]string
	gets   int
	putErr error
}

func newMemStore() *memStore { return &memStore{items: map[string]map[string]string{}} }

func (m *memStore) Available() bool { return true }

func (m *memStore) Get(_ context.Context, id string) (map[string]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.gets++
	v, ok := m.items[id]
	if !ok {
		return nil, ErrSecretNotFound
	}
	return v, nil
}

func (m *memStore) Put(_ context.Context, id string, values map[string]string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.putErr != nil {
		return m.putErr
	}
	m.items[id] = values
	return nil
}

func (m *memStore) Delete(_ context.Context, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.items[id]; !ok {
		return ErrSecretNotFound
	}
	delete(m.items, id)
	return nil
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (fn roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return fn(r) }

func response(status int, body string, headers ...string) *http.Response {
	h := make(http.Header)
	for i := 0; i+1 < len(headers); i += 2 {
		h.Set(headers[i], headers[i+1])
	}
	return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body)), Header: h}
}

// recorder answers every request with one response and keeps the requests.
type recorder struct {
	mu       sync.Mutex
	requests []*http.Request
	bodies   []string
	answer   func(*http.Request) *http.Response
}

func (r *recorder) client() *http.Client {
	return &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		r.mu.Lock()
		defer r.mu.Unlock()
		body := ""
		if req.Body != nil {
			raw, _ := io.ReadAll(req.Body)
			body = string(raw)
		}
		r.requests = append(r.requests, req)
		r.bodies = append(r.bodies, body)
		return r.answer(req), nil
	})}
}

func newTestService(t *testing.T, answer func(*http.Request) *http.Response) (*Service, *memStore, *recorder) {
	t.Helper()
	store := newMemStore()
	svc := New(store, t.TempDir())
	rec := &recorder{answer: answer}
	svc.caller = newCaller(rec.client())
	return svc, store, rec
}

func ok(body string) func(*http.Request) *http.Response {
	return func(*http.Request) *http.Response { return response(http.StatusOK, body) }
}

func TestCatalogueIsConsistent(t *testing.T) {
	ids, tools := map[string]bool{}, map[string]bool{}
	if len(catalogue) != 17 {
		t.Fatalf("catalogue has %d connectors, want 17", len(catalogue))
	}
	for _, c := range catalogue {
		if ids[c.ID] || tools[c.Tool] {
			t.Errorf("%s: duplicate id or tool", c.ID)
		}
		ids[c.ID], tools[c.Tool] = true, true
		if c.Tool != c.ID+"_api" {
			t.Errorf("%s: tool %q, want %q", c.ID, c.Tool, c.ID+"_api")
		}
		if c.Name == "" || c.Blurb == "" || c.Guide == "" || c.Auth == nil || len(c.Methods) == 0 {
			t.Errorf("%s: missing name, blurb, guide, auth or methods", c.ID)
		}
		if !accountSafe(c.ID) {
			t.Errorf("%s: id is not a safe keychain account", c.ID)
		}
		if !strings.HasPrefix(c.KeyURL, "https://") {
			t.Errorf("%s: key URL %q is not https", c.ID, c.KeyURL)
		}
		base, err := c.baseURL(map[string]string{})
		if err != nil || !strings.HasPrefix(base, "https://") {
			t.Errorf("%s: base %q, %v; want an https base", c.ID, base, err)
		}
		if len(c.secretKeys()) != 1 || c.secretKeys()[0] != "key" {
			t.Errorf("%s: secret fields %v, want [key]", c.ID, c.secretKeys())
		}
		if c.Test != nil && !strings.HasPrefix(c.Test.Path, "/") {
			t.Errorf("%s: test path %q", c.ID, c.Test.Path)
		}
		if strings.Contains(c.Guide, "open_pr") || strings.Contains(c.Guide, "ask_user") {
			t.Errorf("%s: guide names a Swarm Online tool", c.ID)
		}
	}
}

func accountSafe(id string) bool {
	for _, r := range id {
		if (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '-' && r != '_' {
			return false
		}
	}
	return id != ""
}

func TestConnectSavesKeyInStoreAndKeepsItOutOfTheIndex(t *testing.T) {
	svc, store, rec := newTestService(t, ok(`{"success":true}`))
	ctx := context.Background()

	result, err := svc.Connect(ctx, "cloudflare", map[string]string{"key": "  cf-secret-token-1234 "})
	if err != nil || !result.OK {
		t.Fatalf("Connect = %+v, %v", result, err)
	}
	if got := store.items["cloudflare"]["key"]; got != "cf-secret-token-1234" {
		t.Fatalf("stored key = %q", got)
	}
	if n := len(rec.requests); n != 1 || rec.requests[0].URL.String() != "https://api.cloudflare.com/client/v4/user/tokens/verify" {
		t.Fatalf("requests = %d, first %v", n, rec.requests)
	}
	if got := rec.requests[0].Header.Get("Authorization"); got != "Bearer cf-secret-token-1234" {
		t.Fatalf("Authorization = %q", got)
	}
	raw, err := os.ReadFile(filepath.Join(filepath.Dir(svc.indexPath), "connections.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "cf-secret-token") {
		t.Fatalf("index file holds the secret: %s", raw)
	}
	info, _ := os.Stat(svc.indexPath)
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("index mode = %v, want 0600", info.Mode().Perm())
	}
	states, _ := svc.List(ctx)
	for _, s := range states {
		if s.ID == "cloudflare" && (!s.Connected || s.Tail != "1234") {
			t.Fatalf("cloudflare state = %+v", s)
		}
	}
}

func TestConnectFailureRemovesANewKey(t *testing.T) {
	svc, store, _ := newTestService(t, func(*http.Request) *http.Response {
		return response(http.StatusUnauthorized, `{"error":{"message":"Incorrect API key provided"}}`)
	})
	result, err := svc.Connect(context.Background(), "openai", map[string]string{"key": "sk-wrong-key-0000"})
	if err != nil {
		t.Fatal(err)
	}
	if result.OK || result.Detail != "Incorrect API key provided" {
		t.Fatalf("result = %+v", result)
	}
	if _, ok := store.items["openai"]; ok {
		t.Fatal("a rejected key stayed in the store")
	}
	if len(svc.Connected(context.Background())) != 0 {
		t.Fatal("a rejected key shows as connected")
	}
}

func TestConnectFailurePutsTheOldKeyBack(t *testing.T) {
	good := true
	svc, store, _ := newTestService(t, func(*http.Request) *http.Response {
		if good {
			return response(http.StatusOK, `{}`)
		}
		return response(http.StatusForbidden, `{"message":"Bad credentials"}`)
	})
	ctx := context.Background()
	if r, err := svc.Connect(ctx, "github", map[string]string{"key": "ghp_good_token_1111"}); err != nil || !r.OK {
		t.Fatalf("first Connect = %+v, %v", r, err)
	}
	good = false
	r, err := svc.Connect(ctx, "github", map[string]string{"key": "ghp_bad_token_2222"})
	if err != nil || r.OK {
		t.Fatalf("second Connect = %+v, %v", r, err)
	}
	if !strings.Contains(r.Detail, "Bad credentials") || !strings.Contains(r.Detail, "lacks the permission") {
		t.Fatalf("detail = %q", r.Detail)
	}
	if got := store.items["github"]["key"]; got != "ghp_good_token_1111" {
		t.Fatalf("stored key = %q, want the old key back", got)
	}
}

func TestConnectBlankSecretKeepsTheSavedKey(t *testing.T) {
	svc, store, rec := newTestService(t, ok(`{}`))
	ctx := context.Background()
	if _, err := svc.Connect(ctx, "posthog", map[string]string{"key": "phx_saved_key_9999"}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Connect(ctx, "posthog", map[string]string{"key": "", "host": "https://eu.posthog.com/"}); err != nil {
		t.Fatal(err)
	}
	if got := store.items["posthog"]; got["key"] != "phx_saved_key_9999" || got["host"] != "https://eu.posthog.com/" {
		t.Fatalf("stored = %v", got)
	}
	if last := rec.requests[len(rec.requests)-1].URL.String(); last != "https://eu.posthog.com/api/users/@me/" {
		t.Fatalf("test call URL = %q", last)
	}
	states, _ := svc.List(ctx)
	for _, s := range states {
		if s.ID == "posthog" && s.Values["host"] != "https://eu.posthog.com/" {
			t.Fatalf("posthog values = %v", s.Values)
		}
	}
}

func TestConnectRejectsBadValues(t *testing.T) {
	svc, store, rec := newTestService(t, ok(`{}`))
	ctx := context.Background()
	cases := []struct {
		id     string
		values map[string]string
		want   string
	}{
		{"openai", map[string]string{}, "API key is required"},
		{"openai", map[string]string{"key": "sk-x", "extra": "y"}, `no field "extra"`},
		{"openai", map[string]string{"key": "sk-a\nsk-b"}, "one line"},
		{"posthog", map[string]string{"key": "phx_1", "host": "http://eu.posthog.com"}, "https"},
		{"posthog", map[string]string{"key": "phx_1", "host": "https://evil.example/api/projects"}, "no path"},
	}
	for _, tc := range cases {
		_, err := svc.Connect(ctx, tc.id, tc.values)
		var invalid ValidationError
		if !errors.As(err, &invalid) || !strings.Contains(invalid.Message, tc.want) {
			t.Errorf("Connect(%s, %v) = %v, want a validation error with %q", tc.id, tc.values, err, tc.want)
		}
	}
	if _, err := svc.Connect(ctx, "nope", map[string]string{"key": "x"}); !errors.Is(err, ErrUnknownConnector) {
		t.Errorf("unknown id: %v", err)
	}
	if len(store.items) != 0 || len(rec.requests) != 0 {
		t.Fatalf("bad values reached the store (%d) or the network (%d)", len(store.items), len(rec.requests))
	}
}

func TestConnectWithoutATestCallSavesWithoutACall(t *testing.T) {
	svc, _, rec := newTestService(t, ok(`{}`))
	r, err := svc.Connect(context.Background(), "serper", map[string]string{"key": "serper-key-1234"})
	if err != nil || !r.OK || !strings.Contains(r.Detail, "first use") {
		t.Fatalf("Connect = %+v, %v", r, err)
	}
	if len(rec.requests) != 0 {
		t.Fatal("serper's key check spent a credit")
	}
}

func TestTestDetectsAGraphQLRejection(t *testing.T) {
	svc, _, _ := newTestService(t, ok(`{"errors":[{"message":"Authentication required, not authenticated"}]}`))
	r, err := svc.Connect(context.Background(), "linear", map[string]string{"key": "lin_api_bad_0000"})
	if err != nil || r.OK || r.Detail != "Authentication required, not authenticated" {
		t.Fatalf("Connect = %+v, %v", r, err)
	}
}

func connect(t *testing.T, svc *Service, store *memStore, id string, values map[string]string) {
	t.Helper()
	if err := store.Put(context.Background(), id, values); err != nil {
		t.Fatal(err)
	}
	c, _ := Lookup(id)
	if err := svc.save(context.Background(), c, values); err != nil {
		t.Fatal(err)
	}
}

func TestCallAttachesEachServicesAuth(t *testing.T) {
	cases := []struct {
		id, tool  string
		values    map[string]string
		path      string
		wantURL   string
		wantName  string
		wantValue string
	}{
		{"anthropic", "anthropic_api", map[string]string{"key": "sk-ant-1"}, "/v1/models", "https://api.anthropic.com/v1/models", "x-api-key", "sk-ant-1"},
		{"linear", "linear_api", map[string]string{"key": "lin_api_1"}, "/graphql", "https://api.linear.app/graphql", "Authorization", "lin_api_1"},
		{"mailgun", "mailgun_api", map[string]string{"key": "mg-1", "region": "EU"}, "/v3/domains", "https://api.eu.mailgun.net/v3/domains", "Authorization", "Basic YXBpOm1nLTE="},
		{"figma", "figma_api", map[string]string{"key": "figd_1", "team": "12345"}, "/v1/teams/{team_id}/projects", "https://api.figma.com/v1/teams/12345/projects", "X-Figma-Token", "figd_1"},
		{"elevenlabs", "elevenlabs_api", map[string]string{"key": "xi-1"}, "v1/user", "https://api.elevenlabs.io/v1/user", "xi-api-key", "xi-1"},
	}
	for _, tc := range cases {
		svc, store, rec := newTestService(t, ok(`{"ok":true}`))
		connect(t, svc, store, tc.id, tc.values)
		method := "GET"
		if tc.id == "linear" {
			method = "POST"
		}
		text, err := svc.Call(context.Background(), tc.tool, CallArgs{Path: tc.path, Method: method})
		if err != nil || !strings.HasPrefix(text, method+" ") || !strings.Contains(text, "200 OK") {
			t.Fatalf("%s: Call = %q, %v", tc.id, text, err)
		}
		req := rec.requests[0]
		if req.URL.String() != tc.wantURL || req.Header.Get(tc.wantName) != tc.wantValue {
			t.Errorf("%s: %s %s = %q", tc.id, req.URL, tc.wantName, req.Header.Get(tc.wantName))
		}
	}
}

func TestCallAnswersInTextForAgentMistakes(t *testing.T) {
	svc, store, rec := newTestService(t, ok(`{}`))
	ctx := context.Background()

	text, err := svc.Call(ctx, "stripe_api", CallArgs{Path: "/v1/account"})
	if err != nil || !strings.Contains(text, "Stripe is not connected") || !strings.Contains(text, "Settings > Connections") {
		t.Fatalf("not connected: %q, %v", text, err)
	}
	connect(t, svc, store, "stripe", map[string]string{"key": "sk_test_abcdef"})
	connect(t, svc, store, "openai", map[string]string{"key": "sk-openai-abcdef"})
	connect(t, svc, store, "figma", map[string]string{"key": "figd_abcdef"})
	for _, tc := range []struct {
		tool string
		args CallArgs
		want string
	}{
		{"stripe_api", CallArgs{Path: " "}, "Pass `path`"},
		{"stripe_api", CallArgs{Path: "/v1/charges", Method: "patch"}, "does not accept PATCH"},
		{"openai_api", CallArgs{Path: "/v1/chat/completions", Method: "POST", Body: "{not json"}, "valid JSON"},
		{"figma_api", CallArgs{Path: "/v1/teams/{team_id}/projects"}, "No Figma team is saved"},
	} {
		text, err := svc.Call(ctx, tc.tool, tc.args)
		if err != nil || !strings.Contains(text, tc.want) {
			t.Errorf("%s %+v = %q, %v; want %q", tc.tool, tc.args, text, err, tc.want)
		}
	}
	if len(rec.requests) != 0 {
		t.Fatalf("%d bad calls reached the network", len(rec.requests))
	}
	if _, err := svc.Call(ctx, "nope_api", CallArgs{Path: "/"}); !errors.Is(err, ErrUnknownConnector) {
		t.Fatalf("unknown tool: %v", err)
	}
}

func TestCallSendsAFormBodyForStripe(t *testing.T) {
	svc, store, rec := newTestService(t, ok(`{"id":"cus_1"}`))
	connect(t, svc, store, "stripe", map[string]string{"key": "sk_test_abcdef"})
	if _, err := svc.Call(context.Background(), "stripe_api", CallArgs{Path: "/v1/customers", Method: "POST", Body: "email=a%40b.c"}); err != nil {
		t.Fatal(err)
	}
	if got := rec.requests[0].Header.Get("Content-Type"); got != "application/x-www-form-urlencoded" {
		t.Fatalf("Content-Type = %q", got)
	}
	if rec.bodies[0] != "email=a%40b.c" {
		t.Fatalf("body = %q", rec.bodies[0])
	}
}

func TestCallDoesNotFollowARedirect(t *testing.T) {
	svc, store, rec := newTestService(t, func(*http.Request) *http.Response {
		return response(http.StatusFound, "", "Location", "https://evil.example/collect")
	})
	connect(t, svc, store, "elevenlabs", map[string]string{"key": "xi-secret-1"})
	text, err := svc.Call(context.Background(), "elevenlabs_api", CallArgs{Path: "/v1/user"})
	if err != nil || !strings.Contains(text, "302 REDIRECT to https://evil.example/collect") || !strings.Contains(text, "does not follow") {
		t.Fatalf("Call = %q, %v", text, err)
	}
	if len(rec.requests) != 1 {
		t.Fatalf("requests = %d, want 1 (no redirect hop)", len(rec.requests))
	}
}

func TestCallHidesAnEchoedSecret(t *testing.T) {
	svc, store, _ := newTestService(t, ok(`{"token":"sk-openai-echoed-1234","note":"key sk-openai-echoed-1234 is valid"}`))
	connect(t, svc, store, "openai", map[string]string{"key": "sk-openai-echoed-1234"})
	text, err := svc.Call(context.Background(), "openai_api", CallArgs{Path: "/v1/models"})
	if err != nil || strings.Contains(text, "sk-openai-echoed-1234") || !strings.Contains(text, hiddenSecret) {
		t.Fatalf("Call = %q, %v", text, err)
	}
}

func TestCallKeepsOnlyTheAskedFields(t *testing.T) {
	svc, store, _ := newTestService(t, ok(`{"success":true,"result":[{"id":"z1","name":"a.com","plan":{"name":"free"},"x":1},{"id":"z2","name":"b.com"}]}`))
	connect(t, svc, store, "cloudflare", map[string]string{"key": "cf-key-123456"})
	text, err := svc.Call(context.Background(), "cloudflare_api", CallArgs{Path: "/zones", Fields: "id, plan.name"})
	if err != nil {
		t.Fatal(err)
	}
	_, body, _ := strings.Cut(text, "\n")
	want := `{"result":[{"id":"z1","plan.name":"free"},{"id":"z2"}],"success":true}`
	if body != want {
		t.Fatalf("body = %s\nwant  %s", body, want)
	}
}

func TestProjectGoesThroughGraphQLData(t *testing.T) {
	parsed, _ := decodeJSON([]byte(`{"data":{"issues":{"nodes":[{"id":"1","title":"t","state":{"name":"Done"}}]}}}`))
	got, _ := encodeJSON(project(parsed, []string{"title", "state.name"}))
	want := `{"data":{"issues":{"nodes":[{"state.name":"Done","title":"t"}]}}}`
	if got != want {
		t.Fatalf("project = %s, want %s", got, want)
	}
	// No field matches: the row comes back whole, not empty.
	parsed, _ = decodeJSON([]byte(`[{"id":1}]`))
	if got, _ := encodeJSON(project(parsed, []string{"nope"})); got != `[{"id":1}]` {
		t.Fatalf("project with no match = %s", got)
	}
}

func TestCallKeepsLargeNumbersExact(t *testing.T) {
	svc, store, _ := newTestService(t, ok(`{"id":12345678901234567890}`))
	connect(t, svc, store, "github", map[string]string{"key": "ghp_token_123456"})
	text, _ := svc.Call(context.Background(), "github_api", CallArgs{Path: "/user"})
	if !strings.Contains(text, "12345678901234567890") {
		t.Fatalf("Call = %q", text)
	}
}

func TestCallTruncatesAVeryLongResponse(t *testing.T) {
	long := `"` + strings.Repeat("a", maxResultChars+10) + `"`
	svc, store, _ := newTestService(t, ok(long))
	connect(t, svc, store, "github", map[string]string{"key": "ghp_token_123456"})
	text, _ := svc.Call(context.Background(), "github_api", CallArgs{Path: "/user"})
	if !strings.Contains(text, "RESPONSE TRUNCATED") {
		t.Fatal("no truncation notice")
	}
}

func TestCallRefusesBeforeTheKeyIsRead(t *testing.T) {
	svc, store, rec := newTestService(t, ok(`{}`))
	connect(t, svc, store, "github", map[string]string{"key": "ghp_token_123456"})
	svc.cache = map[string]map[string]string{}
	store.gets = 0
	text, err := svc.Call(context.Background(), "github_api", CallArgs{Path: "/repos/o/r/keys", Method: "POST", Body: `{"key":"ssh-ed25519 AAAA"}`})
	if err != nil || !strings.HasPrefix(text, RefusalPrefix) {
		t.Fatalf("Call = %q, %v", text, err)
	}
	if store.gets != 0 || len(rec.requests) != 0 {
		t.Fatalf("a refused call read the store (%d) or the network (%d)", store.gets, len(rec.requests))
	}
}

func TestCallReadsTheStoreOnce(t *testing.T) {
	svc, store, _ := newTestService(t, ok(`{}`))
	connect(t, svc, store, "github", map[string]string{"key": "ghp_token_123456"})
	svc.cache = map[string]map[string]string{}
	store.gets = 0
	for range 3 {
		if _, err := svc.Call(context.Background(), "github_api", CallArgs{Path: "/user"}); err != nil {
			t.Fatal(err)
		}
	}
	if store.gets != 1 {
		t.Fatalf("store reads = %d, want 1", store.gets)
	}
}

func TestDisconnectRemovesKeyAndTool(t *testing.T) {
	svc, store, _ := newTestService(t, ok(`{}`))
	ctx := context.Background()
	connect(t, svc, store, "resend", map[string]string{"key": "re_key_123456"})
	if tools := svc.AgentTools(ctx, ""); len(tools) != 1 || tools[0].Name != "resend_api" {
		t.Fatalf("tools = %+v", tools)
	}
	if err := svc.Disconnect(ctx, "resend"); err != nil {
		t.Fatal(err)
	}
	if _, ok := store.items["resend"]; ok || len(svc.AgentTools(ctx, "")) != 0 {
		t.Fatal("disconnect left the key or the tool")
	}
	text, _ := svc.Call(ctx, "resend_api", CallArgs{Path: "/domains"})
	if !strings.Contains(text, "not connected") {
		t.Fatalf("Call after disconnect = %q", text)
	}
}

func TestPromptBlockNamesOnlyConnectedServices(t *testing.T) {
	svc, store, _ := newTestService(t, ok(`{}`))
	ctx := context.Background()
	if block := svc.PromptBlock(ctx, ""); block != "" {
		t.Fatalf("block with nothing connected = %q", block)
	}
	connect(t, svc, store, "vercel", map[string]string{"key": "vercel-token-1"})
	block := svc.PromptBlock(ctx, "")
	if !strings.Contains(block, "Vercel -> vercel_api") || strings.Contains(block, "netlify_api") {
		t.Fatalf("block = %q", block)
	}
}

func TestToolSchemaOffersOnlyTheAllowedMethods(t *testing.T) {
	c, _ := Lookup("serper")
	schema := toolSchema(c)
	method := schema["properties"].(map[string]any)["method"].(map[string]any)
	if got := method["enum"].([]string); len(got) != 1 || got[0] != "POST" {
		t.Fatalf("serper methods = %v", got)
	}
	stripe, _ := Lookup("stripe")
	if !strings.Contains(toolDescription(stripe), "FORM-ENCODED") {
		t.Fatal("stripe's description does not say the body is form-encoded")
	}
}

func TestStoreErrorsStayErrors(t *testing.T) {
	svc, store, _ := newTestService(t, ok(`{}`))
	store.putErr = ErrStoreDenied
	if _, err := svc.Connect(context.Background(), "openai", map[string]string{"key": "sk-1"}); !errors.Is(err, ErrStoreDenied) {
		t.Fatalf("Connect = %v, want ErrStoreDenied", err)
	}
}

func TestProjectGateHidesAndRefusesAConnector(t *testing.T) {
	svc, store, rec := newTestService(t, ok(`{}`))
	ctx := context.Background()
	connect(t, svc, store, "stripe", map[string]string{"key": "sk_test_abcdef"})
	connect(t, svc, store, "openai", map[string]string{"key": "sk-openai-abcdef"})
	projects := map[string]string{"s-blog": "blog", "s-shop": "shop"}
	svc.SetProjectGate(func(_ context.Context, sessionID string) string { return projects[sessionID] },
		func(service, project string) bool { return service != "stripe" || project == "shop" })

	names := func(sessionID string) []string {
		var out []string
		for _, tool := range svc.AgentTools(ctx, sessionID) {
			out = append(out, tool.Name)
		}
		return out
	}
	if got := names("s-shop"); len(got) != 2 {
		t.Fatalf("shop tools = %v", got)
	}
	if got := names("s-blog"); len(got) != 1 || got[0] != "openai_api" {
		t.Fatalf("blog tools = %v", got)
	}
	text, err := svc.Call(ctx, "stripe_api", CallArgs{Path: "/v1/account", SessionID: "s-blog"})
	if err != nil || text != "Stripe is not available in this project." || len(rec.requests) != 0 {
		t.Fatalf("blog stripe call = %q, %v, requests %d", text, err, len(rec.requests))
	}
	if _, err := svc.Call(ctx, "stripe_api", CallArgs{Path: "/v1/account", SessionID: "s-shop"}); err != nil || len(rec.requests) != 1 {
		t.Fatalf("shop stripe call: %v, requests %d", err, len(rec.requests))
	}
	if block := svc.PromptBlock(ctx, "blog"); strings.Contains(block, "stripe_api") || !strings.Contains(block, "openai_api") {
		t.Fatalf("blog block = %q", block)
	}
	if block := svc.PromptBlock(ctx, "shop"); !strings.Contains(block, "API-key services only") || strings.Contains(block, "Nothing else") {
		t.Fatalf("shop block = %q", block)
	}
}
