package mcpconn

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
	"time"
)

// Streamable HTTP JSON-RPC for remote MCP servers: initialize, tools/list and
// tools/call. A reply can be JSON or an SSE stream; the reader stops at the
// reply with its id.

const (
	mcpProtocolVersion = "2025-06-18"
	callLimit          = 30 * time.Second
	probeLimit         = 10 * time.Second
	maxReplyBytes      = 1 << 20
	maxTools           = 500
	maxToolPages       = 10
)

// MCPErrorCode names a protocol failure.
type MCPErrorCode string

// The MCP error codes.
const (
	MCPUnauthorized MCPErrorCode = "unauthorized"
	MCPHTTPStatus   MCPErrorCode = "http_status"
	MCPBadReply     MCPErrorCode = "bad_reply"
	MCPRPCError     MCPErrorCode = "rpc_error"
)

// MCPError is a protocol failure. ServerText is the whole JSON-RPC error
// message, with control characters replaced. It is server text: the broker
// removes every secret from it before it cuts it.
type MCPError struct {
	Code            MCPErrorCode
	Message         string
	Status          int
	WWWAuthenticate string
	RPCCode         *int
	ServerText      string
}

func (e *MCPError) Error() string { return e.Message }

// Target is the URL and the headers of one remote request.
type Target struct {
	URL               string
	Headers           map[string]string
	CredentialHeaders []string
}

// Tool is one remote tool.
type Tool struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	InputSchema any    `json:"inputSchema"`
}

// CallResult is the result of tools/call.
type CallResult struct {
	Content           []any `json:"content"`
	IsError           bool  `json:"isError"`
	StructuredContent any   `json:"structuredContent,omitempty"`
}

// cut returns the first n code points.
func cut(text string, n int) string {
	runes := []rune(text)
	if len(runes) <= n {
		return text
	}
	return string(runes[:n])
}

type mcpClient struct {
	http HTTP
}

type mcpSession struct {
	headers map[string]string
	next    int
}

func (c *mcpClient) post(ctx context.Context, target Target, s *mcpSession, message map[string]any) (*GuardedResponse, error) {
	deadline, ok := ctx.Deadline()
	left := callLimit
	if ok {
		if left = time.Until(deadline); left <= 0 {
			return nil, guardErr(GuardTimeout)
		}
	}
	raw, err := json.Marshal(message)
	if err != nil {
		return nil, err
	}
	body := string(raw)
	headers := map[string]string{}
	for k, v := range s.headers {
		headers[k] = v
	}
	headers["content-type"] = "application/json"
	headers["accept"] = "application/json, text/event-stream"
	res, err := c.http(ctx, GuardedRequest{URL: target.URL, Method: "POST", Headers: headers, Body: &body, Timeout: left, CredentialHeaders: target.CredentialHeaders})
	if err != nil {
		return nil, err
	}
	if res.Status == 401 || res.Status == 403 {
		res.Close()
		return nil, &MCPError{Code: MCPUnauthorized, Message: fmt.Sprintf("HTTP %d", res.Status), Status: res.Status, WWWAuthenticate: res.Header.Get("WWW-Authenticate")}
	}
	if res.Status < 200 || res.Status >= 300 {
		res.Close()
		return nil, &MCPError{Code: MCPHTTPStatus, Message: fmt.Sprintf("HTTP %d", res.Status), Status: res.Status}
	}
	return res, nil
}

// findReply returns the reply with this id. A server request has a method and
// can hold the same id, so only a message without a method answers.
func findReply(value any, id int) map[string]any {
	list, ok := value.([]any)
	if !ok {
		list = []any{value}
	}
	for _, m := range list {
		obj, ok := m.(map[string]any)
		if !ok {
			continue
		}
		if _, hasMethod := obj["method"]; hasMethod {
			continue
		}
		got, ok := obj["id"].(json.Number)
		if !ok || got.String() != fmt.Sprint(id) {
			continue
		}
		_, hasResult := obj["result"]
		_, hasError := obj["error"]
		if hasResult || hasError {
			return obj
		}
	}
	return nil
}

func decodeNumber(data []byte) (any, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	return v, nil
}

// splitLines is a bufio.SplitFunc that ends a line at CRLF, CR or LF.
func splitLines(data []byte, atEOF bool) (int, []byte, error) {
	if atEOF && len(data) == 0 {
		return 0, nil, nil
	}
	if i := bytes.IndexAny(data, "\r\n"); i >= 0 {
		if data[i] == '\n' {
			return i + 1, data[:i], nil
		}
		// A CR: one more byte decides whether it is CRLF.
		if i+1 < len(data) {
			if data[i+1] == '\n' {
				return i + 2, data[:i], nil
			}
			return i + 1, data[:i], nil
		}
		if atEOF {
			return i + 1, data[:i], nil
		}
		return 0, nil, nil
	}
	if atEOF {
		return len(data), data, nil
	}
	return 0, nil, nil
}

