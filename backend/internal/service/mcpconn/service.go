package mcpconn

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/service/connections"
)

const (
	gone             = "This connection does not exist any more."
	saveFailed       = "Swarm could not save the change."
	urlChanged       = "The address of this connection changed. Check the key again."
	brokenProjects   = "Swarm cannot read the connector projects file. Check the file, then try again."
	activeConnection = "This connection is active. Use Refresh tools or Replace key."
)

// ownText matches a store message that Swarm wrote itself, which is safe to show.
var ownText = regexp.MustCompile(`^(Swarm holds|Swarm cannot read|The MCP connections file|A connection with that name|GitHub stays)`)

func ownerText(err error) string {
	if err != nil && ownText.MatchString(err.Error()) {
		return err.Error()
	}
	return saveFailed
}

// Deps are the service's links to the rest of the daemon.
type Deps struct {
	// Projects lists the ids of the registered Swarm projects.
	Projects func(ctx context.Context) []string
	// ProjectOf finds the project of a session; "" when it has none.
	ProjectOf func(ctx context.Context, sessionID string) string
	// RedirectURI is the OAuth redirect URI on the daemon's loopback listener.
	RedirectURI func() string
	// OtherSecrets are the key-based connector secrets, so that a remote
	// result cannot echo them either.
	OtherSecrets func(ctx context.Context) []string
	Logger       *slog.Logger
	HTTP         HTTP
	Now          func() time.Time
}

// Service holds the MCP connections and runs the agents' calls.
type Service struct {
	store  *Store
	client *mcpClient
	oauth  *oauthDeps
	keeper *tokenKeeper
	signIn *signIn
	deps   Deps
	now    func() time.Time
}

// New returns the service. The vault keeps the store's seal key.
func New(dataDir string, vault connections.SecretStore, deps Deps) *Service {
	if deps.HTTP == nil {
		deps.HTTP = NewGuardedHTTP(ProductionPolicy)
	}
	if deps.Now == nil {
		deps.Now = time.Now
	}
	if deps.Logger == nil {
		deps.Logger = slog.Default()
	}
	if deps.Projects == nil {
		deps.Projects = func(context.Context) []string { return nil }
	}
	if deps.RedirectURI == nil {
		deps.RedirectURI = func() string { return "" }
	}
	store := NewStore(dataDir, vault)
	store.now = deps.Now
	s := &Service{store: store, client: &mcpClient{http: deps.HTTP}, oauth: &oauthDeps{http: deps.HTTP}, deps: deps, now: deps.Now}
	s.keeper = &tokenKeeper{store: store, oauth: s.oauth, now: deps.Now, log: s.log, leases: map[string]*lease{}}
	s.signIn = &signIn{store: store, oauth: s.oauth, states: newOAuthStates(deps.Now), redirectURI: deps.RedirectURI, now: deps.Now}
	return s
}

// Store returns the store, for the connector project grants.
func (s *Service) Store() *Store { return s.store }

// Available reports whether this build can keep MCP connections.
func (s *Service) Available() bool { return s.store.Available() }

func (s *Service) log(line string) { s.deps.Logger.Info(line) }

func (s *Service) otherSecrets() []string {
	if s.deps.OtherSecrets == nil {
		return nil
	}
	return s.deps.OtherSecrets(context.Background())
}

// OAuthView is what the owner sees of a sign-in.
type OAuthView struct {
	Issuer       string   `json:"issuer"`
	ClientID     string   `json:"clientId"`
	Scopes       []string `json:"scopes"`
	Status       string   `json:"status"`
	ManualClient bool     `json:"manualClient"`
}

// ConnectionView is what the settings page may see: secret names, never a value.
type ConnectionView struct {
	ID             string            `json:"id"`
	Name           string            `json:"name"`
	AppKey         string            `json:"appKey,omitempty"`
	URL            string            `json:"url"`
	URLHasSecret   bool              `json:"urlHasSecret"`
	AuthKind       string            `json:"authKind"`
	HeaderNames    []string          `json:"headerNames"`
	Secrets        map[string]string `json:"secrets"`
	OAuth          *OAuthView        `json:"oauth,omitempty"`
	Tools          []Tool            `json:"tools"`
	ToolsFetchedAt *int64            `json:"toolsFetchedAt,omitempty"`
	Health         Health            `json:"health"`
	LastError      string            `json:"lastError,omitempty"`
	Projects       *Projects         `json:"projects"`
	CreatedAt      int64             `json:"createdAt"`
	UpdatedAt      int64             `json:"updatedAt"`
}

