package broker

import (
	"context"
	"net/http"
	"strings"
	"testing"
)

// TestRateLimitIsSaidAsGitHubRefusing is design §12's *GitHub rate limit or
// App suspended*: a token already issued keeps being served from the cache
// while GitHub refuses (the positive control — no request reaches GitHub for
// it), and a workspace with none gets a token.refused whose sentence says
// GitHub is refusing requests and offers no retry. Each other reason gets its
// own sentence, never this one.
func TestRateLimitIsSaidAsGitHubRefusing(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	first := tokenOf(t, e.ask(t, wsA, "GET-TOKEN scope=git"))
	e.fake.Fail = func(r *http.Request) (int, string) {
		if r.Method == "POST" {
			return 403, "API rate limit exceeded for installation"
		}
		return 0, ""
	}
	posts := e.fake.Count("POST")
	if got := tokenOf(t, e.ask(t, wsA, "GET-TOKEN scope=git")); got != first || e.fake.Count("POST") != posts {
		t.Error("a cached token was not served while GitHub refuses")
	}
	if got := e.ask(t, wsA, "GET-TOKEN scope=gh"); got != "ERR reason=rate_limited" {
		t.Fatalf("gh, uncached: %q", got)
	}
	var msg string
	if err := e.db.QueryRowContext(ctx, `SELECT message FROM event WHERE kind = 'token.refused' AND workspace_id = ?`, wsA).
		Scan(&msg); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(msg, "GitHub is refusing requests:") || !strings.Contains(msg, "rate limit is spent or the App is suspended") ||
		strings.Contains(strings.ToLower(msg), "try again") || strings.Contains(msg, "API rate limit exceeded") {
		t.Errorf("rate-limited sentence: %q", msg)
	}
	for _, r := range []string{ReasonPermissionMissing, ReasonRevoked, ReasonUnavailable} {
		if s := RefusedSentence(ScopeGit, r); strings.Contains(s, "refusing requests") {
			t.Errorf("%s says %q", r, s)
		}
	}
	if s := RefusedSentence(ScopeGit, ReasonRevoked); !strings.Contains(s, "read-only") || !strings.Contains(s, "Unpushed work") {
		t.Errorf("revoked: %q", s)
	}
}