// readReply reads until the reply with this id. An SSE stream can stay open
// after the reply, so the reader stops at the reply.
func readReply(ctx context.Context, res *GuardedResponse, id int, maxBytes int64) (map[string]any, error) {
	defer res.Close()
	limited := &countingReader{r: res.Body, max: maxBytes}
	sse := strings.Contains(strings.ToLower(res.Header.Get("Content-Type")), "text/event-stream")
	if !sse {
		raw, err := io.ReadAll(limited)
		if err != nil {
			return nil, readError(ctx, err)
		}
		v, err := decodeNumber(raw)
		if err != nil {
			return nil, &MCPError{Code: MCPBadReply, Message: "The server sent JSON that is not valid."}
		}
		if found := findReply(v, id); found != nil {
			return found, nil
		}
		return nil, &MCPError{Code: MCPBadReply, Message: "The reply did not answer the request."}
	}
	scanner := bufio.NewScanner(limited)
	scanner.Buffer(make([]byte, 64*1024), int(maxBytes)+1)
	scanner.Split(splitLines)
	name := ""
	var data []string
	flush := func() map[string]any {
		defer func() { name, data = "", nil }()
		// Skip other event types (for example ping) and data that is not JSON.
		if (name != "" && name != "message") || len(data) == 0 {
			return nil
		}
		v, err := decodeNumber([]byte(strings.Join(data, "\n")))
		if err != nil {
			return nil
		}
		return findReply(v, id)
	}
	for scanner.Scan() {
		line := scanner.Text()
		switch {
		case line == "":
			if found := flush(); found != nil {
				return found, nil
			}
		case strings.HasPrefix(line, ":"):
		case strings.HasPrefix(line, "event:"):
			name = strings.TrimSpace(line[len("event:"):])
		case strings.HasPrefix(line, "data:"):
			data = append(data, strings.TrimPrefix(line[len("data:"):], " "))
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, readError(ctx, err)
	}
	if found := flush(); found != nil {
		return found, nil
	}
	return nil, &MCPError{Code: MCPBadReply, Message: "The reply did not answer the request."}
}

func readError(ctx context.Context, err error) error {
	var guard *GuardError
	if errors.As(err, &guard) {
		return guard
	}
	if errors.Is(err, bufio.ErrTooLong) {
		return guardErr(GuardTooLarge)
	}
	if ctx.Err() != nil || errors.Is(err, context.DeadlineExceeded) {
		return guardErr(GuardTimeout)
	}
	return &GuardError{Code: GuardNetwork, Detail: "read"}
}

// countingReader fails with too_large after max bytes.
type countingReader struct {
	r   io.Reader
	n   int64
	max int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	if c.n > c.max {
		return n, guardErr(GuardTooLarge)
	}
	return n, err
}

var controlChars = regexp.MustCompile(`[\x00-\x1f\x7f]`)

func rpcResult(reply map[string]any) (any, error) {
	if raw, ok := reply["error"]; ok && raw != nil {
		e, _ := raw.(map[string]any)
		message, _ := e["message"].(string)
		if message == "" {
			message = "error"
		}
		var code *int
		if n, ok := e["code"].(json.Number); ok {
			if v, err := n.Int64(); err == nil {
				c := int(v)
				code = &c
			}
		}
		clean := controlChars.ReplaceAllString(message, " ")
		return nil, &MCPError{Code: MCPRPCError, Message: cut(clean, 200), RPCCode: code, ServerText: clean}
	}
	return reply["result"], nil
}

func asObject(v any) (map[string]any, error) {
	obj, ok := v.(map[string]any)
	if !ok {
		return nil, &MCPError{Code: MCPBadReply, Message: "The server sent a result that is not valid."}
	}
	return obj, nil
}

func (c *mcpClient) request(ctx context.Context, target Target, s *mcpSession, method string, params any) (any, error) {
	id := s.next
	s.next++
	res, err := c.post(ctx, target, s, map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params})
	if err != nil {
		return nil, err
	}
	reply, err := readReply(ctx, res, id, maxReplyBytes)
	if err != nil {
		return nil, err
	}
	return rpcResult(reply)
}

