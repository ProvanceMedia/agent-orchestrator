package mcpconn

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/service/connections"
)

// The MCP connection rows. Each row, with its secrets, is sealed as a whole
// with AES-256-GCM. The key is in the macOS login keychain, so another program
// can read the file but not open it. A secret value leaves this package only
// inside the Target that the MCP client sends.

const (
	maxConnections = 100
	limitText      = "Swarm holds at most 100 MCP connections. Remove one first."
	unreadableText = "Swarm cannot read some saved connections. Check the keychain, then try again."
	// BrokenStoreText is the sentence for a connections file that Swarm cannot read.
	BrokenStoreText = "Swarm cannot read the saved connections file. Check the file, then try again."
	vaultAccount    = "mcp-vault"
)

// Projects is "all", or a list of project ids. Nil means that the add flow has
// not finished, and agents do not see the connection.
type Projects struct {
	All  bool     `json:"-"`
	List []string `json:"-"`
}

// JSONSchemaOneOf is read by the API spec generator: projects is "all" or a
// list of project ids. It is not used at runtime.
func (Projects) JSONSchemaOneOf() []interface{} {
	return []interface{}{"", []string{}}
}

// MarshalJSON writes "all" or the list.
func (p Projects) MarshalJSON() ([]byte, error) {
	if p.All {
		return json.Marshal("all")
	}
	list := p.List
	if list == nil {
		list = []string{}
	}
	return json.Marshal(list)
}

// UnmarshalJSON reads "all" or a list of strings.
func (p *Projects) UnmarshalJSON(raw []byte) error {
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		if s != "all" {
			return errors.New(`projects must be "all" or a list`)
		}
		*p = Projects{All: true}
		return nil
	}
	var list []string
	if err := json.Unmarshal(raw, &list); err != nil {
		return errors.New(`projects must be "all" or a list`)
	}
	*p = Projects{List: list}
	return nil
}

// Health is the state of a connection.
type Health string

// The health values.
const (
	HealthOK              Health = "ok"
	HealthUnreachable     Health = "unreachable"
	HealthNeedsSignIn     Health = "needs_sign_in"
	HealthDraft           Health = "draft"
	AuthNone                     = "none"
	AuthHeaders                  = "headers"
	AuthOAuth                    = "oauth"
	oauthStatusOK                = "ok"
	oauthStatusSignIn            = "needs_sign_in"
	secretURL                    = "url"
	secretAccessToken            = "oauth.access_token"  //nolint:gosec // A key name, not a credential.
	secretRefreshToken           = "oauth.refresh_token" //nolint:gosec // A key name, not a credential.
	secretClientSecret           = "oauth.client_secret" //nolint:gosec // A key name, not a credential.
	headerSecretPrefix           = "header."
	clientAuthNone               = "none"
	clientAuthSecretBasic        = "client_secret_basic"
	clientAuthSecretPost         = "client_secret_post"
)

// OAuthConfig is the sign-in binding of one connection.
type OAuthConfig struct {
	Issuer                string   `json:"issuer"`
	AuthorizationEndpoint string   `json:"authorizationEndpoint"`
	TokenEndpoint         string   `json:"tokenEndpoint"`
	RegistrationEndpoint  string   `json:"registrationEndpoint,omitempty"`
	ClientID              string   `json:"clientId"`
	ClientAuth            string   `json:"clientAuth"`
	Scopes                []string `json:"scopes"`
	Resource              string   `json:"resource"`
	AccessTokenExpiresAt  *int64   `json:"accessTokenExpiresAt,omitempty"`
	Status                string   `json:"status"`
	// RedirectURI is the redirect URI that the client was registered or typed
	// for. A change needs a new client.
	RedirectURI string `json:"redirectUri"`
	// ManualClient is true when the owner typed the client id. Swarm never
	// registers such a client again by itself.
	ManualClient        bool   `json:"manualClient"`
	ResourceMetadataURL string `json:"resourceMetadataUrl,omitempty"`
}

