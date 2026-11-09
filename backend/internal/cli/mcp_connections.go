package cli

import (
	"context"
	"encoding/json"
	"fmt"
)

// connectionAgentTool and connectionCallRequest mirror the daemon's
// /connections DTOs.
type connectionAgentTool struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
}

type connectionCallRequest struct {
	Tool   string `json:"tool"`
	Path   string `json:"path"`
	Method string `json:"method,omitempty"`
	Body   string `json:"body,omitempty"`
	Fields string `json:"fields,omitempty"`
}

// connectionTools are the tools of the services connected in Settings >
// Connections, one per service. The list is read again on each tools/list, so
// a service connected after the session started shows up when the harness
// next asks. A daemon that cannot answer gives no connection tools.
func (c *commandContext) connectionTools(ctx context.Context, state *mcpState) []map[string]any {
	var response struct {
		Tools []connectionAgentTool `json:"tools"`
	}
	if err := c.getJSON(ctx, "connections/agent-tools", &response); err != nil {
		return nil
	}
	state.connectionNames = map[string]bool{}
	tools := make([]map[string]any, 0, len(response.Tools))
	for _, tool := range response.Tools {
		state.connectionNames[tool.Name] = true
		tools = append(tools, map[string]any{
			"name":        tool.Name,
			"description": tool.Description,
			"inputSchema": tool.InputSchema,
			// A call can write to the service as the user, so it is not read-only.
			"annotations": map[string]any{
				"readOnlyHint": false, "destructiveHint": true, "idempotentHint": false, "openWorldHint": true,
			},
		})
	}
	return tools
}

// isConnectionTool reports a connection tool name. A name that the last list
// did not have is checked against a new list, for a harness that calls a tool
// before it asks for the list.
func (c *commandContext) isConnectionTool(ctx context.Context, state *mcpState, name string) bool {
	if state.connectionNames[name] {
		return true
	}
	c.connectionTools(ctx, state)
	return state.connectionNames[name]
}

// callConnection runs one connection call through the daemon, which adds the
// key. The agent reads the status line and the body as text.
func (c *commandContext) callConnection(ctx context.Context, name string, raw json.RawMessage) map[string]any {
	request := connectionCallRequest{Tool: name}
	if len(raw) > 0 && string(raw) != "null" {
		var args struct {
			Path   any `json:"path"`
			Method any `json:"method"`
			Body   any `json:"body"`
			Fields any `json:"fields"`
		}
		if err := json.Unmarshal(raw, &args); err != nil {
			return mcpToolError(fmt.Errorf("invalid arguments: %w", err))
		}
		request.Path = mcpString(args.Path)
		request.Method = mcpString(args.Method)
		request.Body = mcpBody(args.Body)
		request.Fields = mcpString(args.Fields)
	}
	var response struct {
		Text string `json:"text"`
	}
	if err := c.postJSON(ctx, "connections/call", request, &response); err != nil {
		return mcpToolError(err)
	}
	return map[string]any{"content": []any{mcpText(response.Text)}}
}

// mcpString keeps a string argument and drops any other type, which a harness
// can send for a flag with no value.
func mcpString(v any) string {
	s, _ := v.(string)
	return s
}

// mcpBody takes the body as a string. An agent that sends a JSON object
// instead gets it encoded, which is what it meant.
func mcpBody(v any) string {
	switch body := v.(type) {
	case nil:
		return ""
	case string:
		return body
	default:
		encoded, err := json.Marshal(body)
		if err != nil {
			return ""
		}
		return string(encoded)
	}
}
