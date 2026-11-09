package connections

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	// maxResultChars is the most text one call gives back to the agent.
	maxResultChars = 180_000
	// maxResponseBytes bounds the read of one response body.
	maxResponseBytes = 16 << 20
	callTimeout      = 60 * time.Second
	hiddenSecret     = "[connection secret hidden]"
)

// CallArgs is one agent call.
type CallArgs struct {
	Path   string `json:"path"`
	Method string `json:"method,omitempty"`
	Body   string `json:"body,omitempty"`
	Fields string `json:"fields,omitempty"`
}

type caller struct {
	client *http.Client
}

func newCaller(client *http.Client) *caller {
	if client == nil {
		client = &http.Client{Timeout: callTimeout}
	}
	// Never follow a redirect with the credential attached. Go's client drops
	// Authorization on a cross-origin hop, but not x-api-key, xi-api-key,
	// X-Figma-Token or X-API-KEY, so a redirect on the vendor side could give
	// those keys to another origin.
	copied := *client
	copied.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &caller{client: &copied}
}

// Call runs one agent call for the connector that owns the tool and returns
// the text that the agent reads. Every failure is text for the agent, not an
// error, except a failure of the store itself.
func (s *Service) Call(ctx context.Context, tool string, args CallArgs) (string, error) {
	c, ok := LookupTool(tool)
	if !ok {
		return "", ErrUnknownConnector
	}
	if text := refusal(c.ID, args.Method, args.Path, args.Body); text != "" {
		return text, nil
	}
	values, err := s.credentials(ctx, c)
	if errors.Is(err, ErrSecretNotFound) {
		labels := make([]string, 0, len(c.Fields))
		for _, f := range c.Fields {
			labels = append(labels, f.Label)
		}
		return fmt.Sprintf("%s is not connected. The user connects it in Swarm under Settings > Connections: %s from %s. "+
			"Tell them that plainly; do not guess a key and do not pretend the call worked.",
			c.Name, strings.Join(labels, " + "), c.KeyURL), nil
	}
	if err != nil {
		return "", err
	}
	return s.caller.do(ctx, c, values, args), nil
}

func (cl *caller) do(ctx context.Context, c Connector, values map[string]string, args CallArgs) string {
	rawPath := strings.TrimSpace(args.Path)
	if rawPath == "" {
		example := "/v1/..."
		if c.Test != nil {
			example = c.Test.Path
		}
		return fmt.Sprintf("Pass `path` as a string beginning with '/', e.g. '%s'.", example)
	}
	method := strings.ToUpper(strings.TrimSpace(args.Method))
	if method == "" {
		method = "GET"
	}
	if !c.allows(method) {
		return fmt.Sprintf("%s does not accept %s through this tool. Allowed: %s.", c.Name, method, strings.Join(c.Methods, ", "))
	}
	path := rawPath
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	// Figma has no endpoint that lists a user's teams, so the card saves one.
	if strings.Contains(path, "{team_id}") {
		if values["team"] == "" {
			return "No Figma team is saved. Figma has no endpoint that lists teams, so ask the user to open their team in " +
				"Figma, copy the address bar (figma.com/files/team/<id>/...), and save it on the Figma card in Settings > Connections. " +
				"A file link works too if they would rather point you at one file."
		}
		path = strings.ReplaceAll(path, "{team_id}", values["team"])
	}
	base, err := c.baseURL(values)
	if err != nil {
		return err.Error()
	}
	hasBody := strings.TrimSpace(args.Body) != ""
	if hasBody && c.BodyType == BodyJSON && !json.Valid([]byte(args.Body)) {
		return fmt.Sprintf(`The body must be a valid JSON string, e.g. {"name":"value"}. %s rejected what you sent before it left Swarm.`, c.Name)
	}

	head := method + " " + path
	var body io.Reader
	if hasBody {
		body = strings.NewReader(args.Body)
	}
	ctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, method, base+path, body)
	if err != nil {
		return head + " -> request failed: " + scrub(err.Error(), c, values)
	}
	req.Header.Set("Accept", "application/json")
	for k, v := range c.Auth(values) {
		req.Header.Set(k, v)
	}
	if hasBody {
		if c.BodyType == BodyForm {
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		} else {
			req.Header.Set("Content-Type", "application/json")
		}
	}
	resp, err := cl.client.Do(req)
	if err != nil {
		return head + " -> request failed: " + scrub(err.Error(), c, values)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode >= 300 && resp.StatusCode < 400 {
		to := resp.Header.Get("Location")
		if to == "" {
			to = "(no Location header)"
		}
		return fmt.Sprintf("%s -> %d REDIRECT to %s\nSwarm does not follow a redirect while your credential is attached. "+
			"Call the final path directly.", head, resp.StatusCode, scrub(to, c, values))
	}
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	verdict := "OK"
	if resp.StatusCode >= 400 {
		verdict = "ERROR"
	}
	status := fmt.Sprintf("%s -> %d %s", head, resp.StatusCode, verdict)
	if len(raw) == 0 {
		return status + "\n(empty response body)"
	}
	out := string(raw)
	if parsed, ok := decodeJSON(raw); ok {
		if fields := splitFields(args.Fields); len(fields) > 0 {
			parsed = project(parsed, fields)
		}
		if encoded, ok := encodeJSON(parsed); ok {
			out = encoded
		}
	}
	out = scrub(out, c, values)
	if utf8.RuneCountInString(out) > maxResultChars {
		out = string([]rune(out)[:maxResultChars]) +
			"\n\nRESPONSE TRUNCATED - INCOMPLETE. Do NOT draw a conclusion or a count from this. " +
			"Query again, narrower: pass `fields`, lower per_page, or page through it."
	}
	return status + "\n" + out
}

