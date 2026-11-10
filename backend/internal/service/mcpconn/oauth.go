package mcpconn

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

// OAuth 2.1 for remote MCP servers: discovery (RFC 9728, RFC 8414), client
// registration (RFC 7591), PKCE with S256, resource indicators (RFC 8707), the
// issuer check (RFC 9207) and refresh under a lease. Every request goes
// through the guard. No token, code, verifier, client secret or state value
// goes into a log line or an error text.

const (
	stateTTL       = 10 * time.Minute
	refreshSkew    = 60 * time.Second
	maxOpenStates  = 100
	oauthLimit     = 10 * time.Second
	maxDocBytes    = 256 * 1024
	maxScopes      = 50
	maxClientInput = 4096
)

// OAuthCode names an OAuth failure.
type OAuthCode string

// OAuthFailure is an OAuth failure with a text for the owner.
type OAuthFailure struct {
	Code         OAuthCode
	Message      string
	ConnectionID string
}

func (e *OAuthFailure) Error() string { return e.Message }

func oauthFail(code OAuthCode, message string) *OAuthFailure {
	return &OAuthFailure{Code: code, Message: message}
}

// authServerMeta is RFC 8414 metadata.
type authServerMeta struct {
	Issuer                     string
	AuthorizationEndpoint      string
	TokenEndpoint              string
	RegistrationEndpoint       string
	CodeChallengeMethods       []string
	HasCodeChallengeMethods    bool
	TokenEndpointAuthMethods   []string
	HasTokenEndpointAuthMethod bool
	IssParameterSupported      bool
}

type discovery struct {
	Issuer   string
	Meta     authServerMeta
	Resource string
	Scopes   []string
}

var knownOAuthErrors = []string{"invalid_request", "invalid_client", "invalid_grant", "unauthorized_client", "unsupported_grant_type",
	"invalid_scope", "invalid_target", "access_denied", "unsupported_response_type", "server_error", "temporarily_unavailable", "interaction_required",
	"login_required", "consent_required", "invalid_redirect_uri", "invalid_client_metadata", "invalid_software_statement", "unapproved_software_statement"}

func knownError(v any) string {
	s, _ := v.(string)
	if slices.Contains(knownOAuthErrors, s) {
		return s
	}
	return ""
}

func stripSlash(s string) string { return strings.TrimRight(s, "/") }

