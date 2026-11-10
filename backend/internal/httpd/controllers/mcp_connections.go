package controllers

import (
	"context"
	"errors"
	"fmt"
	"html"
	"net/http"
	"net/url"

	"github.com/go-chi/chi/v5"

	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/apierr"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/apispec"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/envelope"
	"github.com/aoagents/agent-orchestrator/backend/internal/service/connections"
	"github.com/aoagents/agent-orchestrator/backend/internal/service/mcpconn"
)

// MCPConnections is the controller-facing contract for the remote MCP
// servers of Settings > Connections and the agents' MCP connection tools.
type MCPConnections interface {
	Overview(ctx context.Context) mcpconn.Overview
	Check(ctx context.Context, in mcpconn.CheckInput) mcpconn.CheckResult
	SaveCredentials(ctx context.Context, id string, in mcpconn.CredentialsInput) mcpconn.CheckResult
	StartOAuth(ctx context.Context, id string, client *mcpconn.OAuthClientInput) mcpconn.StartResult
	FinishOAuth(ctx context.Context, query url.Values) mcpconn.FinishResult
	RefreshTools(ctx context.Context, id string) mcpconn.CheckResult
	SetProjects(ctx context.Context, id string, projects mcpconn.Projects) mcpconn.ProjectsResult
	Rename(ctx context.Context, id, name string) mcpconn.ProjectsResult
	Remove(id string) (bool, string)
	Paste(ctx context.Context, text string) mcpconn.PasteResult
	ConnectorProjects() (map[string]mcpconn.Projects, error)
	SetConnectorProjects(ctx context.Context, service string, projects mcpconn.Projects) error
	ProjectOfSession(ctx context.Context, sessionID string) string
	IndexOrUnchanged(ctx context.Context, sessionID, known string) (mcpconn.ToolIndex, bool)
	List(ctx context.Context, project string) []mcpconn.ListedConnection
	Tools(ctx context.Context, project, name string) (any, error)
	Call(ctx context.Context, project, name, tool string, args map[string]any) (any, error)
}

// MCPConnectionsController owns the /mcp routes. Every route is loopback only
// and renderer-origin only, except the OAuth callback, which the owner's
// browser opens on the loopback listener and which a single-use state guards.
type MCPConnectionsController struct {
	Svc MCPConnections
}

// MCPOAuthCallbackPath is the redirect URI path on the loopback listener.
const MCPOAuthCallbackPath = "/api/v1/mcp-oauth/callback"

// Register mounts the MCP connection routes on the supplied router.
func (c *MCPConnectionsController) Register(r chi.Router) {
	r.Get("/mcp/connections", c.overview)
	r.Post("/mcp/check", c.check)
	r.Post("/mcp/paste", c.paste)
	r.Put("/mcp/connections/{id}/credentials", c.credentials)
	r.Post("/mcp/connections/{id}/oauth/start", c.startOAuth)
	r.Post("/mcp/connections/{id}/refresh", c.refresh)
	r.Put("/mcp/connections/{id}/projects", c.projects)
	r.Patch("/mcp/connections/{id}", c.rename)
	r.Delete("/mcp/connections/{id}", c.remove)
	r.Get("/connections/projects", c.connectorProjects)
	r.Put("/connections/{id}/projects", c.setConnectorProjects)
	r.Get("/mcp/agent/index", c.agentIndex)
	r.Post("/mcp/agent/list", c.agentList)
	r.Post("/mcp/agent/tools", c.agentTools)
	r.Post("/mcp/agent/call", c.agentCall)
	r.Get("/mcp-oauth/callback", c.oauthCallback)
}

func (c *MCPConnectionsController) unwired(w http.ResponseWriter, r *http.Request, method, path string) bool {
	if c.Svc == nil {
		apispec.NotImplemented(w, r, method, path)
		return true
	}
	return false
}

func badRequest(w http.ResponseWriter, r *http.Request, code string) {
	envelope.WriteAPIError(w, r, http.StatusBadRequest, "bad_request", code, "invalid request body", nil)
}

func (c *MCPConnectionsController) overview(w http.ResponseWriter, r *http.Request) {
	if c.unwired(w, r, "GET", "/api/v1/mcp/connections") {
		return
	}
	envelope.WriteJSON(w, http.StatusOK, c.Svc.Overview(r.Context()))
}

func (c *MCPConnectionsController) check(w http.ResponseWriter, r *http.Request) {
	if c.unwired(w, r, "POST", "/api/v1/mcp/check") {
		return
	}
	var in MCPCheckRequest
	if err := decodeJSONStrict(r, &in); err != nil {
		badRequest(w, r, "INVALID_MCP_CHECK")
		return
	}
	envelope.WriteJSON(w, http.StatusOK, c.Svc.Check(r.Context(), in))
}

