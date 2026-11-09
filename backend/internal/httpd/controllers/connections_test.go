package controllers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/aoagents/agent-orchestrator/backend/internal/service/connections"
)

type fakeConnections struct {
	connectErr error
	lastValues map[string]string
	lastCall   connections.CallArgs
	lastTool   string
}

func (f *fakeConnections) Available() bool { return true }

func (f *fakeConnections) List(context.Context) ([]connections.State, error) {
	return []connections.State{
		{ID: "github", Connected: true, Tail: "abcd", ConnectedAt: time.Date(2026, 11, 9, 10, 0, 0, 0, time.UTC)},
		{ID: "posthog", Connected: true, Values: map[string]string{"host": "https://eu.posthog.com"}},
		{ID: "retired-service", Connected: true},
		{ID: "openai"},
	}, nil
}

func (f *fakeConnections) Connect(_ context.Context, id string, values map[string]string) (connections.Result, error) {
	f.lastValues = values
	if f.connectErr != nil {
		return connections.Result{}, f.connectErr
	}
	return connections.Result{OK: false, Detail: "Bad credentials"}, nil
}

func (f *fakeConnections) Test(context.Context, string) (connections.Result, error) {
	return connections.Result{OK: true, Detail: "connected"}, nil
}

func (f *fakeConnections) Disconnect(_ context.Context, id string) error {
	if id == "nope" {
		return connections.ErrUnknownConnector
	}
	return nil
}

func (f *fakeConnections) AgentTools(context.Context, string) []connections.AgentTool {
	return []connections.AgentTool{{Name: "github_api", Description: "Call GitHub.", InputSchema: map[string]any{"type": "object"}}}
}

func (f *fakeConnections) Call(_ context.Context, tool string, args connections.CallArgs) (string, error) {
	f.lastTool, f.lastCall = tool, args
	return "GET /user -> 200 OK\n{}", nil
}

func serveConnections(t *testing.T, svc Connections, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	r := chi.NewRouter()
	r.Route("/api/v1", (&ConnectionsController{Svc: svc}).Register)
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	return rec
}

func TestConnectionsListSkipsUnknownAndCarriesNoSecretField(t *testing.T) {
	rec := serveConnections(t, &fakeConnections{}, http.MethodGet, "/api/v1/connections", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body)
	}
	var out ListConnectionsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if !out.Available || len(out.Connections) != 3 {
		t.Fatalf("response = %+v", out)
	}
	gh := out.Connections[0]
	if gh.ID != "github" || gh.Tool != "github_api" || !gh.Connected || gh.Tail != "abcd" || gh.ConnectedAt == nil || len(gh.Fields) == 0 {
		t.Fatalf("github = %+v", gh)
	}
	if out.Connections[1].Values["host"] != "https://eu.posthog.com" || out.Connections[2].Connected {
		t.Fatalf("posthog/openai = %+v / %+v", out.Connections[1], out.Connections[2])
	}
}

func TestConnectionsConnectReturnsTheCheck(t *testing.T) {
	svc := &fakeConnections{}
	rec := serveConnections(t, svc, http.MethodPut, "/api/v1/connections/github", `{"values":{"key":"ghp_x"}}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body)
	}
	var out ConnectionCheckResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if out.OK || out.Detail != "Bad credentials" || out.Connection.ID != "github" || svc.lastValues["key"] != "ghp_x" {
		t.Fatalf("response = %+v, values %v", out, svc.lastValues)
	}
	if strings.Contains(rec.Body.String(), "ghp_x") {
		t.Fatal("the response repeats the key")
	}
}

func TestConnectionsErrorsMapToTheEnvelope(t *testing.T) {
	cases := []struct {
		err      error
		status   int
		wantCode string
	}{
		{connections.ErrUnknownConnector, http.StatusNotFound, "CONNECTION_NOT_FOUND"},
		{connections.ValidationError{Message: "API key is required"}, http.StatusBadRequest, "CONNECTION_INVALID"},
		{connections.ErrBusy, http.StatusConflict, "CONNECTION_BUSY"},
		{connections.ErrStoreUnavailable, http.StatusServiceUnavailable, "CONNECTIONS_UNAVAILABLE"},
		{connections.ErrStoreDenied, http.StatusForbidden, "CONNECTION_KEYCHAIN_DENIED"},
	}
	for _, tc := range cases {
		rec := serveConnections(t, &fakeConnections{connectErr: tc.err}, http.MethodPut, "/api/v1/connections/openai", `{"values":{"key":"k"}}`)
		if rec.Code != tc.status || !strings.Contains(rec.Body.String(), tc.wantCode) {
			t.Errorf("%v: status %d body %s; want %d %s", tc.err, rec.Code, rec.Body, tc.status, tc.wantCode)
		}
	}
	if rec := serveConnections(t, &fakeConnections{}, http.MethodPut, "/api/v1/connections/openai", `{"values":{},"extra":1}`); rec.Code != http.StatusBadRequest {
		t.Errorf("unknown request field: status %d", rec.Code)
	}
	if rec := serveConnections(t, &fakeConnections{}, http.MethodDelete, "/api/v1/connections/nope", ``); rec.Code != http.StatusNotFound {
		t.Errorf("delete unknown: status %d", rec.Code)
	}
	if rec := serveConnections(t, &fakeConnections{}, http.MethodDelete, "/api/v1/connections/openai", ``); rec.Code != http.StatusNoContent {
		t.Errorf("delete: status %d", rec.Code)
	}
}

func TestConnectionsAgentToolsAndCall(t *testing.T) {
	svc := &fakeConnections{}
	rec := serveConnections(t, svc, http.MethodGet, "/api/v1/connections/agent-tools", "")
	var tools ConnectionAgentToolsResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &tools)
	if rec.Code != http.StatusOK || len(tools.Tools) != 1 || tools.Tools[0].Name != "github_api" {
		t.Fatalf("agent-tools = %d %s", rec.Code, rec.Body)
	}
	rec = serveConnections(t, svc, http.MethodPost, "/api/v1/connections/call", `{"tool":"github_api","path":"/user","fields":"login"}`)
	var out CallConnectionResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if rec.Code != http.StatusOK || !strings.HasPrefix(out.Text, "GET /user -> 200 OK") || svc.lastTool != "github_api" || svc.lastCall.Fields != "login" {
		t.Fatalf("call = %d %s, last %s %+v", rec.Code, rec.Body, svc.lastTool, svc.lastCall)
	}
}