// Connection is one stored row, with its secrets.
type Connection struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	AppKey string `json:"appKey,omitempty"`
	// URL is the public form: a query value shows as ••••. The real URL is Secrets["url"].
	URL         string       `json:"url"`
	AuthKind    string       `json:"authKind"`
	HeaderNames []string     `json:"headerNames"`
	OAuth       *OAuthConfig `json:"oauth,omitempty"`
	// Secrets: header.<Name>, url, oauth.access_token, oauth.refresh_token, oauth.client_secret.
	Secrets             map[string]string `json:"secrets"`
	Tools               []Tool            `json:"tools,omitempty"`
	ToolsFetchedAt      *int64            `json:"toolsFetchedAt,omitempty"`
	Health              Health            `json:"health"`
	LastError           string            `json:"lastError,omitempty"`
	ResourceMetadataURL string            `json:"resourceMetadataUrl,omitempty"`
	ChallengeScopes     []string          `json:"challengeScopes,omitempty"`
	Projects            *Projects         `json:"projects"`
	CreatedAt           int64             `json:"createdAt"`
	UpdatedAt           int64             `json:"updatedAt"`
}

var connectionIDPattern = regexp.MustCompile(`^mcp_[0-9a-f]{12}$`)

// IsConnectionID is true for a connection id that Swarm made.
func IsConnectionID(id string) bool { return connectionIDPattern.MatchString(id) }

func newID() string {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	return "mcp_" + hex.EncodeToString(b)
}

// publicURL masks each query value. A URL with a query string can hold a key,
// so then the whole URL is a secret.
func publicURL(raw string) (string, bool) {
	u, err := url.Parse(raw)
	if err != nil {
		return raw, false
	}
	base := u.Scheme + "://" + u.Host + u.EscapedPath()
	hasSecret := u.RawQuery != "" || u.User != nil
	if u.RawQuery == "" {
		return base, hasSecret
	}
	var parts []string
	for _, part := range strings.Split(u.RawQuery, "&") {
		if part == "" {
			continue
		}
		// A query key with no value can be the secret itself, so it is hidden too.
		key, _, found := strings.Cut(part, "=")
		if !found {
			parts = append(parts, "••••")
			continue
		}
		decoded, err := url.QueryUnescape(key)
		if err != nil {
			decoded = key
		}
		parts = append(parts, url.QueryEscape(decoded)+"=••••")
	}
	return base + "?" + strings.Join(parts, "&"), hasSecret
}

// urlSecretParts are the secret parts of a secret URL.
func urlSecretParts(raw string) []string {
	u, err := url.Parse(raw)
	if err != nil {
		return nil
	}
	var parts []string
	for key, values := range u.Query() {
		for _, v := range values {
			if v == "" {
				parts = append(parts, key)
			} else {
				parts = append(parts, v)
			}
		}
	}
	if u.User != nil {
		parts = append(parts, u.User.Username())
		if p, ok := u.User.Password(); ok {
			parts = append(parts, p)
		}
	}
	return parts
}

var nameControl = regexp.MustCompile(`[\x00-\x1f\x7f]`)
var spaces = regexp.MustCompile(`\s+`)

func cleanName(raw string) string {
	s := strings.TrimSpace(spaces.ReplaceAllString(nameControl.ReplaceAllString(raw, " "), " "))
	return cut(s, 64)
}

func findByName(rows []Connection, name string) *Connection {
	n := strings.ToLower(cleanName(name))
	if n == "" {
		return nil
	}
	for i := range rows {
		if strings.EqualFold(rows[i].Name, n) {
			return &rows[i]
		}
	}
	return nil
}

// secretValuesOf is every stored secret value of these rows, for the scrubber.
func secretValuesOf(rows []Connection) []string {
	var out []string
	for _, r := range rows {
		for key, value := range r.Secrets {
			if value == "" {
				continue
			}
			out = append(out, value)
			if key == secretURL {
				out = append(out, urlSecretParts(value)...)
			}
		}
	}
	return out
}