func (c *MCPConnectionsController) paste(w http.ResponseWriter, r *http.Request) {
	if c.unwired(w, r, "POST", "/api/v1/mcp/paste") {
		return
	}
	var in MCPPasteRequest
	if err := decodeJSONStrict(r, &in); err != nil {
		badRequest(w, r, "INVALID_MCP_PASTE")
		return
	}
	envelope.WriteJSON(w, http.StatusOK, c.Svc.Paste(r.Context(), in.Text))
}

func (c *MCPConnectionsController) credentials(w http.ResponseWriter, r *http.Request) {
	if c.unwired(w, r, "PUT", "/api/v1/mcp/connections/{id}/credentials") {
		return
	}
	var in MCPCredentialsRequest
	if err := decodeJSONStrict(r, &in); err != nil {
		badRequest(w, r, "INVALID_MCP_CREDENTIALS")
		return
	}
	envelope.WriteJSON(w, http.StatusOK, c.Svc.SaveCredentials(r.Context(), chi.URLParam(r, "id"), in))
}

func (c *MCPConnectionsController) startOAuth(w http.ResponseWriter, r *http.Request) {
	if c.unwired(w, r, "POST", "/api/v1/mcp/connections/{id}/oauth/start") {
		return
	}
	var in MCPOAuthStartRequest
	if r.ContentLength != 0 {
		if err := decodeJSONStrict(r, &in); err != nil {
			badRequest(w, r, "INVALID_MCP_OAUTH_START")
			return
		}
	}
	var client *mcpconn.OAuthClientInput
	if in.ClientID != "" {
		client = &mcpconn.OAuthClientInput{ClientID: in.ClientID, ClientSecret: in.ClientSecret}
	}
	envelope.WriteJSON(w, http.StatusOK, c.Svc.StartOAuth(r.Context(), chi.URLParam(r, "id"), client))
}

func (c *MCPConnectionsController) refresh(w http.ResponseWriter, r *http.Request) {
	if c.unwired(w, r, "POST", "/api/v1/mcp/connections/{id}/refresh") {
		return
	}
	envelope.WriteJSON(w, http.StatusOK, c.Svc.RefreshTools(r.Context(), chi.URLParam(r, "id")))
}

func (c *MCPConnectionsController) projects(w http.ResponseWriter, r *http.Request) {
	if c.unwired(w, r, "PUT", "/api/v1/mcp/connections/{id}/projects") {
		return
	}
	var in MCPProjectsRequest
	if err := decodeJSONStrict(r, &in); err != nil || in.Projects == nil {
		badRequest(w, r, "INVALID_MCP_PROJECTS")
		return
	}
	envelope.WriteJSON(w, http.StatusOK, c.Svc.SetProjects(r.Context(), chi.URLParam(r, "id"), *in.Projects))
}

func (c *MCPConnectionsController) rename(w http.ResponseWriter, r *http.Request) {
	if c.unwired(w, r, "PATCH", "/api/v1/mcp/connections/{id}") {
		return
	}
	var in MCPRenameRequest
	if err := decodeJSONStrict(r, &in); err != nil {
		badRequest(w, r, "INVALID_MCP_RENAME")
		return
	}
	envelope.WriteJSON(w, http.StatusOK, c.Svc.Rename(r.Context(), chi.URLParam(r, "id"), in.Name))
}

