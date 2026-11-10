// Package mcpconn connects agents to remote MCP servers that the owner adds in
// Settings > Connections: a server URL, with a key, custom headers or an
// OAuth sign-in. The daemon keeps the credentials, checks the project grant at
// each call, adds the credential, and removes every secret from each reply.
// The agent never sees a key or a token.
//
// It is a port of Swarm Online's src/server/mcp (swarm-coder main, 2026-11-06):
// the endpoint guard, the MCP client, OAuth 2.1, the sealed store, the broker
// and the tool index keep Online's rules and texts. The differences: the store
// key is in the macOS keychain, the OAuth redirect goes to the daemon's
// loopback listener, and a project is a Swarm project id.
package mcpconn

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"syscall"
	"time"
)

// ownerMessage is an error whose text is a full sentence for the owner.
type ownerMessage string

func (m ownerMessage) Error() string { return string(m) }

// GuardCode names why the guard refused or failed a request.
type GuardCode string

// The guard codes.
const (
	GuardURLInvalid       GuardCode = "url_invalid"
	GuardURLNotHTTPS      GuardCode = "url_not_https"
	GuardURLCredentials   GuardCode = "url_credentials"
	GuardAddressRefused   GuardCode = "address_refused"
	GuardDNSFailed        GuardCode = "dns_failed"
	GuardTooManyRedirects GuardCode = "too_many_redirects"
	GuardRedirectRefused  GuardCode = "redirect_refused"
	GuardTimeout          GuardCode = "timeout"
	GuardTooLarge         GuardCode = "too_large"
	GuardNetwork          GuardCode = "network"
)

// guardReasons are short reasons, lower case and with no full stop. An agent
// sees them after "<name> did not answer: ".
var guardReasons = map[GuardCode]string{
	GuardURLInvalid:       "the address is not a valid https URL",
	GuardURLNotHTTPS:      "the address must start with https://",
	GuardURLCredentials:   "the address must not hold a user name or a password",
	GuardAddressRefused:   "the address is on a private or local network",
	GuardDNSFailed:        "the host name was not found",
	GuardTooManyRedirects: "the server redirected more than 3 times",
	GuardTimeout:          "the server did not answer in time",
	GuardTooLarge:         "the reply was larger than the limit",
	GuardNetwork:          "the connection failed",
	GuardRedirectRefused:  "the server redirected a request that holds a body to another host",
}

// GuardError is a refusal or a failure of the guard.
type GuardError struct {
	Code   GuardCode
	Detail string
}

func (e *GuardError) Error() string {
	if e.Detail != "" {
		return fmt.Sprintf("%s (%s)", guardReasons[e.Code], e.Detail)
	}
	return guardReasons[e.Code]
}

func guardErr(code GuardCode) *GuardError { return &GuardError{Code: code} }

// sentence makes a guard reason into a sentence for the owner.
func sentence(reason string) string {
	if reason == "" {
		return reason
	}
	return strings.ToUpper(reason[:1]) + reason[1:] + "."
}

var (
	safeDetail = regexp.MustCompile(`^[A-Z0-9_]{2,40}$`)
	safeCode   = regexp.MustCompile(`^[a-z_]{2,40}$`)
)

// failureCode is the code of a failure for a log line and for the row, for
// example network:ECONNREFUSED, timeout, unauthorized or invalid_grant. It
// never holds server text, a URL, a header or a value.
func failureCode(err error) string {
	var guard *GuardError
	if errors.As(err, &guard) {
		if guard.Code == GuardNetwork && safeDetail.MatchString(guard.Detail) {
			return "network:" + guard.Detail
		}
		return string(guard.Code)
	}
	var mcpErr *MCPError
	if errors.As(err, &mcpErr) && safeCode.MatchString(string(mcpErr.Code)) {
		return string(mcpErr.Code)
	}
	var oauthErr *OAuthFailure
	if errors.As(err, &oauthErr) && safeCode.MatchString(string(oauthErr.Code)) {
		return string(oauthErr.Code)
	}
	return "error"
}

const (
	maxRedirects   = 3
	maxHeaderName  = 128
	maxHeaderValue = 8192
	maxURLLength   = 2048
)

