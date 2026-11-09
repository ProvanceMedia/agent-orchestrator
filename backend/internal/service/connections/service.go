package connections

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// SecretStore keeps the field values of each connection, keyed by connector
// id. The production store is the macOS login keychain.
type SecretStore interface {
	// Available reports whether this build can keep secrets at all.
	Available() bool
	Get(ctx context.Context, id string) (map[string]string, error)
	Put(ctx context.Context, id string, values map[string]string) error
	Delete(ctx context.Context, id string) error
}

var (
	// ErrSecretNotFound is the store's answer for a connection it does not hold.
	ErrSecretNotFound = errors.New("connection secret not found")
	// ErrStoreUnavailable means that this build has no secret store.
	ErrStoreUnavailable = errors.New("connections need the macOS keychain, which this build cannot use")
	// ErrUnknownConnector is an id that is not in the catalogue.
	ErrUnknownConnector = errors.New("unknown connection")
	// ErrStoreDenied means that the user or the system refused access to the
	// secret store, for example a keychain prompt that the user denied.
	ErrStoreDenied = errors.New("the keychain refused access to the connection secret")
	// ErrBusy means that a check for the same connection is still running.
	ErrBusy = errors.New("a check is already running for this connection; wait for it to finish")
)

// ValidationError is a user error in the values given for a connection.
type ValidationError struct{ Message string }

func (e ValidationError) Error() string { return e.Message }

// State is what the settings page may know about one connection. It never
// holds a secret value.
type State struct {
	ID        string
	Connected bool
	// Tail is the last 4 characters of the main secret, so the user can see
	// which key is saved.
	Tail        string
	Values      map[string]string
	ConnectedAt time.Time
}

// Result is the outcome of a key check.
type Result struct {
	OK     bool
	Detail string
}

// indexEntry is the non-secret record of one connection, kept in a file in the
// data directory so that a list never reads the keychain.
type indexEntry struct {
	Tail        string            `json:"tail,omitempty"`
	Values      map[string]string `json:"values,omitempty"`
	ConnectedAt time.Time         `json:"connectedAt"`
}

// Service holds the connections and runs the agents' calls.
type Service struct {
	store     SecretStore
	indexPath string
	caller    *caller

	mu       sync.Mutex
	inFlight map[string]bool
	// cache keeps the values read from the store, so that a call does not read
	// the keychain each time.
	cache map[string]map[string]string
}

// New returns a Service that keeps its index file in dataDir.
func New(store SecretStore, dataDir string) *Service {
	return &Service{
		store:     store,
		indexPath: filepath.Join(dataDir, "connections.json"),
		caller:    newCaller(nil),
		inFlight:  map[string]bool{},
		cache:     map[string]map[string]string{},
	}
}

// Available reports whether connections can be saved on this machine.
func (s *Service) Available() bool { return s.store != nil && s.store.Available() }

// List returns the state of every connector in the catalogue.
func (s *Service) List(_ context.Context) ([]State, error) {
	s.mu.Lock()
	index, err := s.readIndex()
	s.mu.Unlock()
	if err != nil {
		return nil, err
	}
	states := make([]State, 0, len(catalogue))
	for _, c := range catalogue {
		state := State{ID: c.ID}
		if entry, ok := index[c.ID]; ok {
			state.Connected = true
			state.Tail = entry.Tail
			state.Values = entry.Values
			state.ConnectedAt = entry.ConnectedAt
		}
		states = append(states, state)
	}
	return states, nil
}

// Connected returns the connectors that have a saved key, in catalogue order.
func (s *Service) Connected(_ context.Context) []Connector {
	s.mu.Lock()
	index, err := s.readIndex()
	s.mu.Unlock()
	if err != nil {
		return nil
	}
	var out []Connector
	for _, c := range catalogue {
		if _, ok := index[c.ID]; ok {
			out = append(out, c)
		}
	}
	return out
}