func (c *MCPConnectionsController) remove(w http.ResponseWriter, r *http.Request) {
	if c.unwired(w, r, "DELETE", "/api/v1/mcp/connections/{id}") {
		return
	}
	if ok, message := c.Svc.Remove(chi.URLParam(r, "id")); !ok {
		if message == mcpconn.BrokenStoreText {
			envelope.WriteError(w, r, apierr.Conflict("MCP_STORE_BROKEN", message, nil))
			return
		}
		envelope.WriteError(w, r, apierr.NotFound("MCP_CONNECTION_NOT_FOUND", message))
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (c *MCPConnectionsController) connectorProjects(w http.ResponseWriter, r *http.Request) {
	if c.unwired(w, r, "GET", "/api/v1/connections/projects") {
		return
	}
	projects, err := c.Svc.ConnectorProjects()
	if err != nil {
		envelope.WriteError(w, r, apierr.Conflict("CONNECTOR_PROJECTS_BROKEN", err.Error(), nil))
		return
	}
	envelope.WriteJSON(w, http.StatusOK, ConnectorProjectsResponse{Projects: projects})
}

func (c *MCPConnectionsController) setConnectorProjects(w http.ResponseWriter, r *http.Request) {
	if c.unwired(w, r, "PUT", "/api/v1/connections/{id}/projects") {
		return
	}
	var in MCPProjectsRequest
	if err := decodeJSONStrict(r, &in); err != nil || in.Projects == nil {
		badRequest(w, r, "INVALID_CONNECTOR_PROJECTS")
		return
	}
	err := c.Svc.SetConnectorProjects(r.Context(), chi.URLParam(r, "id"), *in.Projects)
	var invalid connections.ValidationError
	switch {
	case err == nil:
		w.WriteHeader(http.StatusNoContent)
	case errors.Is(err, connections.ErrUnknownConnector):
		envelope.WriteError(w, r, apierr.NotFound("CONNECTION_NOT_FOUND", "unknown connection"))
	case errors.As(err, &invalid):
		envelope.WriteError(w, r, apierr.Invalid("CONNECTOR_PROJECTS_INVALID", invalid.Message, nil))
	default:
		envelope.WriteError(w, r, apierr.Conflict("CONNECTOR_PROJECTS_BROKEN", err.Error(), nil))
	}
}

func (c *MCPConnectionsController) agentIndex(w http.ResponseWriter, r *http.Request) {
	if c.unwired(w, r, "GET", "/api/v1/mcp/agent/index") {
		return
	}
	q := r.URL.Query()
	index, unchanged := c.Svc.IndexOrUnchanged(r.Context(), q.Get("sessionId"), q.Get("known"))
	envelope.WriteJSON(w, http.StatusOK, MCPAgentIndexResponse{ToolIndex: index, Unchanged: unchanged})
}

func (c *MCPConnectionsController) agentList(w http.ResponseWriter, r *http.Request) {
	if c.unwired(w, r, "POST", "/api/v1/mcp/agent/list") {
		return
	}
	var in MCPAgentRequest
	if err := decodeJSON(r, &in); err != nil {
		badRequest(w, r, "INVALID_MCP_AGENT_CALL")
		return
	}
	list := c.Svc.List(r.Context(), c.Svc.ProjectOfSession(r.Context(), in.SessionID))
	envelope.WriteJSON(w, http.StatusOK, MCPAgentReply{Result: map[string]any{"connections": list}})
}

func (c *MCPConnectionsController) agentTools(w http.ResponseWriter, r *http.Request) {
	if c.unwired(w, r, "POST", "/api/v1/mcp/agent/tools") {
		return
	}
	var in MCPAgentRequest
	if err := decodeJSON(r, &in); err != nil {
		badRequest(w, r, "INVALID_MCP_AGENT_CALL")
		return
	}
	result, err := c.Svc.Tools(r.Context(), c.Svc.ProjectOfSession(r.Context(), in.SessionID), in.Name)
	if err != nil {
		envelope.WriteError(w, r, err)
		return
	}
	envelope.WriteJSON(w, http.StatusOK, MCPAgentReply{Result: result})
}

func (c *MCPConnectionsController) agentCall(w http.ResponseWriter, r *http.Request) {
	if c.unwired(w, r, "POST", "/api/v1/mcp/agent/call") {
		return
	}
	var in MCPAgentRequest
	if err := decodeJSON(r, &in); err != nil {
		badRequest(w, r, "INVALID_MCP_AGENT_CALL")
		return
	}
	var args map[string]any
	if in.Arguments != nil {
		obj, ok := in.Arguments.(map[string]any)
		if !ok {
			envelope.WriteJSON(w, http.StatusOK, MCPAgentReply{Result: "connection_call needs arguments as an object."})
			return
		}
		args = obj
	}
	result, err := c.Svc.Call(r.Context(), c.Svc.ProjectOfSession(r.Context(), in.SessionID), in.Name, in.Tool, args)
	if err != nil {
		envelope.WriteError(w, r, err)
		return
	}
	envelope.WriteJSON(w, http.StatusOK, MCPAgentReply{Result: result})
}

// oauthCallback is where the owner's browser lands after the sign-in. The URL
// holds the OAuth code, so the page has no cache and no referrer.
func (c *MCPConnectionsController) oauthCallback(w http.ResponseWriter, r *http.Request) {
	if c.unwired(w, r, "GET", MCPOAuthCallbackPath) {
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'")
	out := c.Svc.FinishOAuth(r.Context(), r.URL.Query())
	title, status := "Signed in", http.StatusOK
	message := fmt.Sprintf("Swarm is now signed in to %s. Close this tab and go back to Swarm.", out.Name)
	if !out.OK {
		title, status = "The sign-in did not finish", http.StatusBadRequest
		message = out.Message + " Go back to Swarm, open Settings > Connections, and try again."
	}
	w.WriteHeader(status)
	_, _ = fmt.Fprintf(w, `<!doctype html><html lang="en"><head><meta charset="utf-8"><title>%s</title>
<meta name="viewport" content="width=device-width, initial-scale=1">
<style>body{font:16px/1.5 -apple-system,system-ui,sans-serif;margin:0;display:grid;place-items:center;min-height:100vh;background:#111;color:#eee}
main{max-width:28rem;padding:2rem}h1{font-size:1.25rem;margin:0 0 .5rem}p{margin:0;color:#bbb}</style></head>
<body><main><h1>%s</h1><p>%s</p></main></body></html>`, html.EscapeString(title), html.EscapeString(title), html.EscapeString(message))
}