func b64url(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

type oauthDeps struct {
	http HTTP
}

func readJSONObject(ctx context.Context, res *GuardedResponse) map[string]any {
	raw, err := readBody(ctx, res, maxDocBytes)
	if err != nil {
		return map[string]any{}
	}
	var obj map[string]any
	if json.Unmarshal(raw, &obj) != nil || obj == nil {
		return map[string]any{}
	}
	return obj
}

func (o *oauthDeps) getJSON(ctx context.Context, target string) (map[string]any, error) {
	limit := oauthLimit
	if d, ok := ctx.Deadline(); ok && time.Until(d) < limit {
		limit = time.Until(d)
	}
	if limit <= 0 {
		return nil, guardErr(GuardTimeout)
	}
	res, err := o.http(ctx, GuardedRequest{URL: target, Method: "GET", Timeout: limit,
		Headers: map[string]string{"accept": "application/json", "mcp-protocol-version": mcpProtocolVersion}})
	if err != nil {
		return nil, err
	}
	if res.Status != 200 {
		res.Close()
		return nil, nil
	}
	doc := readJSONObject(ctx, res)
	if len(doc) == 0 {
		return nil, nil
	}
	return doc, nil
}

var (
	resourceMetadataQuoted = regexp.MustCompile(`(?i)resource_metadata="([^"]+)"`)
	resourceMetadataBare   = regexp.MustCompile(`(?i)resource_metadata=([^\s,]+)`)
	scopeQuoted            = regexp.MustCompile(`(?i)(?:^|[\s,])scope="([^"]*)"`)
	scopeBare              = regexp.MustCompile(`(?i)(?:^|[\s,])scope=([^\s,"]+)`)
	scopeToken             = regexp.MustCompile(`^[\x21\x23-\x5b\x5d-\x7e]{1,128}$`)
)

// resourceMetadataURL is the resource_metadata value of a WWW-Authenticate header.
func resourceMetadataURL(header string) string {
	if m := resourceMetadataQuoted.FindStringSubmatch(header); m != nil {
		return m[1]
	}
	if m := resourceMetadataBare.FindStringSubmatch(header); m != nil {
		return m[1]
	}
	return ""
}

// challengeScopes is the scope parameter of a WWW-Authenticate header.
func challengeScopes(header string) []string {
	m := scopeQuoted.FindStringSubmatch(header)
	if m == nil {
		m = scopeBare.FindStringSubmatch(header)
	}
	if m == nil {
		return nil
	}
	return cleanScopes(strings.Fields(m[1]))
}

func cleanScopes(list []string) []string {
	var out []string
	for _, t := range list {
		if scopeToken.MatchString(t) && !slices.Contains(out, t) && len(out) < maxScopes {
			out = append(out, t)
		}
	}
	return out
}

func wellKnown(base *url.URL, name string, insert bool) string {
	p := stripSlash(base.EscapedPath())
	origin := base.Scheme + "://" + base.Host
	if insert {
		return origin + "/.well-known/" + name + p
	}
	return origin + p + "/.well-known/" + name
}

func safeURL(v string) bool {
	if v == "" {
		return false
	}
	_, err := CheckURL(v)
	return err == nil
}

func stringList(v any) ([]string, bool) {
	raw, ok := v.([]any)
	if !ok {
		return nil, false
	}
	var out []string
	for _, x := range raw {
		if s, ok := x.(string); ok && s != "" {
			out = append(out, s)
		}
	}
	return out, true
}

// discover finds the sign-in server of an MCP URL. wwwAuthenticate is the
// header of a 401 answer, hint a saved resource_metadata address, and
// hintScopes the scope of an earlier challenge.
func (o *oauthDeps) discover(ctx context.Context, mcpURL, wwwAuthenticate, hint string, requireResourceDoc bool, hintScopes []string) (discovery, error) {
	resourceURL, err := CheckURL(mcpURL)
	if err != nil {
		return discovery{}, err
	}
	resource := resourceURL.Scheme + "://" + resourceURL.Host + resourceURL.EscapedPath()
	candidates := []string{}
	for _, c := range []string{resourceMetadataURL(wwwAuthenticate), hint} {
		if safeURL(c) {
			candidates = append(candidates, c)
		}
	}
	candidates = append(candidates, wellKnown(resourceURL, "oauth-protected-resource", true),
		resourceURL.Scheme+"://"+resourceURL.Host+"/.well-known/oauth-protected-resource")
	var prm map[string]any
	var servers []string
	tried := map[string]bool{}
	for _, c := range candidates {
		if tried[c] {
			continue
		}
		tried[c] = true
		doc, err := o.getJSON(ctx, c)
		if err != nil || doc == nil {
			continue
		}
		res, _ := doc["resource"].(string)
		if strings.TrimSuffix(res, "/") != strings.TrimSuffix(resource, "/") {
			continue
		}
		list, _ := stringList(doc["authorization_servers"])
		if len(list) == 0 {
			continue
		}
		prm, servers = doc, list
		break
	}
	if requireResourceDoc && prm == nil {
		return discovery{}, oauthFail("discovery_failed", "Swarm could not find the sign-in details of this server.")
	}
	issuer := resourceURL.Scheme + "://" + resourceURL.Host
	if len(servers) > 0 {
		issuer = servers[0]
	}
	issuer = stripSlash(issuer)
	issuerURL, err := CheckURL(issuer)
	if err != nil {
		return discovery{}, oauthFail("endpoint_refused", "The address of the sign-in server is not a safe https address.")
	}
	issuerOrigin := issuerURL.Scheme + "://" + issuerURL.Host
	var docs []string
	if stripSlash(issuerURL.EscapedPath()) != "" {
		docs = []string{wellKnown(issuerURL, "oauth-authorization-server", true), wellKnown(issuerURL, "openid-configuration", true), wellKnown(issuerURL, "openid-configuration", false)}
	} else {
		docs = []string{issuerOrigin + "/.well-known/oauth-authorization-server", issuerOrigin + "/.well-known/openid-configuration"}
	}
	var meta *authServerMeta
	mismatch := false
	for _, d := range docs {
		doc, err := o.getJSON(ctx, d)
		if err != nil || doc == nil {
			continue
		}
		iss, _ := doc["issuer"].(string)
		if iss == "" || stripSlash(iss) != issuer {
			mismatch = true
			continue
		}
		m := authServerMeta{Issuer: iss}
		m.AuthorizationEndpoint, _ = doc["authorization_endpoint"].(string)
		m.TokenEndpoint, _ = doc["token_endpoint"].(string)
		if reg, ok := doc["registration_endpoint"]; ok && reg != nil {
			s, isString := reg.(string)
			if !isString {
				s = fmt.Sprint(reg)
			}
			m.RegistrationEndpoint = s
		}
		m.CodeChallengeMethods, m.HasCodeChallengeMethods = stringList(doc["code_challenge_methods_supported"])
		m.TokenEndpointAuthMethods, m.HasTokenEndpointAuthMethod = stringList(doc["token_endpoint_auth_methods_supported"])
		m.IssParameterSupported = doc["authorization_response_iss_parameter_supported"] == true
		meta = &m
		break
	}
	if meta == nil {
		if mismatch {
			return discovery{}, oauthFail("issuer_mismatch", "The sign-in server sent details for another issuer, so Swarm stopped.")
		}
		return discovery{}, oauthFail("discovery_failed", "Swarm could not find the sign-in details of this server.")
	}
	check := [][2]string{{"sign-in", meta.AuthorizationEndpoint}, {"token", meta.TokenEndpoint}}
	if meta.RegistrationEndpoint != "" {
		check = append(check, [2]string{"registration", meta.RegistrationEndpoint})
	}
	for _, c := range check {
		if _, err := CheckURL(c[1]); err != nil {
			return discovery{}, oauthFail("endpoint_refused", fmt.Sprintf("The %s address of this server is not a safe https address.", c[0]))
		}
	}
	if meta.HasCodeChallengeMethods && !slices.Contains(meta.CodeChallengeMethods, "S256") {
		return discovery{}, oauthFail("no_pkce", "This sign-in server does not support PKCE with S256, so Swarm cannot use it.")
	}
	scopes := challengeScopes(wwwAuthenticate)
	if len(scopes) == 0 {
		scopes = cleanScopes(hintScopes)
	}
	if len(scopes) == 0 && prm != nil {
		supported, _ := stringList(prm["scopes_supported"])
		if len(supported) > maxScopes {
			supported = supported[:maxScopes]
		}
		scopes = supported
	}
	if scopes == nil {
		scopes = []string{}
	}
	// The resource is the MCP URL without its query: a query can hold a key.
	return discovery{Issuer: issuer, Meta: *meta, Resource: resource, Scopes: scopes}, nil
}

func (o *oauthDeps) postForm(ctx context.Context, target string, headers map[string]string, body string, credentialHeaders []string) (int, map[string]any, error) {
	res, err := o.http(ctx, GuardedRequest{URL: target, Method: "POST", Timeout: oauthLimit, Headers: headers, Body: &body, CredentialHeaders: credentialHeaders})
	if err != nil {
		return 0, nil, err
	}
	return res.Status, readJSONObject(ctx, res), nil
}

type clientCreds struct {
	ClientID     string
	ClientSecret string
	ClientAuth   string
	Manual       bool
}

func (o *oauthDeps) registerClient(ctx context.Context, meta authServerMeta, redirectURI string) (clientCreds, error) {
	supported := meta.TokenEndpointAuthMethods
	if !meta.HasTokenEndpointAuthMethod {
		supported = []string{clientAuthSecretBasic}
	}
	method := clientAuthSecretPost
	switch {
	case slices.Contains(supported, clientAuthNone):
		method = clientAuthNone
	case slices.Contains(supported, clientAuthSecretBasic):
		method = clientAuthSecretBasic
	}
	body, _ := json.Marshal(map[string]any{
		"client_name": "Swarm 3.0 (this Mac)", "redirect_uris": []string{redirectURI},
		"grant_types": []string{"authorization_code", "refresh_token"}, "response_types": []string{"code"}, "token_endpoint_auth_method": method,
	})
	status, reply, err := o.postForm(ctx, meta.RegistrationEndpoint, map[string]string{"content-type": "application/json", "accept": "application/json"}, string(body), nil)
	if err != nil {
		return clientCreds{}, err
	}
	clientID, _ := reply["client_id"].(string)
	if status < 200 || status >= 300 || clientID == "" {
		suffix := ""
		if code := knownError(reply["error"]); code != "" {
			suffix = ", " + code
		}
		return clientCreds{}, oauthFail("registration_failed", fmt.Sprintf("The sign-in server did not register Swarm (HTTP %d%s).", status, suffix))
	}
	secret, _ := reply["client_secret"].(string)
	granted, _ := reply["token_endpoint_auth_method"].(string)
	if granted == "" {
		granted = clientAuthNone
		if secret != "" {
			granted = clientAuthSecretBasic
		}
	}
	if granted != clientAuthNone && granted != clientAuthSecretBasic && granted != clientAuthSecretPost {
		granted = clientAuthSecretBasic
	}
	return clientCreds{ClientID: clientID, ClientSecret: secret, ClientAuth: granted}, nil
}

func pkce() (string, string) {
	b := make([]byte, 48)
	_, _ = rand.Read(b)
	verifier := b64url(b)
	sum := sha256.Sum256([]byte(verifier))
	return verifier, b64url(sum[:])
}

// pendingBinding holds the sign-in details of a state. They go onto the row
// only after the code exchange works.
type pendingBinding struct {
	OAuth        OAuthConfig
	ClientSecret string
	MetaIssuer   string
	IssRequired  bool
}

type stateRow struct {
	ConnectionID string
	Verifier     string
	Binding      pendingBinding
	ExpiresAt    time.Time
}

// oauthStates live in daemon memory only. take() deletes a state before the
// caller checks it, so a state works once.
type oauthStates struct {
	mu    sync.Mutex
	order []string
	rows  map[string]stateRow
	now   func() time.Time
}

func newOAuthStates(now func() time.Time) *oauthStates {
	return &oauthStates{rows: map[string]stateRow{}, now: now}
}

func (s *oauthStates) prune() {
	t := s.now()
	kept := s.order[:0]
	for _, k := range s.order {
		if row, ok := s.rows[k]; ok && row.ExpiresAt.After(t) {
			kept = append(kept, k)
		} else {
			delete(s.rows, k)
		}
	}
	s.order = kept
}

func (s *oauthStates) create(row stateRow) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.prune()
	for len(s.order) >= maxOpenStates {
		delete(s.rows, s.order[0])
		s.order = s.order[1:]
	}
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	state := b64url(b)
	row.ExpiresAt = s.now().Add(stateTTL)
	s.rows[state] = row
	s.order = append(s.order, state)
	return state
}