var (
	protocolVersionPattern = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`)
	sessionIDPattern       = regexp.MustCompile(`^[\x21-\x7e]{1,256}$`)
	toolNamePattern        = regexp.MustCompile(`^[\x21-\x7e]{1,128}$`)
)

// open sends initialize, then notifications/initialized. The session headers
// go on every later request.
func (c *mcpClient) open(ctx context.Context, target Target) (*mcpSession, error) {
	s := &mcpSession{headers: map[string]string{}, next: 1}
	for k, v := range target.Headers {
		s.headers[k] = v
	}
	id := s.next
	s.next++
	res, err := c.post(ctx, target, s, map[string]any{"jsonrpc": "2.0", "id": id, "method": "initialize", "params": map[string]any{
		"protocolVersion": mcpProtocolVersion, "capabilities": map[string]any{}, "clientInfo": map[string]any{"name": "swarm-3", "version": "1"},
	}})
	if err != nil {
		return nil, err
	}
	sessionID, hasSession := res.Header.Get("Mcp-Session-Id"), len(res.Header.Values("Mcp-Session-Id")) > 0
	reply, err := readReply(ctx, res, id, maxReplyBytes)
	if err != nil {
		return nil, err
	}
	init, err := rpcResult(reply)
	if err != nil {
		return nil, err
	}
	// Both values come from a remote server. Check them before they go into a header.
	version := mcpProtocolVersion
	if obj, ok := init.(map[string]any); ok {
		if v, ok := obj["protocolVersion"].(string); ok && protocolVersionPattern.MatchString(v) {
			version = v
		}
	}
	s.headers["mcp-protocol-version"] = version
	if hasSession {
		if !sessionIDPattern.MatchString(sessionID) {
			return nil, &MCPError{Code: MCPBadReply, Message: "The server sent a session id that is not valid."}
		}
		s.headers["mcp-session-id"] = sessionID
	}
	notified, err := c.post(ctx, target, s, map[string]any{"jsonrpc": "2.0", "method": "notifications/initialized"})
	if err != nil {
		// Some servers refuse a notification with an HTTP error. Only a refused credential stops the session.
		var mcpErr *MCPError
		if !errors.As(err, &mcpErr) || mcpErr.Code != MCPHTTPStatus {
			return nil, err
		}
	} else {
		notified.Close()
	}
	return s, nil
}

func cleanTool(raw any) (Tool, bool) {
	t, ok := raw.(map[string]any)
	if !ok {
		return Tool{}, false
	}
	name, ok := t["name"].(string)
	if !ok || !toolNamePattern.MatchString(name) {
		return Tool{}, false
	}
	description, _ := t["description"].(string)
	schema, ok := t["inputSchema"].(map[string]any)
	var input any = map[string]any{"type": "object"}
	if ok {
		input = schema
	}
	return Tool{Name: name, Description: cut(description, 4000), InputSchema: input}, true
}

// listTools reads the whole tool list, up to 10 pages and 500 tools.
func (c *mcpClient) listTools(ctx context.Context, target Target) ([]Tool, error) {
	s, err := c.open(ctx, target)
	if err != nil {
		return nil, err
	}
	tools := []Tool{}
	names := map[string]bool{}
	cursors := map[string]bool{}
	cursor := ""
	for page := 0; page < maxToolPages && len(tools) < maxTools; page++ {
		params := map[string]any{}
		if cursor != "" {
			params["cursor"] = cursor
		}
		raw, err := c.request(ctx, target, s, "tools/list", params)
		if err != nil {
			return nil, err
		}
		r, err := asObject(raw)
		if err != nil {
			return nil, err
		}
		list, _ := r["tools"].([]any)
		for _, item := range list {
			if tool, ok := cleanTool(item); ok && len(tools) < maxTools && !names[tool.Name] {
				names[tool.Name] = true
				tools = append(tools, tool)
			}
		}
		cursor, _ = r["nextCursor"].(string)
		// A cursor that came before would loop. Stop at it.
		if cursor == "" || cursors[cursor] {
			break
		}
		cursors[cursor] = true
	}
	return tools, nil
}

// callTool calls one remote tool.
func (c *mcpClient) callTool(ctx context.Context, target Target, name string, args map[string]any) (CallResult, error) {
	s, err := c.open(ctx, target)
	if err != nil {
		return CallResult{}, err
	}
	raw, err := c.request(ctx, target, s, "tools/call", map[string]any{"name": name, "arguments": args})
	if err != nil {
		return CallResult{}, err
	}
	r, err := asObject(raw)
	if err != nil {
		return CallResult{}, err
	}
	content, _ := r["content"].([]any)
	if content == nil {
		content = []any{}
	}
	out := CallResult{Content: content, IsError: r["isError"] == true}
	if sc, ok := r["structuredContent"]; ok {
		out.StructuredContent = sc
	}
	return out, nil
}
