package auth

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/krelinga/drydock/internal/events"
	"github.com/krelinga/drydock/internal/sys"
)

// Scope is what a credential may do (vscode-extension §4.1). Every route
// declares one; a credential reaches a route only when it holds that route's
// scope.
type Scope string

const (
	// ScopeRead: every GET that is not session-scoped, and the stream.
	ScopeRead Scope = "read"
	// ScopeLifecycle: clone, start, stop, rebuild, delete, restart the
	// session server, refresh the catalog, check or begin the Claude login,
	// and the ports.
	ScopeLifecycle Scope = "lifecycle"
	// ScopeApprove: approve or decline a host-access request. Granted
	// deliberately, default off.
	ScopeApprove Scope = "approve"
	// ScopeSelf: a token revoking itself. Every token holds it; a cookie
	// session does not (it signs out through its own route).
	ScopeSelf Scope = "self"
	// ScopeSession: the operator's own browser session and nothing else —
	// secrets, the token routes, the device list, signing out. No token can
	// hold it.
	ScopeSession Scope = "session"
)

// GrantableScopes are the scopes a token may be minted with, in display
// order.
var GrantableScopes = []Scope{ScopeRead, ScopeLifecycle, ScopeApprove}

// CredentialKind is what authenticated a request.
type CredentialKind string

const (
	CredentialSession CredentialKind = "session"
	CredentialToken   CredentialKind = "token"
)

// Credential is what the gate attaches to an authenticated request: a cookie
// session, which is the operator and holds every scope but self, or an API
// token, which holds its own scopes and self. It carries no secret: ID is
// the session's SHA-256 or the token's random id.
type Credential struct {
	Kind      CredentialKind
	ID        string
	Label     string
	Scopes    []Scope
	ExpiresAt time.Time
}

// sessionScopes is a cookie session's: everything but self.
var sessionScopes = []Scope{ScopeRead, ScopeLifecycle, ScopeApprove, ScopeSession}

// SessionCredential is the credential a cookie session is.
func SessionCredential(s Session) Credential {
	return Credential{Kind: CredentialSession, ID: s.ID, Label: s.Label,
		Scopes: append([]Scope(nil), sessionScopes...), ExpiresAt: s.ExpiresAt}
}

// TokenCredential is the credential an API token is.
func TokenCredential(t Token) Credential {
	return Credential{Kind: CredentialToken, ID: t.ID, Label: t.Label,
		Scopes: append(append([]Scope(nil), t.Scopes...), ScopeSelf), ExpiresAt: t.ExpiresAt}
}

// Has reports whether the credential holds s. An empty scope is held by
// nobody: a route that declares none is closed to every credential that
// asks by scope (the gate treats it as session, so the operator still
// reaches it, and no token does).
func (c Credential) Has(s Scope) bool {
	if s == "" {
		return false
	}
	for _, h := range c.Scopes {
		if h == s {
			return true
		}
	}
	return false
}

// Token format (vscode-extension §4.1): the prefix and 43 base64url
// characters, 32 random bytes. The prefix makes one recognisable to a secret
// scanner, to the canary sweep and to an operator who pastes it in the wrong
// place.
const (
	TokenPrefix = "drydock_"
	tokenChars  = 43
)

// Lifetimes (§4.1): chosen at mint, 90 days unless asked otherwise, never
// more than a year, and no idle expiry — a laptop unused for a fortnight
// keeps its token.
const (
	DefaultTokenLifetime = 90 * 24 * time.Hour
	MaxTokenLifetime     = 365 * 24 * time.Hour
	// tokenTouchEvery bounds last_used_at's writes to one a minute per token,
	// so a busy client does not turn every request into a write (§4.3).
	tokenTouchEvery = time.Minute
	// MaxTokenLabel is the longest label, in characters.
	MaxTokenLabel = 100
)

// Token is one API token's metadata: never the token, never its hash.
type Token struct {
	ID         string     `json:"id"`
	Label      string     `json:"label"`
	Scopes     []Scope    `json:"scopes"`
	CreatedAt  time.Time  `json:"created_at"`
	LastUsedAt *time.Time `json:"last_used_at"`
	ExpiresAt  time.Time  `json:"expires_at"`
	RevokedAt  *time.Time `json:"revoked_at"`
}

// Errors the token store returns.
var (
	// ErrNoToken: no live token by that value or id — absent, revoked or
	// expired, alike. To a request it is the same 401 as a bad cookie.
	ErrNoToken = errors.New("no valid API token")
	// ErrTokenLabel, ErrTokenScopes, ErrTokenLifetime: a mint refused
	// before anything is written.
	ErrTokenLabel    = errors.New("a token needs a label of 1 to 100 printable characters")
	ErrTokenScopes   = errors.New("a token needs one or more of read, lifecycle and approve, each once")
	ErrTokenLifetime = errors.New("a token lasts from one day to a year")
)