func (s *oauthStates) take(state string) (stateRow, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	row, ok := s.rows[state]
	delete(s.rows, state)
	s.prune()
	return row, ok
}

func authorizeURL(endpoint, clientID, redirectURI, state, challenge, resource string, scopes []string) string {
	u, _ := url.Parse(endpoint)
	q := u.Query()
	q.Set("response_type", "code")
	q.Set("client_id", clientID)
	q.Set("redirect_uri", redirectURI)
	q.Set("state", state)
	q.Set("code_challenge", challenge)
	q.Set("code_challenge_method", "S256")
	q.Set("resource", resource)
	if len(scopes) > 0 {
		q.Set("scope", strings.Join(scopes, " "))
	}
	u.RawQuery = q.Encode()
	return u.String()
}

type tokens struct {
	AccessToken  string
	RefreshToken string
	ExpiresAt    *int64
}

var digitsOnly = regexp.MustCompile(`^\d{1,10}$`)

func (o *oauthDeps) tokenRequest(ctx context.Context, tokenEndpoint string, client clientCreds, form url.Values, now time.Time) (tokens, error) {
	isCode := form.Get("grant_type") == "authorization_code"
	form.Set("client_id", client.ClientID)
	headers := map[string]string{"content-type": "application/x-www-form-urlencoded", "accept": "application/json"}
	if client.ClientAuth == clientAuthSecretBasic && client.ClientSecret != "" {
		raw := url.QueryEscape(client.ClientID) + ":" + url.QueryEscape(client.ClientSecret)
		headers["authorization"] = "Basic " + base64.StdEncoding.EncodeToString([]byte(raw))
	}
	if client.ClientAuth == clientAuthSecretPost && client.ClientSecret != "" {
		form.Set("client_secret", client.ClientSecret)
	}
	status, body, err := o.postForm(ctx, tokenEndpoint, headers, form.Encode(), []string{"authorization"})
	if err != nil {
		return tokens{}, err
	}
	if body["error"] == "invalid_grant" {
		if isCode {
			return tokens{}, oauthFail("invalid_grant", "The sign-in server refused the sign-in code.")
		}
		return tokens{}, oauthFail("invalid_grant", "The sign-in server refused the saved sign-in.")
	}
	failed := func() error {
		suffix := ""
		if code := knownError(body["error"]); code != "" {
			suffix = ", " + code
		}
		return oauthFail("token_failed", fmt.Sprintf("The sign-in server did not give a token (HTTP %d%s).", status, suffix))
	}
	if status != 200 {
		return tokens{}, failed()
	}
	access, ok := body["access_token"].(string)
	if !ok || access == "" {
		return tokens{}, failed()
	}
	out := tokens{AccessToken: access}
	if raw, ok := body["refresh_token"]; ok && raw != nil {
		s, isString := raw.(string)
		if !isString {
			return tokens{}, failed()
		}
		out.RefreshToken = s
	}
	if raw, ok := body["token_type"]; ok && raw != nil {
		s, isString := raw.(string)
		if !isString || !strings.EqualFold(s, "bearer") {
			return tokens{}, failed()
		}
	}
	if raw, ok := body["expires_in"]; ok && raw != nil {
		var seconds float64
		switch v := raw.(type) {
		case float64:
			seconds = v
		case string:
			if !digitsOnly.MatchString(v) {
				return tokens{}, failed()
			}
			n, _ := strconv.ParseFloat(v, 64)
			seconds = n
		default:
			return tokens{}, failed()
		}
		if seconds <= 0 {
			return tokens{}, failed()
		}
		at := now.Add(time.Duration(seconds * float64(time.Second))).UnixMilli()
		out.ExpiresAt = &at
	}
	return out, nil
}