// targetOf is the URL and headers of one request. secrets lets a probe try
// values before they are saved.
func targetOf(row Connection, secrets map[string]string) Target {
	if secrets == nil {
		secrets = row.Secrets
	}
	headers := map[string]string{}
	for _, name := range row.HeaderNames {
		if v, ok := secrets[headerSecretPrefix+name]; ok {
			headers[name] = v
		}
	}
	if token := secrets[secretAccessToken]; row.AuthKind == AuthOAuth && token != "" {
		headers["Authorization"] = "Bearer " + token
	}
	creds := make([]string, 0, len(headers))
	for name := range headers {
		creds = append(creds, name)
	}
	sort.Strings(creds)
	target := Target{URL: row.URL, Headers: headers, CredentialHeaders: creds}
	if secretTarget := secrets[secretURL]; secretTarget != "" {
		target.URL = secretTarget
	}
	return target
}

type sealedFile struct {
	Version int               `json:"version"`
	Rows    map[string]string `json:"rows"`
}

// StoreStatus says whether Swarm can read the saved connections.
type StoreStatus struct {
	Broken     bool `json:"broken"`
	Unreadable int  `json:"unreadable"`
}

// Store keeps the sealed rows and the connector project settings.
type Store struct {
	file         string
	projectsFile string
	vault        connections.SecretStore
	now          func() time.Time
	onChange     func()

	mu  sync.Mutex
	key []byte
}

// NewStore returns the store in dataDir. The vault keeps the seal key.
func NewStore(dataDir string, vault connections.SecretStore) *Store {
	return &Store{
		file:         filepath.Join(dataDir, "mcp-connections.json"),
		projectsFile: filepath.Join(dataDir, "connector-projects.json"),
		vault:        vault,
		now:          time.Now,
	}
}

// Available reports whether this build can seal rows.
func (s *Store) Available() bool { return s.vault != nil && s.vault.Available() }

// OnChange sets the one listener, told after each write that an agent can see.
func (s *Store) OnChange(listener func()) { s.onChange = listener }

func (s *Store) nowMillis() int64 { return s.now().UnixMilli() }

// sealKey reads the seal key from the keychain, or makes it on first use.
func (s *Store) sealKey(ctx context.Context) ([]byte, error) {
	if s.key != nil {
		return s.key, nil
	}
	if !s.Available() {
		return nil, connections.ErrStoreUnavailable
	}
	values, err := s.vault.Get(ctx, vaultAccount)
	if errors.Is(err, connections.ErrSecretNotFound) {
		key := make([]byte, 32)
		if _, err := rand.Read(key); err != nil {
			return nil, err
		}
		// A file sealed with a lost key must not get a new key silently.
		if _, statErr := os.Stat(s.file); statErr == nil {
			return nil, ownerMessage(unreadableText)
		}
		if err := s.vault.Put(ctx, vaultAccount, map[string]string{"key": base64.StdEncoding.EncodeToString(key)}); err != nil {
			return nil, err
		}
		s.key = key
		return key, nil
	}
	if err != nil {
		return nil, err
	}
	key, err := base64.StdEncoding.DecodeString(values["key"])
	if err != nil || len(key) != 32 {
		return nil, ownerMessage(unreadableText)
	}
	s.key = key
	return key, nil
}

func (s *Store) seal(key, plain []byte) (string, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(gcm.Seal(nonce, nonce, plain, nil)), nil
}

func (s *Store) open(key []byte, sealed string) ([]byte, error) {
	raw, err := base64.StdEncoding.DecodeString(sealed)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	if len(raw) < gcm.NonceSize() {
		return nil, errors.New("short")
	}
	return gcm.Open(nil, raw[:gcm.NonceSize()], raw[gcm.NonceSize():], nil)
}

// readSealed treats only a missing file as an empty store. A broken file
// stops every write, so that no row is lost.
func (s *Store) readSealed() (sealedFile, error) {
	raw, err := os.ReadFile(s.file)
	if errors.Is(err, os.ErrNotExist) {
		return sealedFile{Version: 1, Rows: map[string]string{}}, nil
	}
	if err != nil {
		return sealedFile{}, ownerMessage("Swarm cannot read the MCP connections file.")
	}
	var f sealedFile
	if err := json.Unmarshal(raw, &f); err != nil {
		return sealedFile{}, ownerMessage("The MCP connections file is not valid JSON. Swarm does not overwrite it.")
	}
	if f.Version != 1 || f.Rows == nil {
		return sealedFile{}, ownerMessage("The MCP connections file is not a Swarm MCP file.")
	}
	return f, nil
}