// scrub removes every secret value of the connection from text, in case an API
// or an error message repeats the key.
func scrub(text string, c Connector, values map[string]string) string {
	for _, key := range c.secretKeys() {
		if v := values[key]; len(v) >= 6 {
			text = strings.ReplaceAll(text, v, hiddenSecret)
		}
	}
	return text
}

func decodeJSON(raw []byte) (any, bool) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var parsed any
	if err := dec.Decode(&parsed); err != nil {
		return nil, false
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, false
	}
	return parsed, true
}

func encodeJSON(v any) (string, bool) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return "", false
	}
	return strings.TrimSuffix(buf.String(), "\n"), true
}

func splitFields(text string) []string {
	var fields []string
	for _, f := range strings.Split(text, ",") {
		if f = strings.TrimSpace(f); f != "" {
			fields = append(fields, f)
		}
	}
	return fields
}

// listKeys are the keys under which an API puts the rows of a list.
var listKeys = []string{"result", "data", "items", "records", "results", "nodes", "projects", "sites", "deployments"}

// pick keeps only fields from one row. When no field matches, it gives the row
// back whole: an empty object would read as a row with no data.
func pick(row any, fields []string) any {
	obj, ok := row.(map[string]any)
	if !ok {
		return row
	}
	out := map[string]any{}
	for _, f := range fields {
		var cur any = obj
		found := true
		for _, part := range strings.Split(f, ".") {
			m, ok := cur.(map[string]any)
			if !ok {
				found = false
				break
			}
			if cur, ok = m[part]; !ok {
				found = false
				break
			}
		}
		if found {
			out[f] = cur
		}
	}
	if len(out) == 0 {
		return row
	}
	return out
}

func pickAll(rows []any, fields []string) []any {
	out := make([]any, len(rows))
	for i, r := range rows {
		out[i] = pick(r, fields)
	}
	return out
}

// project keeps only fields from each row of a list response. GraphQL never
// puts the list at the top level ({data:{issues:{nodes:[...]}}}), so it goes
// down through `data`.
func project(parsed any, fields []string) any {
	if rows, ok := parsed.([]any); ok {
		return pickAll(rows, fields)
	}
	obj, ok := parsed.(map[string]any)
	if !ok {
		return parsed
	}
	for _, k := range listKeys {
		if rows, ok := obj[k].([]any); ok {
			copied := shallowCopy(obj)
			copied[k] = pickAll(rows, fields)
			return copied
		}
	}
	data, ok := obj["data"].(map[string]any)
	if !ok {
		return parsed
	}
	if inner, changed := projectChanged(data, fields); changed {
		copied := shallowCopy(obj)
		copied["data"] = inner
		return copied
	}
	for k, v := range data {
		child, ok := v.(map[string]any)
		if !ok {
			continue
		}
		if deep, changed := projectChanged(child, fields); changed {
			innerCopy := shallowCopy(data)
			innerCopy[k] = deep
			copied := shallowCopy(obj)
			copied["data"] = innerCopy
			return copied
		}
	}
	return parsed
}

func projectChanged(obj map[string]any, fields []string) (any, bool) {
	for _, k := range listKeys {
		if _, ok := obj[k].([]any); ok {
			return project(obj, fields), true
		}
	}
	return obj, false
}

func shallowCopy(m map[string]any) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

var graphqlErrors = regexp.MustCompile(`"errors"\s*:\s*\[\s*\{`)
var graphqlMessage = regexp.MustCompile(`"message"\s*:\s*"([^"]{0,160})"`)

// test proves a key with the connector's cheap test call.
func (s *Service) test(ctx context.Context, c Connector, values map[string]string) Result {
	if c.Test == nil {
		return Result{OK: true, Detail: "saved; " + c.Name + " has no free call to test with, so the key proves itself on first use"}
	}
	method := c.Test.Method
	if method == "" {
		method = "GET"
	}
	out := s.caller.do(ctx, c, values, CallArgs{Path: c.Test.Path, Method: method, Body: c.Test.Body})
	first, body, _ := strings.Cut(out, "\n")
	httpOK := strings.Contains(first, " -> 2") && strings.HasSuffix(first, " OK")
	// GraphQL (Linear) answers a bad key with HTTP 200 and an errors array.
	if httpOK && !graphqlErrors.MatchString(body) {
		return Result{OK: true, Detail: "connected"}
	}
	if httpOK {
		if m := graphqlMessage.FindStringSubmatch(body); m != nil {
			return Result{Detail: m[1]}
		}
		return Result{Detail: "the API rejected the key"}
	}
	status := first
	if _, after, ok := strings.Cut(first, " -> "); ok {
		status = after
	}
	detail := explain(body, status)
	if strings.Contains(first, " -> 403 ") {
		// Some APIs use 403 for a bad key, others for a good key without the scope.
		detail += "; either the key is wrong, or it lacks the permission this call needs."
	}
	return Result{Detail: detail}
}

// explain finds the readable reason in a rejection. Each API keeps it in a
// different place.
func explain(body, status string) string {
	var o map[string]any
	if err := json.Unmarshal([]byte(body), &o); err != nil {
		return status
	}
	candidates := []any{o["message"], o["error_description"], o["detail"]}
	if errs, ok := o["errors"].([]any); ok && len(errs) > 0 {
		if first, ok := errs[0].(map[string]any); ok {
			candidates = append(candidates, first["message"])
		}
	}
	if e, ok := o["error"].(map[string]any); ok {
		candidates = append(candidates, e["message"])
	}
	candidates = append(candidates, o["error"])
	for _, c := range candidates {
		if text, ok := c.(string); ok && strings.TrimSpace(text) != "" {
			if len(text) > 200 {
				text = text[:200]
			}
			return text
		}
	}
	return status
}