// tokenKeeper keeps OAuth tokens fresh. One refresh for each connection at a
// time; a second caller waits for the first.
type tokenKeeper struct {
	store  *Store
	oauth  *oauthDeps
	now    func() time.Time
	log    func(string)
	mu     sync.Mutex
	leases map[string]*lease
}

type lease struct {
	done chan struct{}
	row  Connection
	ok   bool
}

// fresh returns the row, refreshed first when its access token ends within
// 60 s, or at once with force (after a 401).
func (k *tokenKeeper) fresh(ctx context.Context, id string, force bool) (Connection, bool) {
	row, ok := k.store.Get(ctx, id)
	if !ok || row.AuthKind != AuthOAuth || row.OAuth == nil {
		return row, ok
	}
	expiring := row.OAuth.AccessTokenExpiresAt != nil && *row.OAuth.AccessTokenExpiresAt-k.now().UnixMilli() < refreshSkew.Milliseconds()
	if !force && !expiring {
		return row, true
	}
	k.mu.Lock()
	if l, running := k.leases[id]; running {
		k.mu.Unlock()
		select {
		case <-l.done:
			return l.row, l.ok
		case <-ctx.Done():
			return row, true
		}
	}
	l := &lease{done: make(chan struct{})}
	k.leases[id] = l
	k.mu.Unlock()
	// The refresh runs on, also when this caller stops waiting.
	go func() {
		l.row, l.ok = k.refresh(context.WithoutCancel(ctx), id, force)
		k.mu.Lock()
		delete(k.leases, id)
		k.mu.Unlock()
		close(l.done)
	}()
	select {
	case <-l.done:
		return l.row, l.ok
	case <-ctx.Done():
		return row, true
	}
}