// Connect saves the values for a connection and proves them with the
// connector's test call. A failed check never destroys a working connection:
// the previous values go back.
func (s *Service) Connect(ctx context.Context, id string, values map[string]string) (Result, error) {
	c, ok := Lookup(id)
	if !ok {
		return Result{}, ErrUnknownConnector
	}
	if !s.Available() {
		return Result{}, ErrStoreUnavailable
	}
	s.mu.Lock()
	if s.inFlight[id] {
		s.mu.Unlock()
		return Result{}, ErrBusy
	}
	s.inFlight[id] = true
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.inFlight, id)
		s.mu.Unlock()
	}()

	previous, err := s.credentials(ctx, c)
	if err != nil && !errors.Is(err, ErrSecretNotFound) {
		return Result{}, err
	}
	clean, err := cleanValues(c, values, previous)
	if err != nil {
		return Result{}, err
	}
	if err := s.save(ctx, c, clean); err != nil {
		return Result{}, err
	}
	result := s.test(ctx, c, clean)
	if result.OK {
		return result, nil
	}
	if previous != nil {
		if err := s.save(ctx, c, previous); err != nil {
			return result, fmt.Errorf("the new key failed its check, and Swarm could not put the old key back: %w", err)
		}
	} else if err := s.remove(ctx, c.ID); err != nil {
		return result, fmt.Errorf("the key failed its check, and Swarm could not remove it: %w", err)
	}
	return result, nil
}

// Test proves the saved key again.
func (s *Service) Test(ctx context.Context, id string) (Result, error) {
	c, ok := Lookup(id)
	if !ok {
		return Result{}, ErrUnknownConnector
	}
	values, err := s.credentials(ctx, c)
	if errors.Is(err, ErrSecretNotFound) {
		return Result{OK: false, Detail: c.Name + " is not connected"}, nil
	}
	if err != nil {
		return Result{}, err
	}
	return s.test(ctx, c, values), nil
}

// Disconnect removes the key of a connection.
func (s *Service) Disconnect(ctx context.Context, id string) error {
	if _, ok := Lookup(id); !ok {
		return ErrUnknownConnector
	}
	return s.remove(ctx, id)
}

// cleanValues checks the values against the connector's fields. A blank
// secret keeps the saved one, so the user can change the host or the region
// without a new paste of the key.
func cleanValues(c Connector, values, previous map[string]string) (map[string]string, error) {
	known := map[string]bool{}
	for _, f := range c.Fields {
		known[f.Key] = true
	}
	for key := range values {
		if !known[key] {
			return nil, ValidationError{fmt.Sprintf("%s has no field %q", c.Name, key)}
		}
	}
	clean := map[string]string{}
	for _, f := range c.Fields {
		v := strings.TrimSpace(values[f.Key])
		if v == "" && f.Secret && previous != nil {
			v = previous[f.Key]
		}
		if strings.ContainsAny(v, "\r\n") {
			return nil, ValidationError{f.Label + " must be one line"}
		}
		if v == "" {
			if !f.Optional && f.Secret {
				return nil, ValidationError{f.Label + " is required"}
			}
			continue
		}
		clean[f.Key] = v
	}
	if c.BaseFor != nil {
		if _, err := c.BaseFor(clean); err != nil {
			return nil, ValidationError{err.Error()}
		}
	}
	return clean, nil
}

func (s *Service) save(ctx context.Context, c Connector, values map[string]string) error {
	if err := s.store.Put(ctx, c.ID, values); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cache[c.ID] = values
	index, err := s.readIndex()
	if err != nil {
		return err
	}
	entry := indexEntry{ConnectedAt: time.Now().UTC(), Values: map[string]string{}}
	for _, f := range c.Fields {
		v := values[f.Key]
		switch {
		case v == "":
		case f.Secret:
			if entry.Tail == "" && len(v) > 8 {
				entry.Tail = v[len(v)-4:]
			}
		default:
			entry.Values[f.Key] = v
		}
	}
	if len(entry.Values) == 0 {
		entry.Values = nil
	}
	index[c.ID] = entry
	return s.writeIndex(index)
}

func (s *Service) remove(ctx context.Context, id string) error {
	if err := s.store.Delete(ctx, id); err != nil && !errors.Is(err, ErrSecretNotFound) {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.cache, id)
	index, err := s.readIndex()
	if err != nil {
		return err
	}
	if _, ok := index[id]; !ok {
		return nil
	}
	delete(index, id)
	return s.writeIndex(index)
}