// viewOf removes each secret from the remote tool text too.
func viewOf(row Connection, secrets []string) ConnectionView {
	if secrets == nil {
		secrets = secretValuesOf([]Connection{row})
	}
	names := map[string]string{}
	for k := range row.Secrets {
		names[k] = "set"
	}
	headerNames := append([]string{}, row.HeaderNames...)
	v := ConnectionView{ID: row.ID, Name: row.Name, AppKey: row.AppKey, URL: row.URL, URLHasSecret: row.Secrets[secretURL] != "",
		AuthKind: row.AuthKind, HeaderNames: headerNames, Secrets: names, Tools: scrubTools(row.Tools, secrets), ToolsFetchedAt: row.ToolsFetchedAt,
		Health: row.Health, LastError: row.LastError, Projects: row.Projects, CreatedAt: row.CreatedAt, UpdatedAt: row.UpdatedAt}
	if row.OAuth != nil {
		v.OAuth = &OAuthView{Issuer: row.OAuth.Issuer, ClientID: row.OAuth.ClientID, Scopes: append([]string{}, row.OAuth.Scopes...), Status: row.OAuth.Status, ManualClient: row.OAuth.ManualClient}
	}
	return v
}

// Overview is the settings page's read.
type Overview struct {
	Available   bool             `json:"available"`
	Connections []ConnectionView `json:"connections"`
	Store       StoreStatus      `json:"store"`
	Apps        []App            `json:"apps"`
	Projects    []string         `json:"projects"`
}

// Overview lists the connections with the store state.
func (s *Service) Overview(ctx context.Context) Overview {
	out := Overview{Available: s.Available(), Connections: []ConnectionView{}, Apps: Apps(), Projects: s.projects(ctx)}
	if !out.Available {
		return out
	}
	out.Store = s.store.Status(ctx)
	rows := s.store.List(ctx)
	secrets := secretValuesOf(rows)
	for _, r := range rows {
		out.Connections = append(out.Connections, viewOf(r, secrets))
	}
	return out
}

func (s *Service) projects(ctx context.Context) []string {
	list := s.deps.Projects(ctx)
	sort.Strings(list)
	if list == nil {
		list = []string{}
	}
	return list
}

// CheckResult is the answer of Check link, Save key and Refresh tools.
type CheckResult struct {
	OK      bool   `json:"ok"`
	ID      string `json:"id,omitempty"`
	Outcome string `json:"outcome"`
	// Field is the field that the owner must change: url, key or header.
	Field      string          `json:"field,omitempty"`
	Error      string          `json:"error,omitempty"`
	Connection *ConnectionView `json:"connection,omitempty"`
}

func viewPtr(row Connection) *ConnectionView {
	v := viewOf(row, nil)
	return &v
}

// CheckInput is Check link.
type CheckInput struct {
	URL    string `json:"url,omitempty"`
	Name   string `json:"name,omitempty"`
	AppKey string `json:"appKey,omitempty"`
	ID     string `json:"id,omitempty"`
}

