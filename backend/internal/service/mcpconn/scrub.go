package mcpconn

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// The result scrubber removes every secret value from each string and each
// object key of a value. The broker uses it for agent replies, and the view
// uses it for remote tool names and descriptions.

// Redacted replaces a secret.
const Redacted = "[secret removed]"

const (
	// minSecret: shorter values would cut normal words out of a result.
	minSecret     = 8
	maxScrubDepth = 64
	maxScrubNodes = 200_000
	scrubCut      = "[too deep or too large]"
)

var authPrefix = regexp.MustCompile(`(?i)^(Bearer|Basic)\s+`)

// secretList is each secret and each secret without a Bearer or Basic prefix,
// longest first. Short values are dropped.
func secretList(secrets []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, v := range secrets {
		for _, s := range []string{v, authPrefix.ReplaceAllString(v, "")} {
			if len(s) >= minSecret && !seen[s] {
				seen[s] = true
				out = append(out, s)
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return len(out[i]) > len(out[j]) })
	return out
}

// cleanString replaces every secret. Overlapping matches merge, so that no
// part of a secret stays.
func cleanString(value string, list []string) string {
	type span struct{ start, end int }
	var spans []span
	for _, secret := range list {
		for at := 0; ; {
			i := strings.Index(value[at:], secret)
			if i < 0 {
				break
			}
			spans = append(spans, span{at + i, at + i + len(secret)})
			at += i + 1
		}
	}
	if len(spans) == 0 {
		return value
	}
	sort.Slice(spans, func(i, j int) bool { return spans[i].start < spans[j].start })
	var b strings.Builder
	pos, start, end := 0, 0, -1
	for _, s := range spans {
		if s.start > end {
			if end >= 0 {
				b.WriteString(value[pos:start])
				b.WriteString(Redacted)
				pos = end
			}
			start, end = s.start, s.end
		} else if s.end > end {
			end = s.end
		}
	}
	b.WriteString(value[pos:start])
	b.WriteString(Redacted)
	b.WriteString(value[end:])
	return b.String()
}

// scrubSecrets removes every secret from each string and each object key of a
// JSON-like value. It stops at a depth of 64 and at 200,000 nodes.
func scrubSecrets(value any, secrets []string) any {
	list := secretList(secrets)
	if len(list) == 0 {
		return value
	}
	nodes := 0
	var clean func(v any, depth int) any
	clean = func(v any, depth int) any {
		switch t := v.(type) {
		case string:
			return cleanString(t, list)
		case []any:
			if depth >= maxScrubDepth {
				return scrubCut
			}
			nodes++
			if nodes > maxScrubNodes {
				return scrubCut
			}
			out := make([]any, len(t))
			for i, x := range t {
				out[i] = clean(x, depth+1)
			}
			return out
		case map[string]any:
			if depth >= maxScrubDepth {
				return scrubCut
			}
			nodes++
			if nodes > maxScrubNodes {
				return scrubCut
			}
			out := make(map[string]any, len(t))
			for k, x := range t {
				nodes++
				if nodes > maxScrubNodes {
					out[scrubCut] = scrubCut
					break
				}
				key := cleanString(k, list)
				for n := 2; ; n++ {
					if _, taken := out[key]; !taken {
						break
					}
					key = fmt.Sprintf("%s %d", cleanString(k, list), n)
				}
				out[key] = clean(x, depth+1)
			}
			return out
		default:
			return v
		}
	}
	return clean(value, 0)
}

// scrubTools removes every secret from a tool list.
func scrubTools(tools []Tool, secrets []string) []Tool {
	list := secretList(secrets)
	out := make([]Tool, 0, len(tools))
	for _, t := range tools {
		if len(list) == 0 {
			out = append(out, t)
			continue
		}
		out = append(out, Tool{
			Name:        cleanString(t.Name, list),
			Description: cleanString(t.Description, list),
			InputSchema: scrubSecrets(t.InputSchema, secrets),
		})
	}
	return out
}
