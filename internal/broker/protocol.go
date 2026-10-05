// Package broker is the token broker (design §9): one Unix socket per
// workspace, bind-mounted into that workspace's container and no other, on
// which the container asks for a GitHub token and gets one scoped to its own
// repository.
//
// **The socket is the identity.** A request names no repository, because it
// could not be trusted to: the broker knows which workspace a connection is
// for from which socket it arrived on, and the workspace's repository was fixed
// when the workspace was created. A compromised container can ask only for
// what it already has.
package broker

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

// Scope is what a token is for. Each maps to exactly the permission set in
// design §9.3, and the golden files in testdata pin both: widening either is
// a test failure, not a quiet change.
type Scope string

const (
	ScopeGit Scope = "git" // the credential helper: clone, fetch, push
	ScopeGH  Scope = "gh"  // the gh shim: PRs, issues, workflow runs
)

// Permissions returns the scope's §9.3 permission set. A fresh map each call,
// so no caller can widen a shared one.
func (s Scope) Permissions() map[string]string {
	switch s {
	case ScopeGit:
		return map[string]string{
			"metadata": "read", "contents": "write",
			// Without it, a push that touches .github/workflows/ is
			// refused with an error that does not say why (§9.3).
			"workflows": "write",
		}
	case ScopeGH:
		return map[string]string{
			"metadata": "read", "contents": "write", "pull_requests": "write", "issues": "write",
			"workflows": "write", "actions": "write", "checks": "read",
		}
	}
	return nil
}

// requestedBy is token_grant.requested_by for a scope.
func (s Scope) requestedBy() string {
	if s == ScopeGit {
		return "git-credential"
	}
	return "gh"
}

// Request is one parsed request line.
type Request struct {
	Verb  string // GET-TOKEN, GET-SECRETS or PING
	Scope Scope  // GET-TOKEN only
}

// maxLine bounds a request. The longest valid one is 20 bytes; anything near
// this is not a client of ours.
const maxLine = 128

// ErrBadRequest is any line that is not exactly a request this broker speaks.
var ErrBadRequest = errors.New("broker: bad request")

// Parse reads one request line, strictly. The grammar is the whole security
// argument in miniature: there is no field for a repository, and a line that
// carries any field the grammar does not have — a repo, an owner, a second
// scope — is refused rather than ignored (testing §8.3, cross-broker). A
// permissive parser that skipped unknown fields would invite exactly the
// client that tries one.
func Parse(line string) (Request, error) {
	if len(line) > maxLine || strings.ContainsAny(line, "\r\n\x00") {
		return Request{}, ErrBadRequest
	}
	f := strings.Split(line, " ")
	switch {
	case len(f) == 1 && f[0] == "PING":
		return Request{Verb: "PING"}, nil
	case len(f) == 1 && f[0] == "GET-SECRETS":
		// No arguments at all (§10.3): which secrets, like which
		// repository, is decided by the socket the line arrived on.
		return Request{Verb: "GET-SECRETS"}, nil
	case len(f) == 2 && f[0] == "GET-TOKEN":
		switch f[1] {
		case "scope=git":
			return Request{Verb: "GET-TOKEN", Scope: ScopeGit}, nil
		case "scope=gh":
			return Request{Verb: "GET-TOKEN", Scope: ScopeGH}, nil
		}
	}
	return Request{}, ErrBadRequest
}

// Reasons an ERR line can give. The client turns each into one sentence
// (§12: "GitHub access unavailable", never a git error), so the set is
// closed and small.
const (
	ReasonBadRequest   = "bad_request"
	ReasonRepoArchived = "repo_archived"
	ReasonRevoked      = "revoked"      // the repository left the installation, or the workspace is going away
	ReasonRateLimited  = "rate_limited" // GitHub's limit, or the App suspended
	ReasonUnavailable  = "unavailable"  // GitHub unreachable, or anything else
)

func okToken(token string, expires time.Time) string {
	return fmt.Sprintf("OK token=%s expires_at=%s\n", token, expires.UTC().Format(time.RFC3339))
}

func errLine(reason string) string { return "ERR reason=" + reason + "\n" }

// secretsAnswer frames a GET-SECRETS answer (§10.3):
//
//	OK count=N
//	NAME value      (N lines)
//	END
//
// The framing is only sound because a value is one line — refused at write
// if it holds any control character (§10.1), checked again by the store
// before delivery — and the client fails the fetch when count= disagrees
// with the lines it received.
func secretsAnswer(names, values []string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "OK count=%d\n", len(names))
	for i := range names {
		b.WriteString(names[i])
		b.WriteByte(' ')
		b.WriteString(values[i])
		b.WriteByte('\n')
	}
	b.WriteString("END\n")
	return b.String()
}