// Check is Check link. A URL that fails the guard makes no row.
func (s *Service) Check(ctx context.Context, in CheckInput) CheckResult {
	if !s.Available() {
		return CheckResult{Outcome: "error", Error: connections.ErrStoreUnavailable.Error()}
	}
	var existing *Connection
	if in.ID != "" {
		row, ok := s.store.Get(ctx, in.ID)
		if !ok {
			return CheckResult{Outcome: "error", Error: gone}
		}
		if row.Projects != nil {
			return CheckResult{ID: row.ID, Outcome: "error", Error: activeConnection}
		}
		existing = &row
	}
	existingID := ""
	if existing != nil {
		existingID = existing.ID
	}
	typed := strings.TrimSpace(in.URL)
	target := typed
	if target == "" && existing != nil {
		target = targetOf(*existing, nil).URL
	}
	if _, err := CheckURL(target); err != nil {
		s.log(fmt.Sprintf("[mcp] %s check_%s", idOrDash(existingID), failureCode(err)))
		return CheckResult{ID: existingID, Outcome: "error", Field: "url", Error: guardSentence(err)}
	}
	outcome := s.probe(ctx, Target{URL: target, Headers: map[string]string{}})
	if outcome.Kind == probeError {
		s.log(fmt.Sprintf("[mcp] %s check_%s", idOrDash(existingID), outcome.Code))
		return CheckResult{ID: existingID, Outcome: "error", Field: outcome.Field, Error: outcome.Message}
	}
	apply := func(r *Connection) {
		r.ResourceMetadataURL, r.ChallengeScopes = "", nil
		if outcome.Kind == probeOAuth {
			r.ResourceMetadataURL, r.ChallengeScopes = outcome.ResourceMetadataURL, outcome.Scopes
		}
		switch outcome.Kind {
		case probeOpen:
			r.AuthKind = AuthNone
			r.Tools = outcome.Tools
			at := s.now().UnixMilli()
			r.ToolsFetchedAt = &at
			r.Health = HealthDraft
		case probeNeedsKey:
			if r.AuthKind == AuthNone {
				r.AuthKind = AuthHeaders
			}
		default:
			hasHeader := false
			for k := range r.Secrets {
				if strings.HasPrefix(k, headerSecretPrefix) {
					hasHeader = true
				}
			}
			if !hasHeader {
				r.AuthKind = AuthOAuth
			}
		}
	}
	var saved Connection
	var found bool
	var err error
	if existing != nil {
		// One write: a new address and its outcome land together.
		saved, found, err = s.store.Update(ctx, existing.ID, func(r *Connection) {
			if typed != "" && typed != targetOf(*r, nil).URL {
				public, hasSecret := publicURL(typed)
				r.URL = public
				if hasSecret {
					r.Secrets[secretURL] = typed
				} else {
					delete(r.Secrets, secretURL)
				}
				// Tokens and keys for one resource never go to another.
				for k := range r.Secrets {
					if strings.HasPrefix(k, "oauth.") || strings.HasPrefix(k, headerSecretPrefix) {
						delete(r.Secrets, k)
					}
				}
				r.OAuth, r.Tools, r.ToolsFetchedAt = nil, nil, nil
				r.Health, r.AuthKind = HealthDraft, AuthNone
			}
			apply(r)
		})
	} else {
		app := appByKey(in.AppKey)
		name := strings.TrimSpace(in.Name)
		appKey := ""
		if app != nil {
			appKey = app.Key
			if name == "" {
				name = app.Name
			}
		}
		if name == "" {
			if u, perr := url.Parse(target); perr == nil {
				name = u.Hostname()
			}
		}
		var created Connection
		if created, err = s.store.Create(ctx, name, target, appKey); err == nil {
			saved, found, err = s.store.Update(ctx, created.ID, apply)
		}
	}
	if err != nil {
		s.log(fmt.Sprintf("[mcp] %s check_store", idOrDash(existingID)))
		return CheckResult{ID: existingID, Outcome: "error", Field: "url", Error: ownerText(err)}
	}
	if !found {
		return CheckResult{ID: existingID, Outcome: "error", Error: gone}
	}
	s.log(fmt.Sprintf("[mcp] %s check_%s", saved.ID, outcome.Kind))
	kind := string(outcome.Kind)
	if outcome.Kind == probeOAuth && outcome.ManualClient {
		kind = "manual_client"
	}
	return CheckResult{OK: true, ID: saved.ID, Outcome: kind, Connection: viewPtr(saved)}
}

func idOrDash(id string) string {
	if IsConnectionID(id) {
		return id
	}
	return "-"
}

// Header is one custom header.
type Header struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// CredentialsInput is a key (sent as Authorization: Bearer <key>) or headers.
type CredentialsInput struct {
	Key     string   `json:"key,omitempty"`
	Headers []Header `json:"headers,omitempty"`
}