func writeAtomic(file string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(file), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(file), "."+filepath.Base(file)+".tmp-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer func() { _ = os.Remove(name) }()
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(name, file)
}

type scanResult struct {
	rows       []Connection
	broken     bool
	unreadable int
}

func (s *Store) scanLocked(ctx context.Context) scanResult {
	f, err := s.readSealed()
	if err != nil {
		return scanResult{broken: true}
	}
	if len(f.Rows) == 0 {
		return scanResult{}
	}
	key, err := s.sealKey(ctx)
	if err != nil {
		return scanResult{unreadable: len(f.Rows)}
	}
	var out scanResult
	for id, sealed := range f.Rows {
		plain, err := s.open(key, sealed)
		if err != nil {
			out.unreadable++
			continue
		}
		var row Connection
		if err := json.Unmarshal(plain, &row); err != nil || row.ID != id {
			out.unreadable++
			continue
		}
		if row.Secrets == nil {
			row.Secrets = map[string]string{}
		}
		out.rows = append(out.rows, row)
	}
	sort.Slice(out.rows, func(i, j int) bool { return out.rows[i].CreatedAt < out.rows[j].CreatedAt })
	return out
}

// Status says whether the file is broken and how many rows are unreadable.
func (s *Store) Status(ctx context.Context) StoreStatus {
	s.mu.Lock()
	defer s.mu.Unlock()
	r := s.scanLocked(ctx)
	return StoreStatus{Broken: r.broken, Unreadable: r.unreadable}
}

// List returns every readable row, oldest first.
func (s *Store) List(ctx context.Context) []Connection {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.scanLocked(ctx).rows
}

// Get returns one row.
func (s *Store) Get(ctx context.Context, id string) (Connection, bool) {
	if !IsConnectionID(id) {
		return Connection{}, false
	}
	for _, r := range s.List(ctx) {
		if r.ID == id {
			return r, true
		}
	}
	return Connection{}, false
}

// Create adds a draft row.
func (s *Store) Create(ctx context.Context, name, rawURL, appKey string) (Connection, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	scan := s.scanLocked(ctx)
	if scan.broken {
		_, err := s.readSealed()
		return Connection{}, err
	}
	if scan.unreadable > 0 {
		return Connection{}, ownerMessage(unreadableText)
	}
	if len(scan.rows) >= maxConnections {
		return Connection{}, ownerMessage(limitText)
	}
	base := cleanName(name)
	if base == "" {
		if u, err := url.Parse(rawURL); err == nil {
			base = u.Hostname()
		}
	}
	taken := map[string]bool{}
	for _, r := range scan.rows {
		taken[strings.ToLower(r.Name)] = true
	}
	unique := base
	for n := 2; taken[strings.ToLower(unique)]; n++ {
		unique = fmt.Sprintf("%s (%d)", cut(base, 58), n)
	}
	public, hasSecret := publicURL(rawURL)
	at := s.nowMillis()
	row := Connection{ID: newID(), Name: unique, AppKey: appKey, URL: public, AuthKind: AuthNone, HeaderNames: []string{},
		Secrets: map[string]string{}, Health: HealthDraft, CreatedAt: at, UpdatedAt: at}
	if hasSecret {
		row.Secrets[secretURL] = rawURL
	}
	return s.saveLocked(ctx, row)
}

func (s *Store) saveLocked(ctx context.Context, row Connection) (Connection, error) {
	f, err := s.readSealed()
	if err != nil {
		return Connection{}, err
	}
	if _, ok := f.Rows[row.ID]; !ok && len(f.Rows) >= maxConnections {
		return Connection{}, ownerMessage(limitText)
	}
	key, err := s.sealKey(ctx)
	if err != nil {
		return Connection{}, err
	}
	row.UpdatedAt = s.nowMillis()
	plain, err := json.Marshal(row)
	if err != nil {
		return Connection{}, err
	}
	sealed, err := s.seal(key, plain)
	if err != nil {
		return Connection{}, err
	}
	f.Rows[row.ID] = sealed
	raw, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return Connection{}, err
	}
	if err := writeAtomic(s.file, raw); err != nil {
		return Connection{}, err
	}
	return row, nil
}