var refusedHeaderNames = []string{"connection", "content-length", "cookie", "cookie2", "expect", "host",
	"keep-alive", "proxy-authenticate", "proxy-authorization", "proxy-connection", "set-cookie", "set-cookie2", "te", "trailer",
	"transfer-encoding", "upgrade", "via"}

var refusedHeaderPrefixes = []string{"proxy-", "sec-", "http2-"}

// headerToken is the RFC 9110 field-name token.
var headerToken = regexp.MustCompile("^[!#$%&'*+\\-.^_`|~0-9A-Za-z]+$")

// CheckURL accepts an https URL with no user information and no fragment.
func CheckURL(value string) (*url.URL, error) {
	value = strings.TrimSpace(value)
	if value == "" || len(value) > maxURLLength {
		return nil, guardErr(GuardURLInvalid)
	}
	u, err := url.Parse(value)
	if err != nil || u.Opaque != "" {
		return nil, guardErr(GuardURLInvalid)
	}
	if !strings.EqualFold(u.Scheme, "https") {
		if u.Scheme == "" {
			return nil, guardErr(GuardURLInvalid)
		}
		return nil, guardErr(GuardURLNotHTTPS)
	}
	if u.Hostname() == "" {
		return nil, guardErr(GuardURLInvalid)
	}
	if u.User != nil {
		return nil, guardErr(GuardURLCredentials)
	}
	if u.Fragment != "" || strings.Contains(value, "#") {
		return nil, guardErr(GuardURLInvalid)
	}
	u.Scheme = "https"
	return u, nil
}

func mustPrefixes(list ...string) []netip.Prefix {
	out := make([]netip.Prefix, 0, len(list))
	for _, p := range list {
		out = append(out, netip.MustParsePrefix(p))
	}
	return out
}

var refusedV4 = mustPrefixes(
	"0.0.0.0/8", "10.0.0.0/8", "100.64.0.0/10", "127.0.0.0/8", "169.254.0.0/16", "172.16.0.0/12",
	"192.168.0.0/16", "192.0.0.0/24", "198.18.0.0/15", "224.0.0.0/4", "240.0.0.0/4", "255.255.255.255/32",
)

var refusedV6 = mustPrefixes(
	"::/96", "::ffff:0:0:0/96", "64:ff9b::/96", "64:ff9b:1::/48", "2002::/16", "2001::/32", "100::/64",
	"fec0::/10", "ff00::/8", "::1/128", "fc00::/7", "fe80::/10",
)

// RefusedAddress is true for an address that a remote MCP or OAuth request
// must never reach: a private, local, tailnet or special network.
func RefusedAddress(addr netip.Addr) bool {
	if !addr.IsValid() {
		return true
	}
	if addr.Is4In6() {
		addr = addr.Unmap()
	}
	list := refusedV6
	if addr.Is4() {
		list = refusedV4
	}
	for _, p := range list {
		if p.Contains(addr) {
			return true
		}
	}
	return false
}

// checkHeaderName returns "" when Swarm may send this header name, else the
// text to show next to the field.
func checkHeaderName(name string) string {
	if name == "" {
		return "A header name cannot be empty."
	}
	if len(name) > maxHeaderName {
		return fmt.Sprintf("The header name is longer than %d characters.", maxHeaderName)
	}
	if !headerToken.MatchString(name) {
		return "This header name has characters that are not allowed. Use letters, digits and dashes."
	}
	lower := strings.ToLower(name)
	if slices.Contains(refusedHeaderNames, lower) || slices.ContainsFunc(refusedHeaderPrefixes, func(p string) bool { return strings.HasPrefix(lower, p) }) {
		return fmt.Sprintf("Swarm cannot send the %q header.", name)
	}
	return ""
}

// checkHeaderValue returns "" when the value is safe to send. The text never
// repeats the value.
func checkHeaderValue(name, value string) string {
	if len([]rune(value)) > maxHeaderValue {
		return fmt.Sprintf("The value for %q is longer than %d characters.", name, maxHeaderValue)
	}
	for _, r := range value {
		if (r < 0x20 || r > 0x7e) && (r < 0xa0 || r > 0xff) {
			return fmt.Sprintf("The value for %q has a line break, a control character or a character outside Latin-1.", name)
		}
	}
	return ""
}