type usedRefresh struct{ refreshToken, tokenEndpoint, clientID string }

func sameRefresh(r *Connection, used usedRefresh) bool {
	return r.Secrets[secretRefreshToken] == used.refreshToken && r.OAuth != nil && r.OAuth.TokenEndpoint == used.tokenEndpoint && r.OAuth.ClientID == used.clientID
}

func (k *tokenKeeper) refresh(ctx context.Context, id string, force bool) (Connection, bool) {
	row, ok := k.store.Get(ctx, id)
	if !ok || row.OAuth == nil {
		return row, ok
	}
	refreshToken := row.Secrets[secretRefreshToken]
	if refreshToken == "" {
		// Without a refresh token the access token works until it ends,
		// unless a call just showed that it does not.
		left := row.OAuth.AccessTokenExpiresAt
		if !force && row.Secrets[secretAccessToken] != "" && left != nil && *left > k.now().UnixMilli() {
			return row, true
		}
		return k.signInNeeded(ctx, id, nil)
	}
	used := usedRefresh{refreshToken, row.OAuth.TokenEndpoint, row.OAuth.ClientID}
	form := url.Values{"grant_type": {"refresh_token"}, "refresh_token": {refreshToken}, "resource": {row.OAuth.Resource}}
	t, err := k.oauth.tokenRequest(ctx, row.OAuth.TokenEndpoint, clientCreds{ClientID: row.OAuth.ClientID, ClientSecret: row.Secrets[secretClientSecret], ClientAuth: row.OAuth.ClientAuth}, form, k.now())
	if err != nil {
		var failure *OAuthFailure
		if errors.As(err, &failure) && failure.Code == "invalid_grant" {
			k.log(fmt.Sprintf("[mcp] %s invalid_grant", id))
			return k.signInNeeded(ctx, id, &used)
		}
		k.log(fmt.Sprintf("[mcp] %s %s", id, failureCode(err)))
		return k.store.Get(ctx, id)
	}
	saved, found, err := k.store.Update(ctx, id, func(r *Connection) {
		// A sign-in that finished during the network call wins.
		if r.OAuth == nil || !sameRefresh(r, used) {
			return
		}
		if t.RefreshToken != "" {
			r.Secrets[secretRefreshToken] = t.RefreshToken
		}
		r.Secrets[secretAccessToken] = t.AccessToken
		r.OAuth.AccessTokenExpiresAt = t.ExpiresAt
		r.OAuth.Status = oauthStatusOK
	})
	if err != nil {
		return k.store.Get(ctx, id)
	}
	return saved, found
}