// errNameTaken is the rename refusal.
var errNameTaken = ownerMessage("A connection with that name exists. Choose another name.")

// Update reads the row again, applies change, and saves it. It returns false
// when the row is gone. The listener hears a change that an agent can see.
func (s *Store) Update(ctx context.Context, id string, change func(*Connection)) (Connection, bool, error) {
	s.mu.Lock()
	scan := s.scanLocked(ctx)
	var row *Connection
	for i := range scan.rows {
		if scan.rows[i].ID == id {
			row = &scan.rows[i]
		}
	}
	if row == nil {
		s.mu.Unlock()
		return Connection{}, false, nil
	}
	seen := func(r Connection) string {
		raw, _ := json.Marshal([]any{r.Name, r.Projects, r.Health == HealthNeedsSignIn, r.Tools})
		return string(raw)
	}
	was := seen(*row)
	before := row.Name
	change(row)
	if row.Name = cleanName(row.Name); row.Name == "" {
		row.Name = before
	}
	if !strings.EqualFold(row.Name, before) {
		for _, other := range scan.rows {
			if other.ID != id && strings.EqualFold(other.Name, row.Name) {
				s.mu.Unlock()
				return Connection{}, true, errNameTaken
			}
		}
	}
	saved, err := s.saveLocked(ctx, *row)
	s.mu.Unlock()
	if err != nil {
		return Connection{}, true, err
	}
	if seen(saved) != was && s.onChange != nil {
		s.onChange()
	}
	return saved, true, nil
}

// Remove deletes a row. A broken file is never changed.
func (s *Store) Remove(id string) (bool, error) {
	if !IsConnectionID(id) {
		return false, nil
	}
	s.mu.Lock()
	f, err := s.readSealed()
	if err != nil {
		s.mu.Unlock()
		return false, err
	}
	if _, ok := f.Rows[id]; !ok {
		s.mu.Unlock()
		return false, nil
	}
	delete(f.Rows, id)
	raw, err := json.MarshalIndent(f, "", "  ")
	if err == nil {
		err = writeAtomic(s.file, raw)
	}
	s.mu.Unlock()
	if err != nil {
		return false, err
	}
	if s.onChange != nil {
		s.onChange()
	}
	return true, nil
}

func (s *Store) readProjects() (map[string]Projects, error) {
	raw, err := os.ReadFile(s.projectsFile)
	if errors.Is(err, os.ErrNotExist) {
		return map[string]Projects{}, nil
	}
	if err != nil {
		return nil, ownerMessage("Swarm cannot read the connector projects file.")
	}
	out := map[string]Projects{}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, ownerMessage("The connector projects file is not valid JSON. Swarm does not overwrite it.")
	}
	return out, nil
}

// ConnectorAllowed returns the project setting of a key-based connector.
// GitHub and a connector that is not in the file count as all projects. A
// broken file allows no project.
func (s *Store) ConnectorAllowed(service string) Projects {
	if service == "github" {
		return Projects{All: true}
	}
	s.mu.Lock()
	all, err := s.readProjects()
	s.mu.Unlock()
	if err != nil {
		return Projects{List: []string{}}
	}
	p, ok := all[service]
	if !ok {
		return Projects{All: true}
	}
	return p
}

// ConnectorProjects returns the setting of every connector in the file.
func (s *Store) ConnectorProjects() (map[string]Projects, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.readProjects()
}

// SetConnectorProjects saves the project setting of a key-based connector.
func (s *Store) SetConnectorProjects(service string, projects Projects) error {
	if service == "github" {
		return ownerMessage("GitHub stays available to every project.")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	all, err := s.readProjects()
	if err != nil {
		return err
	}
	all[service] = projects
	raw, err := json.MarshalIndent(all, "", "  ")
	if err != nil {
		return err
	}
	if err := writeAtomic(s.projectsFile, raw); err != nil {
		return err
	}
	if s.onChange != nil {
		go s.onChange()
	}
	return nil
}
