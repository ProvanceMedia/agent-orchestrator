package controllers

import (
	"context"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/apierr"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/apispec"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/envelope"
	"github.com/aoagents/agent-orchestrator/backend/internal/service/connections"
)

// Connections is the controller-facing contract for the connection store and
// the agents' passthrough calls.
type Connections interface {
	Available() bool
	List(ctx context.Context) ([]connections.State, error)
	Connect(ctx context.Context, id string, values map[string]string) (connections.Result, error)
	Test(ctx context.Context, id string) (connections.Result, error)
	Disconnect(ctx context.Context, id string) error
	AgentTools(ctx context.Context) []connections.AgentTool
	Call(ctx context.Context, tool string, args connections.CallArgs) (string, error)
}

// ConnectionsController owns the /connections routes behind Settings >
// Connections and the connection tools of `ao mcp`. The routes are loopback
// only: the LAN listener blocks them, and a browser caller needs the renderer
// origin.
type ConnectionsController struct {
	Svc Connections
}

// Register mounts the connection routes on the supplied router.
func (c *ConnectionsController) Register(r chi.Router) {
	r.Get("/connections", c.list)
	r.Get("/connections/agent-tools", c.agentTools)
	r.Post("/connections/call", c.call)
	r.Put("/connections/{id}", c.connect)
	r.Delete("/connections/{id}", c.disconnect)
	r.Post("/connections/{id}/test", c.test)
}

func (c *ConnectionsController) list(w http.ResponseWriter, r *http.Request) {
	if c.Svc == nil {
		apispec.NotImplemented(w, r, "GET", "/api/v1/connections")
		return
	}
	states, err := c.Svc.List(r.Context())
	if err != nil {
		envelope.WriteError(w, r, err)
		return
	}
	out := ListConnectionsResponse{Available: c.Svc.Available(), Connections: make([]ConnectionResponse, 0, len(states))}
	for _, state := range states {
		if item, ok := connectionResponse(state); ok {
			out.Connections = append(out.Connections, item)
		}
	}
	envelope.WriteJSON(w, http.StatusOK, out)
}

func (c *ConnectionsController) connect(w http.ResponseWriter, r *http.Request) {
	if c.Svc == nil {
		apispec.NotImplemented(w, r, "PUT", "/api/v1/connections/{id}")
		return
	}
	var request ConnectConnectionRequest
	if err := decodeJSONStrict(r, &request); err != nil {
		envelope.WriteAPIError(w, r, http.StatusBadRequest, "bad_request", "INVALID_CONNECTION_REQUEST", "invalid connection request", nil)
		return
	}
	id := chi.URLParam(r, "id")
	result, err := c.Svc.Connect(r.Context(), id, request.Values)
	if err != nil {
		envelope.WriteError(w, r, connectionError(err))
		return
	}
	c.writeCheck(w, r, id, result)
}

func (c *ConnectionsController) test(w http.ResponseWriter, r *http.Request) {
	if c.Svc == nil {
		apispec.NotImplemented(w, r, "POST", "/api/v1/connections/{id}/test")
		return
	}
	id := chi.URLParam(r, "id")
	result, err := c.Svc.Test(r.Context(), id)
	if err != nil {
		envelope.WriteError(w, r, connectionError(err))
		return
	}
	c.writeCheck(w, r, id, result)
}

func (c *ConnectionsController) writeCheck(w http.ResponseWriter, r *http.Request, id string, result connections.Result) {
	states, err := c.Svc.List(r.Context())
	if err != nil {
		envelope.WriteError(w, r, err)
		return
	}
	out := ConnectionCheckResponse{OK: result.OK, Detail: result.Detail}
	for _, state := range states {
		if state.ID == id {
			out.Connection, _ = connectionResponse(state)
		}
	}
	envelope.WriteJSON(w, http.StatusOK, out)
}

func (c *ConnectionsController) disconnect(w http.ResponseWriter, r *http.Request) {
	if c.Svc == nil {
		apispec.NotImplemented(w, r, "DELETE", "/api/v1/connections/{id}")
		return
	}
	if err := c.Svc.Disconnect(r.Context(), chi.URLParam(r, "id")); err != nil {
		envelope.WriteError(w, r, connectionError(err))
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (c *ConnectionsController) agentTools(w http.ResponseWriter, r *http.Request) {
	if c.Svc == nil {
		apispec.NotImplemented(w, r, "GET", "/api/v1/connections/agent-tools")
		return
	}
	out := ConnectionAgentToolsResponse{Tools: []ConnectionAgentTool{}}
	for _, tool := range c.Svc.AgentTools(r.Context()) {
		out.Tools = append(out.Tools, ConnectionAgentTool{Name: tool.Name, Description: tool.Description, InputSchema: tool.InputSchema})
	}
	envelope.WriteJSON(w, http.StatusOK, out)
}

func (c *ConnectionsController) call(w http.ResponseWriter, r *http.Request) {
	if c.Svc == nil {
		apispec.NotImplemented(w, r, "POST", "/api/v1/connections/call")
		return
	}
	var request CallConnectionRequest
	if err := decodeJSONStrict(r, &request); err != nil {
		envelope.WriteAPIError(w, r, http.StatusBadRequest, "bad_request", "INVALID_CONNECTION_CALL", "invalid connection call", nil)
		return
	}
	text, err := c.Svc.Call(r.Context(), request.Tool, connections.CallArgs{
		Path: request.Path, Method: request.Method, Body: request.Body, Fields: request.Fields,
	})
	if err != nil {
		envelope.WriteError(w, r, connectionError(err))
		return
	}
	envelope.WriteJSON(w, http.StatusOK, CallConnectionResponse{Text: text})
}

func connectionResponse(state connections.State) (ConnectionResponse, bool) {
	c, ok := connections.Lookup(state.ID)
	if !ok {
		return ConnectionResponse{}, false
	}
	out := ConnectionResponse{
		ID: c.ID, Name: c.Name, Tool: c.Tool, Blurb: c.Blurb, Tint: c.Tint, KeyURL: c.KeyURL,
		Fields: c.Fields, Connected: state.Connected, Tail: state.Tail, Values: state.Values,
	}
	if out.Fields == nil {
		out.Fields = []connections.Field{}
	}
	if !state.ConnectedAt.IsZero() {
		at := state.ConnectedAt
		out.ConnectedAt = &at
	}
	return out, true
}

// connectionError maps the service's errors to the API error envelope. An
// unknown error stays a 500.
func connectionError(err error) error {
	var invalid connections.ValidationError
	switch {
	case errors.Is(err, connections.ErrUnknownConnector):
		return apierr.NotFound("CONNECTION_NOT_FOUND", "unknown connection")
	case errors.As(err, &invalid):
		return apierr.Invalid("CONNECTION_INVALID", invalid.Message, nil)
	case errors.Is(err, connections.ErrBusy):
		return apierr.Conflict("CONNECTION_BUSY", err.Error(), nil)
	case errors.Is(err, connections.ErrStoreUnavailable):
		return apierr.Unavailable("CONNECTIONS_UNAVAILABLE", err.Error())
	case errors.Is(err, connections.ErrStoreDenied):
		return apierr.Forbidden("CONNECTION_KEYCHAIN_DENIED", "the keychain refused access; allow Swarm in the keychain prompt and try again")
	}
	return err
}