func (k *tokenKeeper) signInNeeded(ctx context.Context, id string, used *usedRefresh) (Connection, bool) {
	saved, found, err := k.store.Update(ctx, id, func(r *Connection) {
		if r.OAuth == nil || (used != nil && !sameRefresh(r, *used)) {
			return
		}
		delete(r.Secrets, secretAccessToken)
		delete(r.Secrets, secretRefreshToken)
		r.OAuth.Status = oauthStatusSignIn
		r.OAuth.AccessTokenExpiresAt = nil
		r.Health = HealthNeedsSignIn
	})
	if err != nil {
		return k.store.Get(ctx, id)
	}
	return saved, found
}

// signIn starts and finishes the owner's sign-in.
type signIn struct {
	store       *Store
	oauth       *oauthDeps
	states      *oauthStates
	redirectURI func() string
	now         func() time.Time
}

// start discovers, finds or registers a client, keeps a state, and gives the
// authorization URL for the owner's browser. The row changes only when the
// code exchange works.
func (si *signIn) start(ctx context.Context, id string, manual *clientCreds, metadataHint string, scopes []string) (string, error) {
	row, ok := si.store.Get(ctx, id)
	if !ok {
		return "", oauthFail("state_invalid", "This connection does not exist any more.")
	}
	redirectURI := si.redirectURI()
	old := row.OAuth
	hint := ""
	if safeURL(metadataHint) {
		hint = metadataHint
	} else if old != nil {
		hint = old.ResourceMetadataURL
	}
	found, err := si.oauth.discover(ctx, targetOf(row, nil).URL, "", hint, false, scopes)
	if err != nil {
		return "", err
	}
	bound := old != nil && old.Issuer == found.Issuer && old.Resource == found.Resource && old.RedirectURI == redirectURI
	var client clientCreds
	switch {
	case manual != nil && strings.TrimSpace(manual.ClientID) != "":
		if len(manual.ClientSecret) > maxClientInput {
			return "", &OAuthFailure{Code: "manual_client_required", Message: "The client secret is too long.", ConnectionID: id}
		}
		secret := strings.TrimSpace(manual.ClientSecret)
		auth := clientAuthNone
		if secret != "" {
			auth = clientAuthSecretPost
			if !found.Meta.HasTokenEndpointAuthMethod || slices.Contains(found.Meta.TokenEndpointAuthMethods, clientAuthSecretBasic) {
				auth = clientAuthSecretBasic
			}
		}
		client = clientCreds{ClientID: cut(strings.TrimSpace(manual.ClientID), 256), ClientSecret: secret, ClientAuth: auth, Manual: true}
	case bound && old.ClientID != "":
		client = clientCreds{ClientID: old.ClientID, ClientSecret: row.Secrets[secretClientSecret], ClientAuth: old.ClientAuth, Manual: old.ManualClient}
	case old != nil && old.ManualClient:
		return "", &OAuthFailure{Code: "manual_client_required", Message: "The sign-in server or the address changed. Enter the client ID again.", ConnectionID: id}
	case found.Meta.RegistrationEndpoint != "":
		if client, err = si.oauth.registerClient(ctx, found.Meta, redirectURI); err != nil {
			return "", err
		}
	default:
		return "", &OAuthFailure{Code: "manual_client_required", Message: "This server needs a client ID. Enter it under Advanced authentication.", ConnectionID: id}
	}
	verifier, challenge := pkce()
	state := si.states.create(stateRow{ConnectionID: id, Verifier: verifier, Binding: pendingBinding{
		OAuth: OAuthConfig{Issuer: found.Issuer, AuthorizationEndpoint: found.Meta.AuthorizationEndpoint, TokenEndpoint: found.Meta.TokenEndpoint,
			RegistrationEndpoint: found.Meta.RegistrationEndpoint, ClientID: client.ClientID, ClientAuth: client.ClientAuth, Scopes: found.Scopes,
			Resource: found.Resource, RedirectURI: redirectURI, ManualClient: client.Manual, ResourceMetadataURL: hint},
		ClientSecret: client.ClientSecret, MetaIssuer: found.Meta.Issuer, IssRequired: found.Meta.IssParameterSupported,
	}})
	return authorizeURL(found.Meta.AuthorizationEndpoint, client.ClientID, redirectURI, state, challenge, found.Resource, found.Scopes), nil
}

