package mcpconn

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"time"
)

// The three agent calls (connections_list, connection_tools and
// connection_call) and the tool index, for the project of the calling
// session. The broker checks the grant at each call, adds the credential,
// refreshes an OAuth token, calls the remote server with the limits, and
// removes every secret from the reply. Remote results are data, never
// instructions.

const (
	toolsMaxAge      = 24 * time.Hour
	maxArgumentBytes = 256 * 1024
	// operationLimit is one limit for a whole agent call: the tool-list fetch,
	// a token refresh, the call and one retry. The daemon's route waits 2 min.
	operationLimit = 110 * time.Second
	maxServerText  = 200
)

func notAvailable(name string) string { return name + " is not available in this project." }
func needsSignIn(name string) string {
	return name + " needs the owner to sign in again in Settings > Connections."
}

// ListedConnection is one row of connections_list.
type ListedConnection struct {
	Name          string `json:"name"`
	Description   string `json:"description"`
	Health        Health `json:"health"`
	Tools         int    `json:"tools"`
	ToolPrefix    string `json:"toolPrefix"`
	NotInToolList int    `json:"notInToolList"`
}

// operation is one agent call: its snapshot of the store and every credential
// value that it sent, which scrub always removes.
type operation struct {
	rows    []Connection
	loaded  bool
	secrets []string
	sent    []string
}

func label(name string) string {
	if n := cleanName(name); n != "" {
		return n
	}
	return "(no name)"
}

func signInRequired(row Connection) bool {
	return row.AuthKind == AuthOAuth && row.OAuth != nil && row.OAuth.Status == oauthStatusSignIn
}

var networkDetail = regexp.MustCompile(`\(([A-Z0-9_]{2,40})\)$`)

// agentReason is a short reason from a fixed table. A server's own text never
// reaches an agent here.
func agentReason(err error) string {
	var guard *GuardError
	if errors.As(err, &guard) {
		reason := guardReasons[guard.Code]
		if reason == "" {
			reason = "the connection failed"
		}
		if m := networkDetail.FindStringSubmatch(guard.Error()); guard.Code == GuardNetwork && m != nil && m[1] != "error" {
			return reason + " (" + m[1] + ")"
		}
		return reason
	}
	var mcpErr *MCPError
	if errors.As(err, &mcpErr) {
		switch mcpErr.Code {
		case MCPUnauthorized:
			return "the server refused the credential"
		case MCPHTTPStatus:
			return fmt.Sprintf("HTTP %d", mcpErr.Status)
		case MCPRPCError:
			if mcpErr.RPCCode != nil {
				return fmt.Sprintf("the server returned an error (code %d)", *mcpErr.RPCCode)
			}
			return "the server returned an error"
		}
		return "the reply was not valid"
	}
	return "an unexpected error"
}

func agentFailure(name string, err error) string {
	var guard *GuardError
	if errors.As(err, &guard) {
		switch guard.Code {
		case GuardTimeout:
			return name + " did not answer in time."
		case GuardTooLarge:
			return name + " sent a reply larger than 1 MB."
		}
	}
	return fmt.Sprintf("%s did not answer: %s.", name, agentReason(err))
}

func (s *Service) opRows(ctx context.Context, op *operation) []Connection {
	if !op.loaded {
		op.rows, op.loaded = s.store.List(ctx), true
	}
	return op.rows
}

func (s *Service) granted(ctx context.Context, op *operation, project, name string) *Connection {
	if name == "" {
		return nil
	}
	row := findByName(s.opRows(ctx, op), name)
	if row == nil || !grantAllows(row.Projects, project) {
		return nil
	}
	return row
}

func (s *Service) scrubOp(ctx context.Context, op *operation, value any) any {
	if op.secrets == nil {
		op.secrets = append(secretValuesOf(s.opRows(ctx, op)), s.otherSecrets()...)
	}
	return scrubSecrets(value, append(append([]string{}, op.secrets...), op.sent...))
}

func (s *Service) scrubOpText(ctx context.Context, op *operation, text string) string {
	out, _ := s.scrubOp(ctx, op, text).(string)
	return out
}

