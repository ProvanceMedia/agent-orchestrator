package mcpconn

import (
	"context"
	"errors"
	"fmt"
)

// Check link: what a remote MCP server needs before Swarm can use it. An
// error text never holds a URL query, a header value or a key.

type probeKind string

const (
	probeOpen     probeKind = "open"
	probeNeedsKey probeKind = "needs_key"
	probeOAuth    probeKind = "oauth"
	probeError    probeKind = "error"
)

type probeOutcome struct {
	Kind                probeKind
	Tools               []Tool
	ManualClient        bool
	ResourceMetadataURL string
	Scopes              []string
	// Field is the field that the owner must change: url or key.
	Field   string
	Message string
	// Code is for the log line only.
	Code string
}

const notMCP = "The server did not answer as an MCP server."

func guardSentence(err error) string {
	var guard *GuardError
	if errors.As(err, &guard) {
		return sentence(guardReasons[guard.Code])
	}
	return sentence(guardReasons[GuardNetwork])
}

func (s *Service) probe(ctx context.Context, target Target) probeOutcome {
	urlError := func(message string, err error) probeOutcome {
		return probeOutcome{Kind: probeError, Field: "url", Message: message, Code: failureCode(err)}
	}
	if _, err := CheckURL(target.URL); err != nil {
		return urlError(guardSentence(err), err)
	}
	ctx, cancel := context.WithTimeout(ctx, probeLimit)
	defer cancel()
	tools, err := s.client.listTools(ctx, target)
	if err == nil {
		return probeOutcome{Kind: probeOpen, Tools: tools}
	}
	var guard *GuardError
	if errors.As(err, &guard) {
		return urlError(sentence(guardReasons[guard.Code]), err)
	}
	var mcpErr *MCPError
	if !errors.As(err, &mcpErr) {
		return urlError(notMCP, err)
	}
	if mcpErr.Code == MCPHTTPStatus {
		if mcpErr.Status >= 500 {
			return urlError(fmt.Sprintf("The server answered HTTP %d. Try again later.", mcpErr.Status), err)
		}
		return urlError(fmt.Sprintf("The server answered HTTP %d. Check the address.", mcpErr.Status), err)
	}
	if mcpErr.Code != MCPUnauthorized {
		return urlError(notMCP, err)
	}
	if len(target.CredentialHeaders) > 0 {
		return probeOutcome{Kind: probeError, Field: "key", Message: "The server did not accept this key or these headers.", Code: string(mcpErr.Code)}
	}
	// A 401 with a challenge gets full discovery. A 401 with no challenge
	// counts as OAuth only through a protected resource document.
	found, derr := s.oauth.discover(ctx, target.URL, mcpErr.WWWAuthenticate, "", mcpErr.WWWAuthenticate == "", nil)
	if derr != nil {
		return probeOutcome{Kind: probeNeedsKey}
	}
	out := probeOutcome{Kind: probeOAuth, ManualClient: found.Meta.RegistrationEndpoint == "", Scopes: challengeScopes(mcpErr.WWWAuthenticate)}
	if hint := resourceMetadataURL(mcpErr.WWWAuthenticate); hint != "" && len(hint) <= maxURLLength && safeURL(hint) {
		out.ResourceMetadataURL = hint
	}
	return out
}