// finish handles the redirect to the callback. The state is taken before any
// other step.
func (si *signIn) finish(ctx context.Context, query url.Values) (Connection, error) {
	state := query.Get("state")
	row, ok := stateRow{}, false
	if state != "" {
		row, ok = si.states.take(state)
	}
	if !ok {
		return Connection{}, oauthFail("state_invalid", "This sign-in link is not valid or was used already. Start the sign-in again.")
	}
	fail := func(code OAuthCode, message string) error {
		return &OAuthFailure{Code: code, Message: message, ConnectionID: row.ConnectionID}
	}
	if !row.ExpiresAt.After(si.now()) {
		return Connection{}, fail("state_expired", "This sign-in took longer than 10 minutes. Start it again.")
	}
	if errCode := query.Get("error"); errCode != "" {
		// The text holds the error code only when it is on the allow-list.
		if code := knownError(errCode); code != "" {
			return Connection{}, fail("provider_error", "The sign-in server answered "+code+".")
		}
		return Connection{}, fail("provider_error", "The sign-in server answered with an error.")
	}
	conn, ok := si.store.Get(ctx, row.ConnectionID)
	if !ok {
		return Connection{}, fail("state_invalid", "This connection does not exist any more.")
	}
	bind := row.Binding
	current := ""
	if u, err := url.Parse(targetOf(conn, nil).URL); err == nil {
		current = u.Scheme + "://" + u.Host + u.EscapedPath()
	}
	if current != bind.OAuth.Resource {
		return Connection{}, fail("state_invalid", "The address of this connection changed. Start the sign-in again.")
	}
	iss, hasIss := query.Get("iss"), query.Has("iss")
	if (!hasIss && bind.IssRequired) || (hasIss && iss != bind.MetaIssuer) {
		return Connection{}, fail("iss_mismatch", "The answer came from another sign-in server, so Swarm stopped.")
	}
	code := query.Get("code")
	if code == "" {
		return Connection{}, fail("token_failed", "The sign-in server sent no code.")
	}
	form := url.Values{"grant_type": {"authorization_code"}, "code": {code}, "redirect_uri": {bind.OAuth.RedirectURI},
		"code_verifier": {row.Verifier}, "resource": {bind.OAuth.Resource}}
	t, err := si.oauth.tokenRequest(ctx, bind.OAuth.TokenEndpoint, clientCreds{ClientID: bind.OAuth.ClientID, ClientSecret: bind.ClientSecret, ClientAuth: bind.OAuth.ClientAuth}, form, si.now())
	if err != nil {
		var failure *OAuthFailure
		var guard *GuardError
		switch {
		case errors.As(err, &failure):
			c := failure.Code
			if c == "invalid_grant" {
				c = "token_failed"
			}
			return Connection{}, fail(c, failure.Message)
		case errors.As(err, &guard):
			return Connection{}, fail("token_failed", "The token server did not answer: "+guard.Error()+".")
		default:
			return Connection{}, fail("token_failed", "The sign-in server did not give a token.")
		}
	}
	// One write: the new binding and the new tokens together.
	saved, found, err := si.store.Update(ctx, conn.ID, func(r *Connection) {
		for key := range r.Secrets {
			if strings.HasPrefix(key, headerSecretPrefix) {
				delete(r.Secrets, key)
			}
		}
		r.HeaderNames = []string{}
		r.AuthKind = AuthOAuth
		if bind.ClientSecret != "" {
			r.Secrets[secretClientSecret] = bind.ClientSecret
		} else {
			delete(r.Secrets, secretClientSecret)
		}
		if t.RefreshToken != "" {
			r.Secrets[secretRefreshToken] = t.RefreshToken
		} else {
			delete(r.Secrets, secretRefreshToken)
		}
		r.Secrets[secretAccessToken] = t.AccessToken
		cfg := bind.OAuth
		cfg.AccessTokenExpiresAt = t.ExpiresAt
		cfg.Status = oauthStatusOK
		r.OAuth = &cfg
		if r.Health == HealthNeedsSignIn {
			r.Health = HealthOK
			if r.Projects == nil {
				r.Health = HealthDraft
			}
		}
	})
	if err != nil || !found {
		return Connection{}, fail("token_failed", "Swarm could not save the sign-in.")
	}
	return saved, nil
}