// SaveCredentials saves a key or headers. Nothing is written until the server
// takes them, so a refused Replace key keeps the working key.
func (s *Service) SaveCredentials(ctx context.Context, id string, in CredentialsInput) CheckResult {
	row, ok := s.store.Get(ctx, id)
	if !ok {
		return CheckResult{Outcome: "error", Error: gone}
	}
	fail := func(field, message string) CheckResult {
		return CheckResult{ID: id, Outcome: "error", Field: field, Error: message, Connection: viewPtr(row)}
	}
	secrets := map[string]string{}
	var names []string
	if key := strings.TrimSpace(in.Key); key != "" {
		if bad := checkHeaderValue("Authorization", key); bad != "" {
			return fail("key", bad)
		}
		secrets[headerSecretPrefix+"Authorization"] = "Bearer " + key
		names = append(names, "Authorization")
	}
	if len(in.Headers) > maxHeaders {
		return fail("header", fmt.Sprintf("Use %d headers or fewer.", maxHeaders))
	}
	for _, h := range in.Headers {
		name := strings.TrimSpace(h.Name)
		if bad := checkHeaderName(name); bad != "" {
			return fail("header", bad)
		}
		for _, n := range names {
			if strings.EqualFold(n, name) {
				return fail("header", fmt.Sprintf("The %q header is set twice.", name))
			}
		}
		if h.Value == "" {
			return fail("header", fmt.Sprintf("Type a value for %q.", name))
		}
		if bad := checkHeaderValue(name, h.Value); bad != "" {
			return fail("header", bad)
		}
		secrets[headerSecretPrefix+name] = h.Value
		names = append(names, name)
	}
	if len(names) == 0 {
		return fail("key", "Type a key or at least one header.")
	}
	kept := map[string]string{}
	if u := row.Secrets[secretURL]; u != "" {
		kept[secretURL] = u
	}
	for k, v := range secrets {
		kept[k] = v
	}
	probeRow := row
	probeRow.AuthKind, probeRow.HeaderNames = AuthHeaders, names
	probed := targetOf(probeRow, kept)
	outcome := s.probe(ctx, probed)
	if outcome.Kind != probeOpen {
		s.log(fmt.Sprintf("[mcp] %s credentials_refused", id))
		if outcome.Kind == probeError {
			return fail("key", outcome.Message)
		}
		return fail("key", "The server did not accept this key or these headers.")
	}
	moved := false
	saved, found, err := s.store.Update(ctx, id, func(r *Connection) {
		// Another tab may have changed the address during the probe.
		if targetOf(*r, nil).URL != probed.URL {
			moved = true
			return
		}
		for k := range r.Secrets {
			if strings.HasPrefix(k, headerSecretPrefix) || strings.HasPrefix(k, "oauth.") {
				delete(r.Secrets, k)
			}
		}
		for k, v := range secrets {
			r.Secrets[k] = v
		}
		r.AuthKind, r.HeaderNames, r.OAuth = AuthHeaders, names, nil
		r.Tools = outcome.Tools
		at := s.now().UnixMilli()
		r.ToolsFetchedAt = &at
		r.Health = HealthOK
		if r.Projects == nil {
			r.Health = HealthDraft
		}
		r.LastError = ""
	})
	if moved {
		return fail("key", urlChanged)
	}
	if err != nil || !found {
		return fail("key", "Swarm could not save the key.")
	}
	s.log(fmt.Sprintf("[mcp] %s credentials_saved", id))
	return CheckResult{OK: true, ID: id, Outcome: "open", Connection: viewPtr(saved)}
}

// OAuthClientInput is a client id that the owner typed.
type OAuthClientInput struct {
	ClientID     string `json:"clientId"`
	ClientSecret string `json:"clientSecret,omitempty"`
}

// StartResult is the answer of Sign in.
type StartResult struct {
	OK           bool   `json:"ok"`
	URL          string `json:"url,omitempty"`
	Error        string `json:"error,omitempty"`
	ManualClient bool   `json:"manualClient,omitempty"`
}