// GuardPolicy is how the guard resolves and checks addresses. Production uses
// ProductionPolicy only; tests may trust a local test server.
type GuardPolicy struct {
	Resolve func(ctx context.Context, host string) ([]netip.Addr, error)
	Refused func(netip.Addr) bool
	// RootCAs replaces the system roots, for tests only.
	RootCAs *x509.CertPool
}

// ProductionPolicy resolves with the system resolver and refuses every
// private, local and special address.
var ProductionPolicy = GuardPolicy{
	Resolve: func(ctx context.Context, host string) ([]netip.Addr, error) {
		return net.DefaultResolver.LookupNetIP(ctx, "ip", host)
	},
	Refused: RefusedAddress,
}

// GuardedRequest is one outbound request.
type GuardedRequest struct {
	URL     string
	Method  string
	Headers map[string]string
	Body    *string
	// Timeout is one limit for the whole request: the lookups, every
	// redirect, the headers and the body.
	Timeout time.Duration
	// CredentialHeaders hold a credential. A redirect to another origin drops
	// them, and Authorization always.
	CredentialHeaders []string
}

// GuardedResponse is the answer. Close it when you stop reading.
type GuardedResponse struct {
	Status int
	Header http.Header
	URL    string
	Body   io.ReadCloser
	cancel context.CancelFunc
}

// Close stops the time limit and closes the body.
func (r *GuardedResponse) Close() {
	_ = r.Body.Close()
	r.cancel()
}

// HTTP is the one door from the daemon to remote MCP and OAuth servers.
type HTTP func(ctx context.Context, req GuardedRequest) (*GuardedResponse, error)

func safeNetCode(err error) string {
	for _, e := range []syscall.Errno{syscall.ECONNREFUSED, syscall.ECONNRESET, syscall.EHOSTUNREACH, syscall.ENETUNREACH, syscall.ETIMEDOUT, syscall.EPIPE} {
		if errors.Is(err, e) {
			switch e {
			case syscall.ECONNREFUSED:
				return "ECONNREFUSED"
			case syscall.ECONNRESET:
				return "ECONNRESET"
			case syscall.EHOSTUNREACH:
				return "EHOSTUNREACH"
			case syscall.ENETUNREACH:
				return "ENETUNREACH"
			case syscall.ETIMEDOUT:
				return "ETIMEDOUT"
			default:
				return "EPIPE"
			}
		}
	}
	var certErr *tls.CertificateVerificationError
	var unknownCA x509.UnknownAuthorityError
	var hostErr x509.HostnameError
	if errors.As(err, &certErr) || errors.As(err, &unknownCA) || errors.As(err, &hostErr) {
		return "CERT_INVALID"
	}
	return "error"
}

// asGuardError maps any transport error to a GuardError.
func asGuardError(ctx context.Context, err error) error {
	var guard *GuardError
	if errors.As(err, &guard) {
		return guard
	}
	if errors.Is(err, context.DeadlineExceeded) || ctx.Err() != nil {
		return guardErr(GuardTimeout)
	}
	return &GuardError{Code: GuardNetwork, Detail: safeNetCode(err)}
}

