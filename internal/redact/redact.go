// Package redact is the one masking rule for text Drydock shows that a
// subprocess wrote: the session server's log (internal/supervisor) and a
// failed build's log (internal/provision). Design §13.5, redact by default.
//
// Two orders matter. Literal values are masked before the credential
// patterns, so a pattern cannot consume part of a value and leave the rest of
// it matchable by nothing (a secret "pfx-ghp_…" would otherwise show its
// "pfx-"). And longer values before shorter ones, so a value containing
// another is masked whole.
//
// # Rules and details
//
// Literal values first, longest first, then GitHub, Anthropic and bearer token
// shapes and credential query parameters. The session server's Ring, the build
// log (provision.BuildLog: masked when kept and again when served — only the
// masked copy is held, so a rotated or revoked value stays masked; masked,
// then cut; withheld when the values cannot be read) and the journal's logTail
// all use it.
package redact

import (
	"regexp"
	"sort"
	"strings"
)

// MinMask is the shortest literal value masked: masking "a" everywhere would
// make a log unreadable and protect nothing.
const MinMask = 4

// Mask is what a masked span becomes.
const Mask = "[redacted]"

// tokenPatterns are credentials by shape: GitHub's token prefixes, Anthropic
// keys and OAuth tokens, a bearer header, and a credential-looking query
// parameter.
var tokenPatterns = []*regexp.Regexp{
	regexp.MustCompile(`gh[pousr]_[A-Za-z0-9]{20,}`),
	regexp.MustCompile(`github_pat_[A-Za-z0-9_]{20,}`),
	regexp.MustCompile(`sk-ant-[A-Za-z0-9_-]{10,}`),
	regexp.MustCompile(`(?i)(bearer\s+)[A-Za-z0-9._~+/=-]{8,}`),
	regexp.MustCompile(`(?i)([?&](?:code|token|access_token|refresh_token|key)=)[^&\s"']+`),
}

// String masks the given literal values in s, longest first, then every
// credential shape.
func String(s string, values []string) string {
	vs := make([]string, 0, len(values))
	for _, v := range values {
		if len(v) >= MinMask {
			vs = append(vs, v)
		}
	}
	sort.SliceStable(vs, func(i, j int) bool { return len(vs[i]) > len(vs[j]) })
	for _, v := range vs {
		s = strings.ReplaceAll(s, v, Mask)
	}
	for _, re := range tokenPatterns {
		if re.NumSubexp() > 0 {
			s = re.ReplaceAllString(s, "${1}"+Mask)
		} else {
			s = re.ReplaceAllString(s, Mask)
		}
	}
	return s
}