// StartOAuth gives the authorization URL that the owner opens in a browser.
func (s *Service) StartOAuth(ctx context.Context, id string, client *OAuthClientInput) StartResult {
	if client != nil && len(client.ClientSecret) > maxClientInput {
		return StartResult{Error: "The client secret is too long."}
	}
	var manual *clientCreds
	if client != nil && strings.TrimSpace(client.ClientID) != "" {
		manual = &clientCreds{ClientID: client.ClientID, ClientSecret: client.ClientSecret}
	}
	row, ok := s.store.Get(ctx, id)
	hint := ""
	var scopes []string
	if ok {
		hint = row.ResourceMetadataURL
		if hint == "" && row.OAuth != nil {
			hint = row.OAuth.ResourceMetadataURL
		}
		scopes = row.ChallengeScopes
	}
	authURL, err := s.signIn.start(ctx, id, manual, hint, scopes)
	if err != nil {
		var failure *OAuthFailure
		var guard *GuardError
		switch {
		case errors.As(err, &failure):
			s.log(fmt.Sprintf("[mcp] %s %s", idOrDash(id), failure.Code))
			return StartResult{Error: failure.Message, ManualClient: failure.Code == "manual_client_required"}
		case errors.As(err, &guard):
			s.log(fmt.Sprintf("[mcp] %s %s", idOrDash(id), failureCode(err)))
			return StartResult{Error: guardSentence(err)}
		default:
			s.log(fmt.Sprintf("[mcp] %s error", idOrDash(id)))
			return StartResult{Error: "Swarm could not start the sign-in."}
		}
	}
	return StartResult{OK: true, URL: authURL}
}

// FinishResult is the outcome of the OAuth callback.
type FinishResult struct {
	OK      bool
	ID      string
	Name    string
	Message string
}

// FinishOAuth handles the browser's redirect to the callback.
func (s *Service) FinishOAuth(ctx context.Context, query url.Values) FinishResult {
	row, err := s.signIn.finish(ctx, query)
	if err != nil {
		var failure *OAuthFailure
		if errors.As(err, &failure) {
			s.log(fmt.Sprintf("[mcp] %s %s", idOrDash(failure.ConnectionID), failure.Code))
			return FinishResult{ID: failure.ConnectionID, Message: failure.Message}
		}
		s.log("[mcp] - " + failureCode(err))
		return FinishResult{Message: "Swarm could not finish the sign-in."}
	}
	s.fetchTools(ctx, row.ID)
	s.log(fmt.Sprintf("[mcp] %s signed_in", row.ID))
	return FinishResult{OK: true, ID: row.ID, Name: row.Name}
}

// fetchTools reads the tool list again. answered is true when the server
// answered with a JSON-RPC error: it is reachable.
func (s *Service) fetchTools(ctx context.Context, id string) (Connection, bool, bool) {
	row, ok := s.store.Get(ctx, id)
	if !ok {
		return row, false, false
	}
	callCtx, cancel := context.WithTimeout(ctx, probeLimit)
	tools, err := s.client.listTools(callCtx, targetOf(row, nil))
	cancel()
	if err != nil {
		code := failureCode(err)
		s.log(fmt.Sprintf("[mcp] %s %s", id, code))
		answered := code == string(MCPRPCError)
		saved, found, uerr := s.store.Update(ctx, id, func(r *Connection) {
			if r.Projects == nil {
				return
			}
			if !answered || r.Health == HealthUnreachable {
				r.Health = HealthUnreachable
				if answered {
					r.Health = HealthOK
				}
			}
			r.LastError = code
		})
		if uerr != nil || !found {
			return row, false, answered
		}
		return saved, false, answered
	}
	saved, found, uerr := s.store.Update(ctx, id, func(r *Connection) {
		r.Tools = tools
		at := s.now().UnixMilli()
		r.ToolsFetchedAt = &at
		r.Health = HealthOK
		if r.Projects == nil {
			r.Health = HealthDraft
		}
		r.LastError = ""
	})
	if uerr != nil || !found {
		return row, false, false
	}
	return saved, true, false
}

// RefreshTools reads the tool list of a connection again.
func (s *Service) RefreshTools(ctx context.Context, id string) CheckResult {
	if _, ok := s.store.Get(ctx, id); !ok {
		return CheckResult{Outcome: "error", Error: gone}
	}
	fresh, ok := s.keeper.fresh(ctx, id, false)
	if ok && fresh.AuthKind == AuthOAuth && fresh.OAuth != nil && fresh.OAuth.Status == oauthStatusSignIn {
		return CheckResult{ID: id, Outcome: "error", Error: "Sign in again first.", Connection: viewPtr(fresh)}
	}
	row, fetched, answered := s.fetchTools(ctx, id)
	if fetched {
		return CheckResult{OK: true, ID: id, Outcome: "open", Connection: viewPtr(row)}
	}
	message := "The server did not answer. The old tool list stays."
	if answered {
		message = "The server answered with an error. The old tool list stays."
	}
	out := CheckResult{ID: id, Outcome: "error", Error: message}
	if row.ID != "" {
		out.Connection = viewPtr(row)
	}
	return out
}