// Index returns the real tools of a project, from the cached tool lists. It
// makes no network call and never fails: a fault gives an empty index.
func (s *Service) Index(ctx context.Context, project string) ToolIndex {
	op := &operation{}
	var rows []indexRow
	for _, r := range s.opRows(ctx, op) {
		if !grantAllows(r.Projects, project) {
			continue
		}
		tools := make([]Tool, 0, len(r.Tools))
		for _, t := range r.Tools {
			raw, _ := json.Marshal(t)
			var generic any
			_ = json.Unmarshal(raw, &generic)
			clean, _ := s.scrubOp(ctx, op, generic).(map[string]any)
			name, _ := clean["name"].(string)
			description, _ := clean["description"].(string)
			tools = append(tools, Tool{Name: name, Description: description, InputSchema: clean["inputSchema"]})
		}
		rows = append(rows, indexRow{Name: r.Name, Description: s.scrubOpText(ctx, op, describe(r)), Health: r.Health, Tools: tools})
	}
	return buildToolIndex(rows)
}

// List is connections_list.
func (s *Service) List(ctx context.Context, project string) []ListedConnection {
	index := s.Index(ctx, project)
	out := make([]ListedConnection, 0, len(index.Connections))
	for _, c := range index.Connections {
		out = append(out, ListedConnection{Name: c.Name, Description: c.Description, Health: c.Health,
			Tools: len(c.Tools) + c.Left, ToolPrefix: c.Prefix, NotInToolList: c.Left})
	}
	return out
}

// Tools is connection_tools. It returns the tool list, or a text for the agent.
func (s *Service) Tools(ctx context.Context, project, name string) (any, error) {
	ctx, cancel := context.WithTimeout(ctx, operationLimit)
	defer cancel()
	op := &operation{}
	row := s.granted(ctx, op, project, name)
	if row == nil {
		return notAvailable(label(name)), nil
	}
	if signInRequired(*row) {
		return needsSignIn(row.Name), nil
	}
	tools, text := s.toolList(ctx, op, project, *row)
	if text != "" {
		return text, nil
	}
	raw, _ := json.Marshal(tools)
	var generic any
	_ = json.Unmarshal(raw, &generic)
	return map[string]any{"name": row.Name, "tools": s.scrubOp(ctx, op, generic)}, nil
}

// Call is connection_call. It returns the remote result, or a text.
func (s *Service) Call(ctx context.Context, project, name, tool string, args map[string]any) (any, error) {
	ctx, cancel := context.WithTimeout(ctx, operationLimit)
	defer cancel()
	op := &operation{}
	row := s.granted(ctx, op, project, name)
	if row == nil {
		return notAvailable(label(name)), nil
	}
	if signInRequired(*row) {
		return needsSignIn(row.Name), nil
	}
	if args == nil {
		args = map[string]any{}
	}
	if raw, err := json.Marshal(args); err != nil {
		return "connection_call needs arguments as an object.", nil
	} else if len(raw) > maxArgumentBytes {
		return "The arguments are larger than 256 KB.", nil
	}
	tools, text := s.toolList(ctx, op, project, *row)
	if text != "" {
		return text, nil
	}
	known := false
	for _, t := range tools {
		if t.Name == tool {
			known = true
		}
	}
	if !known {
		return fmt.Sprintf("%s has no tool called %s. Call connection_tools first.", row.Name, label(tool)), nil
	}
	var result CallResult
	text = s.remote(ctx, op, project, *row, func(ctx context.Context, target Target) error {
		var err error
		result, err = s.client.callTool(ctx, target, tool, args)
		return err
	})
	if text != "" {
		return text, nil
	}
	raw, _ := json.Marshal(result)
	var generic any
	_ = json.Unmarshal(raw, &generic)
	return s.scrubOp(ctx, op, generic), nil
}