// Tokens is the API token store. Create and Revoke write their row and its
// event as one fact (events.Commit); the bearer lookup on every request
// writes nothing but, at most once a minute, last_used_at.
type Tokens struct {
	DB     *sql.DB
	Clock  sys.Clock
	Random io.Reader
	// Events is the log Create and Revoke commit through. Nil refuses both.
	Events *events.Log
}

// WellFormedToken reports whether s has a token's shape: the prefix and 43
// base64url characters, nothing else. Checked before any lookup, so a
// malformed value costs no query.
func WellFormedToken(s string) bool {
	if !strings.HasPrefix(s, TokenPrefix) || len(s) != len(TokenPrefix)+tokenChars {
		return false
	}
	for _, c := range s[len(TokenPrefix):] {
		if !(c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-' || c == '_') {
			return false
		}
	}
	return true
}

// NormalizeScopes checks a mint's scopes: one or more of the grantable
// three, none twice, returned in GrantableScopes order.
func NormalizeScopes(in []Scope) ([]Scope, error) {
	seen := map[Scope]bool{}
	for _, s := range in {
		ok := false
		for _, g := range GrantableScopes {
			ok = ok || s == g
		}
		if !ok || seen[s] {
			return nil, ErrTokenScopes
		}
		seen[s] = true
	}
	var out []Scope
	for _, g := range GrantableScopes {
		if seen[g] {
			out = append(out, g)
		}
	}
	if len(out) == 0 {
		return nil, ErrTokenScopes
	}
	return out, nil
}

// normalizeLabel trims the label and refuses an empty, over-long or
// control-character one.
func normalizeLabel(label string) (string, error) {
	label = strings.TrimSpace(label)
	if label == "" || !utf8.ValidString(label) || utf8.RuneCountInString(label) > MaxTokenLabel {
		return "", ErrTokenLabel
	}
	for _, r := range label {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			return "", ErrTokenLabel
		}
	}
	return label, nil
}

// Create mints a token and returns its value, which exists only in this
// return and in the response that carries it: what is stored is its SHA-256.
// api_token.created carries the metadata alone.
func (t *Tokens) Create(ctx context.Context, label string, scopes []Scope, lifetime time.Duration) (string, Token, error) {
	if t.Events == nil {
		return "", Token{}, errors.New("api tokens: no event log")
	}
	label, err := normalizeLabel(label)
	if err != nil {
		return "", Token{}, err
	}
	scopes, err = NormalizeScopes(scopes)
	if err != nil {
		return "", Token{}, err
	}
	if lifetime == 0 {
		lifetime = DefaultTokenLifetime
	}
	if lifetime < 24*time.Hour || lifetime > MaxTokenLifetime {
		return "", Token{}, ErrTokenLifetime
	}
	raw := make([]byte, tokenBytes)
	if _, err := io.ReadFull(t.Random, raw); err != nil {
		return "", Token{}, fmt.Errorf("token: %w", err)
	}
	value := TokenPrefix + base64.RawURLEncoding.EncodeToString(raw)
	idRaw := make([]byte, 10)
	if _, err := io.ReadFull(t.Random, idRaw); err != nil {
		return "", Token{}, fmt.Errorf("token id: %w", err)
	}
	now := t.Clock.Now().UTC()
	tok := Token{ID: "tok_" + hex.EncodeToString(idRaw), Label: label, Scopes: scopes,
		CreatedAt: now, ExpiresAt: now.Add(lifetime)}
	_, err = t.Events.Commit(ctx, func(tx *sql.Tx) ([]events.Event, error) {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO api_token (id, token_hash, label, scope_read, scope_lifecycle, scope_approve, created_at, expires_at)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
			tok.ID, hashToken(value), tok.Label, b2i(has(scopes, ScopeRead)), b2i(has(scopes, ScopeLifecycle)),
			b2i(has(scopes, ScopeApprove)), ts(tok.CreatedAt), ts(tok.ExpiresAt)); err != nil {
			return nil, fmt.Errorf("store token: %w", err)
		}
		ev, err := events.NewEvent("", events.Info, events.KindAPITokenCreated,
			"An API token was created.", map[string]any{"token": tok})
		return []events.Event{ev}, err
	})
	if err != nil {
		return "", Token{}, err
	}
	return value, tok, nil
}

// Lookup resolves a bearer value to a live token: well-formed, known,
// unrevoked and unexpired, or ErrNoToken whichever it is not. It is never a
// sign-in attempt and never touches the lockout (§4.3): a stale token in a
// busy client must not lock the operator out of the web UI.
func (t *Tokens) Lookup(ctx context.Context, value string) (Token, error) {
	if !WellFormedToken(value) {
		return Token{}, ErrNoToken
	}
	tok, err := t.get(ctx, `token_hash = ?`, hashToken(value))
	if err != nil {
		return Token{}, err
	}
	now := t.Clock.Now().UTC()
	if !live(tok, now) {
		return Token{}, ErrNoToken
	}
	if tok.LastUsedAt == nil || !now.Before(tok.LastUsedAt.Add(tokenTouchEvery)) {
		if _, err := t.DB.ExecContext(ctx, `UPDATE api_token SET last_used_at = ? WHERE id = ?`, ts(now), tok.ID); err != nil {
			return Token{}, fmt.Errorf("touch token: %w", err)
		}
		tok.LastUsedAt = &now
	}
	return tok, nil
}