// ProjectsResult is the answer of "Which projects can use this?".
type ProjectsResult struct {
	OK         bool            `json:"ok"`
	Error      string          `json:"error,omitempty"`
	Connection *ConnectionView `json:"connection,omitempty"`
}

// SetProjects answers "Which projects can use this?". The connection becomes
// active here.
func (s *Service) SetProjects(ctx context.Context, id string, projects Projects) ProjectsResult {
	row, ok := s.store.Get(ctx, id)
	if !ok {
		return ProjectsResult{Error: gone}
	}
	switch row.AuthKind {
	case AuthHeaders:
		if len(row.HeaderNames) == 0 {
			return ProjectsResult{Error: "Save a key or headers first."}
		}
		for _, n := range row.HeaderNames {
			if row.Secrets[headerSecretPrefix+n] == "" {
				return ProjectsResult{Error: "Save a key or headers first."}
			}
		}
	case AuthNone:
		if row.Tools == nil {
			return ProjectsResult{Error: "Check the link first."}
		}
	case AuthOAuth:
		if row.Secrets[secretAccessToken] == "" {
			return ProjectsResult{Error: "Sign in first."}
		}
	}
	if !projects.All && len(projects.List) == 0 {
		return ProjectsResult{Error: "Choose All projects or at least one project."}
	}
	clean, err := cleanProjects(projects, s.projects(ctx))
	if err != nil {
		return ProjectsResult{Error: err.Error()}
	}
	saved, found, err := s.store.Update(ctx, id, func(r *Connection) {
		r.Projects = &clean
		if r.Health == HealthDraft {
			r.Health = HealthUnreachable
			if r.Tools != nil {
				r.Health = HealthOK
			}
		}
	})
	if err != nil || !found {
		return ProjectsResult{Error: "Swarm could not save the projects."}
	}
	s.log(fmt.Sprintf("[mcp] %s projects_saved", id))
	return ProjectsResult{OK: true, Connection: viewPtr(saved)}
}

// Rename changes the name that agents use.
func (s *Service) Rename(ctx context.Context, id, name string) ProjectsResult {
	if cleanName(name) == "" {
		return ProjectsResult{Error: "Type a name."}
	}
	saved, found, err := s.store.Update(ctx, id, func(r *Connection) { r.Name = name })
	if err != nil {
		return ProjectsResult{Error: ownerText(err)}
	}
	if !found {
		return ProjectsResult{Error: gone}
	}
	return ProjectsResult{OK: true, Connection: viewPtr(saved)}
}

// Remove deletes a connection.
func (s *Service) Remove(id string) (bool, string) {
	if !IsConnectionID(id) {
		return false, gone
	}
	ok, err := s.store.Remove(id)
	if err != nil {
		return false, BrokenStoreText
	}
	if !ok {
		return false, gone
	}
	s.log(fmt.Sprintf("[mcp] %s removed", id))
	return true, ""
}

// PasteResult is the answer of Paste a config.
type PasteResult struct {
	OK      bool             `json:"ok"`
	Error   string           `json:"error,omitempty"`
	Drafts  []ConnectionView `json:"drafts"`
	Refused []Refusal        `json:"refused"`
}

// Paste makes a draft for each url entry with its header names. No pasted
// value is kept.
func (s *Service) Paste(ctx context.Context, text string) PasteResult {
	entries, refused, perr := parsePastedConfig(text)
	out := PasteResult{Drafts: []ConnectionView{}, Refused: append([]Refusal{}, refused...)}
	if perr != "" {
		out.Error = perr
		return out
	}
	for _, e := range entries {
		if _, err := CheckURL(e.URL); err != nil {
			out.Refused = append(out.Refused, Refusal{Name: e.Name, Error: guardSentence(err)})
			continue
		}
		row, err := s.store.Create(ctx, e.Name, e.URL, "")
		if err != nil {
			out.Refused = append(out.Refused, Refusal{Name: e.Name, Error: ownerText(err)})
			continue
		}
		saved, _, err := s.store.Update(ctx, row.ID, func(r *Connection) {
			r.HeaderNames = append([]string{}, e.HeaderNames...)
			r.AuthKind = AuthNone
			if len(e.HeaderNames) > 0 {
				r.AuthKind = AuthHeaders
			}
		})
		if err != nil {
			out.Refused = append(out.Refused, Refusal{Name: e.Name, Error: ownerText(err)})
			continue
		}
		out.Drafts = append(out.Drafts, viewOf(saved, nil))
	}
	out.OK = len(out.Drafts) > 0
	return out
}