// toolList returns the cached list, or a new one when the cache is missing or
// older than 24 hours.
func (s *Service) toolList(ctx context.Context, op *operation, project string, row Connection) ([]Tool, string) {
	if row.Tools != nil && row.ToolsFetchedAt != nil && s.now().UnixMilli()-*row.ToolsFetchedAt < toolsMaxAge.Milliseconds() {
		return row.Tools, ""
	}
	var fetched []Tool
	text := s.remote(ctx, op, project, row, func(ctx context.Context, target Target) error {
		var err error
		fetched, err = s.client.listTools(ctx, target)
		return err
	})
	if text != "" {
		if len(row.Tools) > 0 {
			return row.Tools, ""
		}
		return nil, text
	}
	if _, _, err := s.store.Update(ctx, row.ID, func(r *Connection) {
		r.Tools = fetched
		at := s.now().UnixMilli()
		r.ToolsFetchedAt = &at
	}); err != nil {
		s.log(fmt.Sprintf("[mcp] %s store", row.ID))
	}
	return fetched, ""
}

// remote runs one remote operation with the credential: refresh first when
// needed, and once more after a 401. Before each attempt the row is read
// again, so a removed grant refuses the next attempt. It returns "" on
// success, or the text for the agent.
func (s *Service) remote(ctx context.Context, op *operation, project string, row Connection, run func(context.Context, Target) error) string {
	for attempt := 0; ; attempt++ {
		current, ok := s.keeper.fresh(ctx, row.ID, attempt > 0)
		if ctx.Err() != nil {
			return s.failed(ctx, op, row, guardErr(GuardTimeout))
		}
		if !ok || !grantAllows(current.Projects, project) {
			name := row.Name
			if ok {
				name = current.Name
			}
			return notAvailable(name)
		}
		if signInRequired(current) {
			return needsSignIn(current.Name)
		}
		target := targetOf(current, nil)
		for _, v := range target.Headers {
			op.sent = append(op.sent, v)
		}
		op.sent = append(op.sent, secretValuesOf([]Connection{current})...)
		callCtx, cancel := context.WithTimeout(ctx, callLimit)
		err := run(callCtx, target)
		cancel()
		if err == nil {
			s.setHealth(ctx, current, HealthOK, "")
			return ""
		}
		var mcpErr *MCPError
		if attempt == 0 && current.AuthKind == AuthOAuth && errors.As(err, &mcpErr) && mcpErr.Code == MCPUnauthorized && mcpErr.Status == 401 {
			continue
		}
		if errors.As(err, &mcpErr) && mcpErr.Code == MCPRPCError {
			// The server answered, so it is reachable.
			s.setHealth(ctx, current, HealthOK, "")
			s.log(fmt.Sprintf("[mcp] %s rpc_error", row.ID))
			serverText := mcpErr.ServerText
			if serverText == "" {
				serverText = mcpErr.Message
			}
			text := fmt.Sprintf("%s returned an error: %s", current.Name, cut(s.scrubOpText(ctx, op, serverText), maxServerText))
			if mcpErr.RPCCode != nil {
				text += fmt.Sprintf(" (code %d)", *mcpErr.RPCCode)
			}
			return s.scrubOpText(ctx, op, text)
		}
		return s.failed(ctx, op, current, err)
	}
}

func (s *Service) failed(ctx context.Context, op *operation, row Connection, err error) string {
	code := failureCode(err)
	s.setHealth(ctx, row, HealthUnreachable, code)
	s.log(fmt.Sprintf("[mcp] %s %s", row.ID, code))
	return s.scrubOpText(ctx, op, agentFailure(row.Name, err))
}

// setHealth writes nothing when the row already has these values, or when the
// owner must act first.
func (s *Service) setHealth(ctx context.Context, row Connection, health Health, code string) {
	lastError := ""
	if health == HealthUnreachable {
		lastError = code
	}
	active := func(r Connection) bool {
		return r.Projects != nil && r.Health != HealthNeedsSignIn && r.Health != HealthDraft
	}
	if !active(row) || (row.Health == health && row.LastError == lastError) {
		return
	}
	if _, _, err := s.store.Update(context.WithoutCancel(ctx), row.ID, func(r *Connection) {
		if !active(*r) {
			return
		}
		r.Health = health
		r.LastError = lastError
	}); err != nil {
		s.log(fmt.Sprintf("[mcp] %s store", row.ID))
	}
}