// NewGuardedHTTP returns the guarded client for a policy.
func NewGuardedHTTP(policy GuardPolicy) HTTP {
	dial := func(ctx context.Context, network, addr string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(addr)
		if err != nil {
			return nil, guardErr(GuardURLInvalid)
		}
		host = strings.Trim(host, "[]")
		var addrs []netip.Addr
		if ip, err := netip.ParseAddr(host); err == nil {
			addrs = []netip.Addr{ip}
		} else if addrs, err = policy.Resolve(ctx, host); err != nil {
			if ctx.Err() != nil {
				return nil, guardErr(GuardTimeout)
			}
			return nil, guardErr(GuardDNSFailed)
		}
		if len(addrs) == 0 {
			return nil, guardErr(GuardDNSFailed)
		}
		// Every answer must pass, so a second lookup cannot pick a private one.
		for _, a := range addrs {
			if policy.Refused(a) {
				return nil, guardErr(GuardAddressRefused)
			}
		}
		// Connect to the address that passed the check, never to a second lookup.
		var d net.Dialer
		return d.DialContext(ctx, network, net.JoinHostPort(addrs[0].Unmap().String(), port))
	}
	return func(ctx context.Context, request GuardedRequest) (*GuardedResponse, error) {
		ctx, cancel := context.WithTimeout(ctx, request.Timeout)
		u, err := CheckURL(request.URL)
		if err != nil {
			cancel()
			return nil, err
		}
		origin := u.Scheme + "://" + u.Host
		method := request.Method
		if method == "" {
			method = http.MethodGet
		}
		body := request.Body
		headers := map[string]string{}
		for name, value := range request.Headers {
			if problem := checkHeaderName(name); problem != "" {
				cancel()
				return nil, &GuardError{Code: GuardNetwork, Detail: "a header name was refused"}
			}
			if problem := checkHeaderValue(name, value); problem != "" {
				cancel()
				return nil, &GuardError{Code: GuardNetwork, Detail: fmt.Sprintf("header %q refused", name)}
			}
			headers[name] = value
		}
		credentials := map[string]bool{"authorization": true}
		for _, h := range request.CredentialHeaders {
			credentials[strings.ToLower(h)] = true
		}
		client := &http.Client{
			Transport: &http.Transport{
				DialContext:       dial,
				TLSClientConfig:   &tls.Config{RootCAs: policy.RootCAs, MinVersion: tls.VersionTLS12},
				DisableKeepAlives: true,
				ForceAttemptHTTP2: true,
				Proxy:             nil,
			},
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		}
		for hop := 0; ; hop++ {
			var reader io.Reader
			if body != nil {
				reader = strings.NewReader(*body)
			}
			req, err := http.NewRequestWithContext(ctx, method, u.String(), reader)
			if err != nil {
				cancel()
				return nil, guardErr(GuardURLInvalid)
			}
			for name, value := range headers {
				req.Header.Set(name, value)
			}
			resp, err := client.Do(req)
			if err != nil {
				cancel()
				return nil, asGuardError(ctx, err)
			}
			location := ""
			switch resp.StatusCode {
			case http.StatusMovedPermanently, http.StatusFound, http.StatusSeeOther, http.StatusTemporaryRedirect, http.StatusPermanentRedirect:
				location = resp.Header.Get("Location")
			}
			if location == "" {
				return &GuardedResponse{Status: resp.StatusCode, Header: resp.Header, URL: u.String(), Body: resp.Body, cancel: cancel}, nil
			}
			_ = resp.Body.Close()
			if hop >= maxRedirects {
				cancel()
				return nil, guardErr(GuardTooManyRedirects)
			}
			next, err := u.Parse(location)
			if err != nil {
				cancel()
				return nil, guardErr(GuardURLInvalid)
			}
			next.Fragment, next.RawFragment = "", ""
			if u, err = CheckURL(next.String()); err != nil {
				cancel()
				return nil, err
			}
			sameOrigin := u.Scheme+"://"+u.Host == origin
			if !sameOrigin && body != nil && (resp.StatusCode == http.StatusTemporaryRedirect || resp.StatusCode == http.StatusPermanentRedirect) {
				cancel()
				return nil, guardErr(GuardRedirectRefused)
			}
			// A credential never follows a redirect to another origin.
			if !sameOrigin {
				for name := range headers {
					if credentials[strings.ToLower(name)] {
						delete(headers, name)
					}
				}
			}
			if resp.StatusCode == http.StatusSeeOther || ((resp.StatusCode == http.StatusMovedPermanently || resp.StatusCode == http.StatusFound) && method == http.MethodPost) {
				method = http.MethodGet
				body = nil
				for name := range headers {
					if strings.EqualFold(name, "content-type") {
						delete(headers, name)
					}
				}
			}
		}
	}
}

// readBody reads the whole body, at most maxBytes, and closes the response.
func readBody(ctx context.Context, res *GuardedResponse, maxBytes int64) ([]byte, error) {
	defer res.Close()
	raw, err := io.ReadAll(io.LimitReader(res.Body, maxBytes+1))
	if err != nil {
		return nil, asGuardError(ctx, err)
	}
	if int64(len(raw)) > maxBytes {
		return nil, guardErr(GuardTooLarge)
	}
	return raw, nil
}