// ConnectorProjects returns the project setting of every key-based connector
// except GitHub, which stays available to every project.
func (s *Service) ConnectorProjects() (map[string]Projects, error) {
	if _, err := s.store.ConnectorProjects(); err != nil {
		return nil, ownerMessage(brokenProjects)
	}
	out := map[string]Projects{}
	for _, c := range connections.Catalogue() {
		if c.ID != "github" {
			out[c.ID] = s.store.ConnectorAllowed(c.ID)
		}
	}
	return out, nil
}

// SetConnectorProjects saves the project setting of a key-based connector.
func (s *Service) SetConnectorProjects(ctx context.Context, service string, projects Projects) error {
	if _, ok := connections.Lookup(service); !ok {
		return connections.ErrUnknownConnector
	}
	if !projects.All && len(projects.List) == 0 {
		return connections.ValidationError{Message: "Choose All projects or at least one project."}
	}
	clean, err := cleanProjects(projects, s.projects(ctx))
	if err != nil {
		return connections.ValidationError{Message: err.Error()}
	}
	if err := s.store.SetConnectorProjects(service, clean); err != nil {
		if strings.HasPrefix(err.Error(), "GitHub stays") {
			return connections.ValidationError{Message: err.Error()}
		}
		if strings.Contains(err.Error(), "connector projects file") {
			return ownerMessage(brokenProjects)
		}
		return err
	}
	return nil
}

// ConnectorAllowed reports whether a project may use a key-based connector.
func (s *Service) ConnectorAllowed(service, project string) bool {
	p := s.store.ConnectorAllowed(service)
	return grantAllows(&p, project)
}

// PromptLines names the MCP connections of a project for the standing
// instructions. The last rule is there also for a project with no connection,
// because the owner can add one while the session runs (Swarm Online,
// 2026-11-06: an agent asked the owner for a key that an active connection held).
func (s *Service) PromptLines(ctx context.Context, project string) string {
	if !s.Available() {
		return ""
	}
	var rows []string
	for _, c := range s.Index(ctx, project).Connections {
		line := fmt.Sprintf("- %s: tools named %s__<tool> (%s)", c.Name, c.Prefix, c.Description)
		if c.Health == HealthNeedsSignIn {
			line += " - the owner must sign in again in Settings > Connections"
		}
		rows = append(rows, line)
	}
	named := ""
	if len(rows) > 0 {
		named = "These MCP connections are granted to this project. Their tools are in your tool list on the ao tool server. " +
			"Swarm adds the credential itself, so never ask for a key and never look for one in the environment.\n" +
			strings.Join(rows, "\n") + "\n"
	}
	return named + "This list is from the start of this session. Before you say that a service is not connected, or ask the owner for a key, " +
		"call connections_list. If a tool of a connection is not in your tool list, use connection_tools and connection_call."
}

// ProjectOfSession finds the project of a calling session. The project always
// comes from the daemon's own session record, never from the agent.
func (s *Service) ProjectOfSession(ctx context.Context, sessionID string) string {
	if s.deps.ProjectOf == nil || strings.TrimSpace(sessionID) == "" {
		return ""
	}
	return s.deps.ProjectOf(ctx, sessionID)
}

// IndexOrUnchanged is the tool index of a session's project, or only the
// version when the caller already has it.
func (s *Service) IndexOrUnchanged(ctx context.Context, sessionID, known string) (ToolIndex, bool) {
	index := s.Index(ctx, s.ProjectOfSession(ctx, sessionID))
	if known != "" && known == index.Version {
		return ToolIndex{Version: index.Version}, true
	}
	return index, false
}