// Alive reports whether a token id still names a live token, writing
// nothing. The event stream asks it on every heartbeat, so a revoked or
// expired token's stream ends within one beat.
func (t *Tokens) Alive(ctx context.Context, id string) (bool, error) {
	tok, err := t.get(ctx, `id = ?`, id)
	if errors.Is(err, ErrNoToken) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return live(tok, t.Clock.Now().UTC()), nil
}

// List returns every token, live or not, newest first.
func (t *Tokens) List(ctx context.Context) ([]Token, error) {
	rows, err := t.DB.QueryContext(ctx, `SELECT `+tokenColumns+` FROM api_token ORDER BY created_at DESC, id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Token{}
	for rows.Next() {
		tok, err := scanToken(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, tok)
	}
	return out, rows.Err()
}

// Revoke sets a token's revoked_at, with api_token.revoked in the same
// commit. A token already revoked, or none by that id, is ErrNoToken and
// writes nothing. An expired token may still be revoked.
func (t *Tokens) Revoke(ctx context.Context, id string) (Token, error) {
	if t.Events == nil {
		return Token{}, errors.New("api tokens: no event log")
	}
	var tok Token
	_, err := t.Events.Commit(ctx, func(tx *sql.Tx) ([]events.Event, error) {
		now := t.Clock.Now().UTC()
		res, err := tx.ExecContext(ctx, `UPDATE api_token SET revoked_at = ? WHERE id = ? AND revoked_at IS NULL`, ts(now), id)
		if err != nil {
			return nil, err
		}
		if n, err := res.RowsAffected(); err != nil {
			return nil, err
		} else if n == 0 {
			return nil, ErrNoToken
		}
		if tok, err = scanToken(tx.QueryRowContext(ctx, `SELECT `+tokenColumns+` FROM api_token WHERE id = ?`, id)); err != nil {
			return nil, err
		}
		ev, err := events.NewEvent("", events.Info, events.KindAPITokenRevoked,
			"An API token was revoked.", map[string]any{"token": tok})
		return []events.Event{ev}, err
	})
	return tok, err
}

// RevokeAll revokes every live token, writing no event: it is `drydock
// passwd`'s, a process of its own with no stream to publish to, and a
// changed password must end every credential minted under the old one, as it
// ends every session. A running server's next lookup or heartbeat refuses
// them.
func (t *Tokens) RevokeAll(ctx context.Context) error {
	_, err := t.DB.ExecContext(ctx, `UPDATE api_token SET revoked_at = ? WHERE revoked_at IS NULL`, ts(t.Clock.Now().UTC()))
	return err
}

const tokenColumns = `id, label, scope_read, scope_lifecycle, scope_approve, created_at, last_used_at, expires_at, revoked_at`

func (t *Tokens) get(ctx context.Context, where string, arg string) (Token, error) {
	tok, err := scanToken(t.DB.QueryRowContext(ctx, `SELECT `+tokenColumns+` FROM api_token WHERE `+where, arg))
	if errors.Is(err, sql.ErrNoRows) {
		return Token{}, ErrNoToken
	}
	return tok, err
}

func scanToken(r scanner) (Token, error) {
	var tok Token
	var read, lifecycle, approve int
	var created, expires string
	var used, revoked sql.NullString
	if err := r.Scan(&tok.ID, &tok.Label, &read, &lifecycle, &approve, &created, &used, &expires, &revoked); err != nil {
		return Token{}, err
	}
	for _, s := range []struct {
		on    int
		scope Scope
	}{{read, ScopeRead}, {lifecycle, ScopeLifecycle}, {approve, ScopeApprove}} {
		if s.on == 1 {
			tok.Scopes = append(tok.Scopes, s.scope)
		}
	}
	var err error
	if tok.CreatedAt, err = parseTS(created); err != nil {
		return Token{}, err
	}
	if tok.ExpiresAt, err = parseTS(expires); err != nil {
		return Token{}, err
	}
	if used.Valid {
		u, err := parseTS(used.String)
		if err != nil {
			return Token{}, err
		}
		tok.LastUsedAt = &u
	}
	if revoked.Valid {
		r, err := parseTS(revoked.String)
		if err != nil {
			return Token{}, err
		}
		tok.RevokedAt = &r
	}
	return tok, nil
}

func live(tok Token, now time.Time) bool {
	return tok.RevokedAt == nil && now.Before(tok.ExpiresAt)
}

func has(scopes []Scope, s Scope) bool {
	for _, x := range scopes {
		if x == s {
			return true
		}
	}
	return false
}

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}
