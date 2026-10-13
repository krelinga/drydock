package auth

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"
)

var tokenShape = regexp.MustCompile(`^drydock_[A-Za-z0-9_-]{43}$`)

// TestTokenIsStoredOnlyAsItsHash: a minted token has §4.1's shape, and the
// database file holds its SHA-256 and never the token (the control: the
// hash is found, so the search is reading the right bytes). Its metadata
// carries neither.
func TestTokenIsStoredOnlyAsItsHash(t *testing.T) {
	r := newRig(t)
	v, tok, err := r.svc.Tokens.Create(context.Background(), "  VS Code on my laptop ", []Scope{ScopeApprove, ScopeRead}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !tokenShape.MatchString(v) || !WellFormedToken(v) {
		t.Errorf("token %q is not drydock_ + 43 base64url characters", v)
	}
	if tok.Label != "VS Code on my laptop" || strings.Join(scopeNames(tok.Scopes), ",") != "read,approve" ||
		!tok.ExpiresAt.Equal(t0.Add(DefaultTokenLifetime)) || tok.LastUsedAt != nil || tok.RevokedAt != nil {
		t.Errorf("metadata = %+v", tok)
	}
	r.db.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`)
	raw, err := os.ReadFile(r.dir + "/drydock.db")
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte(v))
	if !strings.Contains(string(raw), hex.EncodeToString(sum[:])) {
		t.Fatal("control: the token's hash is not in the database file")
	}
	if strings.Contains(string(raw), v) || strings.Contains(string(raw), v[len(TokenPrefix):]) {
		t.Error("the database file holds the token")
	}
}

// TestTokenLookup: a live token resolves; a malformed, unknown, revoked or
// expired one is ErrNoToken alike. last_used_at moves at most once a minute.
func TestTokenLookup(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	v, tok, err := r.svc.Tokens.Create(ctx, "laptop", []Scope{ScopeRead}, 2*24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	got, err := r.svc.Tokens.Lookup(ctx, v)
	if err != nil || got.ID != tok.ID || got.LastUsedAt == nil || !got.LastUsedAt.Equal(t0) {
		t.Fatalf("control: lookup = %+v, %v", got, err)
	}
	used := func() string {
		var s string
		r.db.QueryRow(`SELECT last_used_at FROM api_token WHERE id = ?`, tok.ID).Scan(&s)
		return s
	}
	first := used()
	r.clock.Advance(30 * time.Second)
	r.svc.Tokens.Lookup(ctx, v)
	if used() != first {
		t.Error("last_used_at moved within a minute")
	}
	r.clock.Advance(31 * time.Second)
	r.svc.Tokens.Lookup(ctx, v)
	if used() == first {
		t.Error("last_used_at did not move after a minute")
	}
	if alive, _ := r.svc.Tokens.Alive(ctx, tok.ID); !alive {
		t.Error("control: Alive says a live token is not")
	}

	for name, bad := range map[string]string{
		"empty": "", "no prefix": v[len(TokenPrefix):], "short": v[:len(v)-1], "long": v + "A",
		"bad character": v[:len(v)-1] + "!", "unknown": TokenPrefix + strings.Repeat("A", 43),
	} {
		if _, err := r.svc.Tokens.Lookup(ctx, bad); !errors.Is(err, ErrNoToken) {
			t.Errorf("%s: %v; want ErrNoToken", name, err)
		}
	}
	r.clock.Advance(2 * 24 * time.Hour)
	if _, err := r.svc.Tokens.Lookup(ctx, v); !errors.Is(err, ErrNoToken) {
		t.Errorf("expired: %v; want ErrNoToken", err)
	}
	if alive, _ := r.svc.Tokens.Alive(ctx, tok.ID); alive {
		t.Error("Alive says an expired token is live")
	}
}

// TestTokenRevokeAndList: revoking writes revoked_at with api_token.revoked
// in one commit, a second revoke is ErrNoToken and writes nothing, and the
// list keeps the revoked row (Settings says when). The event's data is
// metadata alone.
func TestTokenRevokeAndList(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	v, tok, _ := r.svc.Tokens.Create(ctx, "laptop", []Scope{ScopeRead}, 0)
	r.clock.Advance(time.Hour)
	other, _, _ := r.svc.Tokens.Create(ctx, "desktop", []Scope{ScopeRead, ScopeLifecycle}, 0)
	got, err := r.svc.Tokens.Revoke(ctx, tok.ID)
	if err != nil || got.RevokedAt == nil {
		t.Fatalf("revoke = %+v, %v", got, err)
	}
	if _, err := r.svc.Tokens.Revoke(ctx, tok.ID); !errors.Is(err, ErrNoToken) {
		t.Errorf("a second revoke = %v; want ErrNoToken", err)
	}
	if _, err := r.svc.Tokens.Revoke(ctx, "tok_nope"); !errors.Is(err, ErrNoToken) {
		t.Errorf("an unknown id = %v; want ErrNoToken", err)
	}
	if _, err := r.svc.Tokens.Lookup(ctx, v); !errors.Is(err, ErrNoToken) {
		t.Errorf("a revoked token looked up: %v", err)
	}
	if _, err := r.svc.Tokens.Lookup(ctx, other); err != nil {
		t.Errorf("control: the other token: %v", err)
	}
	list, _ := r.svc.Tokens.List(ctx)
	if len(list) != 2 || list[0].Label != "desktop" || list[1].RevokedAt == nil {
		t.Errorf("list = %+v", list)
	}
	var kinds []string
	rows, _ := r.db.Query(`SELECT kind, coalesce(data,'') FROM event ORDER BY id`)
	for rows.Next() {
		var k, d string
		rows.Scan(&k, &d)
		kinds = append(kinds, k)
		if strings.Contains(d, v) || strings.Contains(d, other) || strings.Contains(d, "hash") {
			t.Errorf("event %s data %s carries a token", k, d)
		}
	}
	rows.Close()
	if strings.Join(kinds, ",") != "api_token.created,api_token.created,api_token.revoked" {
		t.Errorf("events = %v", kinds)
	}
}

// TestTokenMintRefusals: a label, scope set or lifetime outside the rules
// writes nothing; the controls are the boundaries that are allowed.
func TestTokenMintRefusals(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	for name, c := range map[string]struct {
		label  string
		scopes []Scope
		life   time.Duration
		want   error
	}{
		"blank label":       {"   ", []Scope{ScopeRead}, 0, ErrTokenLabel},
		"long label":        {strings.Repeat("é", 101), []Scope{ScopeRead}, 0, ErrTokenLabel},
		"control character": {"a\nb", []Scope{ScopeRead}, 0, ErrTokenLabel},
		"format character":  {"a‮b", []Scope{ScopeRead}, 0, ErrTokenLabel},
		"no scopes":         {"x", nil, 0, ErrTokenScopes},
		"lifecycle only":    {"x", []Scope{ScopeLifecycle}, 0, ErrTokenScopes},
		"approve only":      {"x", []Scope{ScopeApprove}, 0, ErrTokenScopes},
		"no read":           {"x", []Scope{ScopeLifecycle, ScopeApprove}, 0, ErrTokenScopes},
		"session":           {"x", []Scope{ScopeSession}, 0, ErrTokenScopes},
		"self":              {"x", []Scope{ScopeSelf}, 0, ErrTokenScopes},
		"twice":             {"x", []Scope{ScopeRead, ScopeRead}, 0, ErrTokenScopes},
		"under a day":       {"x", []Scope{ScopeRead}, time.Hour, ErrTokenLifetime},
		"over a year":       {"x", []Scope{ScopeRead}, MaxTokenLifetime + time.Hour, ErrTokenLifetime},
	} {
		if _, _, err := r.svc.Tokens.Create(ctx, c.label, c.scopes, c.life); !errors.Is(err, c.want) {
			t.Errorf("%s: %v; want %v", name, err, c.want)
		}
	}
	var n int
	r.db.QueryRow(`SELECT count(*) FROM api_token`).Scan(&n)
	if n != 0 {
		t.Errorf("refused mints wrote %d rows", n)
	}
	for _, c := range []struct {
		label string
		life  time.Duration
	}{{strings.Repeat("é", 100), MaxTokenLifetime}, {"x", 24 * time.Hour}} {
		if _, _, err := r.svc.Tokens.Create(ctx, c.label, GrantableScopes, c.life); err != nil {
			t.Errorf("control: %q for %v: %v", c.label, c.life, err)
		}
	}
}

// TestSetPasswordRevokesEveryToken: `drydock passwd` ends every credential
// minted under the old password, tokens as well as sessions. The control:
// the token worked before.
func TestSetPasswordRevokesEveryToken(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	v, _, _ := r.svc.Tokens.Create(ctx, "laptop", []Scope{ScopeRead}, 0)
	if _, err := r.svc.Tokens.Lookup(ctx, v); err != nil {
		t.Fatalf("control: %v", err)
	}
	if err := r.svc.SetPassword(ctx, "a brand new passphrase"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.svc.Tokens.Lookup(ctx, v); !errors.Is(err, ErrNoToken) {
		t.Errorf("after passwd the token = %v; want ErrNoToken", err)
	}
}

// TestCredentialScopes: a cookie session holds every scope but self, a
// token its own and self, and nobody the empty scope.
func TestCredentialScopes(t *testing.T) {
	s := SessionCredential(Session{ID: "h"})
	tok := TokenCredential(Token{ID: "tok_x", Scopes: []Scope{ScopeRead}})
	for _, c := range []struct {
		c    Credential
		s    Scope
		want bool
	}{
		{s, ScopeRead, true}, {s, ScopeLifecycle, true}, {s, ScopeApprove, true}, {s, ScopeSession, true}, {s, ScopeSelf, false}, {s, "", false},
		{tok, ScopeRead, true}, {tok, ScopeSelf, true}, {tok, ScopeLifecycle, false}, {tok, ScopeApprove, false}, {tok, ScopeSession, false}, {tok, "", false},
	} {
		if got := c.c.Has(c.s); got != c.want {
			t.Errorf("%s has %q = %v; want %v", c.c.Kind, c.s, got, c.want)
		}
	}
}

func scopeNames(s []Scope) []string {
	out := make([]string, len(s))
	for i, x := range s {
		out[i] = string(x)
	}
	return out
}