// credentials returns the saved values of a connection, from the cache when
// it can.
func (s *Service) credentials(ctx context.Context, c Connector) (map[string]string, error) {
	s.mu.Lock()
	cached, ok := s.cache[c.ID]
	s.mu.Unlock()
	if ok {
		return cached, nil
	}
	if !s.Available() {
		return nil, ErrSecretNotFound
	}
	values, err := s.store.Get(ctx, c.ID)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	s.cache[c.ID] = values
	s.mu.Unlock()
	return values, nil
}

func (s *Service) readIndex() (map[string]indexEntry, error) {
	raw, err := os.ReadFile(s.indexPath)
	if errors.Is(err, os.ErrNotExist) {
		return map[string]indexEntry{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", s.indexPath, err)
	}
	index := map[string]indexEntry{}
	if err := json.Unmarshal(raw, &index); err != nil {
		return nil, fmt.Errorf("read %s: %w", s.indexPath, err)
	}
	return index, nil
}

func (s *Service) writeIndex(index map[string]indexEntry) error {
	raw, err := json.MarshalIndent(index, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.indexPath), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(s.indexPath), ".connections-*.json")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if _, err := tmp.Write(append(raw, '\n')); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), s.indexPath)
}

// AgentTool is one tool for the `ao mcp` server.
type AgentTool struct {
	Name        string
	Description string
	InputSchema map[string]any
}

// AgentTools returns one tool for each connected service.
func (s *Service) AgentTools(ctx context.Context) []AgentTool {
	connected := s.Connected(ctx)
	tools := make([]AgentTool, 0, len(connected))
	for _, c := range connected {
		tools = append(tools, AgentTool{Name: c.Tool, Description: toolDescription(c), InputSchema: toolSchema(c)})
	}
	return tools
}

func toolDescription(c Connector) string {
	text := c.Guide + "\n\n" +
		"Swarm keeps the credential in the macOS keychain and attaches it itself; it never enters your context, so never " +
		"ask the user for it and never look for it in the environment. Host and base path are added for you; pass " +
		"only the path. Allowed methods: " + strings.Join(c.Methods, ", ") + "."
	if c.BodyType == BodyForm {
		text += " The body is FORM-ENCODED, not JSON."
	}
	return text
}

func toolSchema(c Connector) map[string]any {
	bodyHint := "Request body as a JSON string."
	if c.BodyType == BodyForm {
		bodyHint = "Request body as a form-encoded string, e.g. 'name=value&other=value'."
	}
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"path": map[string]any{
				"type": "string", "minLength": 1,
				"description": "API path after the base URL, with any query string, beginning with '/'.",
			},
			"method": map[string]any{
				"type": "string", "enum": c.Methods, "description": "HTTP method. Defaults to GET.",
			},
			"body": map[string]any{"type": "string", "description": bodyHint},
			"fields": map[string]any{
				"type": "string",
				"description": "Comma-separated field names (dot paths allowed) to keep from each row of a list response. " +
					"Use it to keep a large list small.",
			},
		},
		"required": []string{"path"},
	}
}

// PromptBlock is the standing-instruction block that names the connected
// services. Without it an agent does not know that a service is there until it
// reads the tool list, and it looks for a key in the environment instead.
func (s *Service) PromptBlock(ctx context.Context) string {
	connected := s.Connected(ctx)
	if len(connected) == 0 {
		return ""
	}
	rows := make([]string, 0, len(connected))
	for _, c := range connected {
		rows = append(rows, fmt.Sprintf("- %s -> %s  (%s)", c.Name, c.Tool, c.Blurb))
	}
	sort.Strings(rows)
	return "## Connected services\n\n" +
		"The user has connected these accounts in Swarm. Each one is a tool on the ao tool server that you can call now. " +
		"Swarm attaches the credential itself, so it never appears in your context. Never ask for a key and never look " +
		"for one in the environment.\n" +
		strings.Join(rows, "\n") + "\n" +
		"Read the tool's own description before your first call: it has the paths, the auth quirks and the write rules " +
		"for that service. Nothing else is connected. If a task needs a service that is not on this list, say so plainly."
}
