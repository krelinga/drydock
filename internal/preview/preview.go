// Package preview is the preview handshake's state (port forwarding §5, §7;
// §13 step 2): which preview hosts exist, the one-time tokens that carry a
// signed-in device from the UI's origin to a preview host, and the host-only
// preview sessions those tokens become.
//
// The HTTP side — the routes, the gate's order, the redirects — is
// internal/api's. This package holds what those decide on, and nothing in it
// writes a response.
//
// Three properties are the point, and each has its test:
//
//   - **A token is single-use, short-lived and bound.** It lives in memory
//     only, is spent by the first presentation whatever that presentation's
//     outcome (an atomic compare-and-delete under one lock), expires after
//     TokenTTL, and is good for one preview host and one auth session. A
//     restart forgets every pending token, which fails the handshake closed.
//   - **A preview session is a credential stored as its hash.** The cookie
//     value exists only in the browser and in the response that set it;
//     preview_session.id is its SHA-256, as auth_session.id is the session
//     cookie's (design §4).
//   - **A preview session dies with what made it.** Its auth session (a
//     cascade, so revoke-all reaches every preview), its port's enable (a
//     disable or a retire deletes the rows), and its own idle window.
//
// # Rules and details
//
// Tokens are keyed by SHA-256, each bound to an auth session, one preview
// host, one port and the landing path, 60 s, at most 64 pending per auth
// session and 4,096 in all; Consume is one lookup-and-delete under one lock
// that spends the token whatever the outcome. preview_session rows are keyed
// by the cookie's SHA-256, validated per request against the Host, a 12-hour
// idle window, the auth session's own expiry, an enabled unretired port and a
// running workspace; SetEnabled(false), Retire and RetireWorkspacePorts (from
// workspace.Remove) delete a port's sessions in the same transaction. Slug
// (one label, never drydock-check) and ParseReturn (an https URL on exactly
// one preview host, or nothing) are the host rules.
//
// StripCookie removes the preview cookie and the UI's __Host-drydock too.
// CookieGuard filters Set-Cookie before every header block, 1xx included (a
// 103 through httputil.ReverseProxy is tested), and latches only at the final
// one.
//
// The Upstream seam's implementation is Proxy (PF §13 step 3): an
// httputil.ReverseProxy per request over one transport whose only dial
// resolves the container through a Resolver (internal/container's Address,
// then Confirm after the connect) — the URL host is a dial key of workspace
// and port, never an address; nothing remembers one, though a kept-alive
// connection is reused (it is bound to its container). Host is
// localhost:<port> unless the port is passthrough; every incoming
// Forwarded/X-Forwarded-*/X-Real-Ip is dropped and the proxy sets
// X-Forwarded-Proto: https, X-Forwarded-Host (the preview host) and
// X-Forwarded-For (Caddy's last entry, only if it is an address); the path and
// raw query go upstream as sent (decided: nothing past /.drydock/ is Drydock's
// to clean). A 101's Set-Cookie is filtered in ModifyResponse — ReverseProxy
// writes it past the CookieGuard.
//
// Upgrades are tracked and closed: after IdleTimeout with no byte either way
// (on the injected clock); when the gate's WithRecheck says their preview
// session no longer holds (every RecheckEvery, 30 s — Vite pings, so idleness
// never ends a revoked tab); at once by CloseWhere on a sign-out
// (api.SessionRoutes.Revoked); and by Close at shutdown. The fallback's
// /.drydock/ check reads the path cleaned (reservedPath). Not running →
// /.drydock/denied; refused 502, timed out 504, Docker unreachable 503, each
// Drydock's page naming the port only. Limit is §10.7's cap, outside the front
// door so the handshake's redirects count, and an open websocket for its life.
//
// Migration 8's forwarded_port has **no foreign key to workspace**: a cascade
// would delete a row and free its slug for reissue.
//
// The port registry (PF §13 step 4, registry.go) is this Service too: Ports,
// Port, Add, Update/SetEnabled, Retire and DeclarePorts, each one transaction
// with its one port.* event (events.Log.Commit), the whole row as data.port
// (port.retired: port_id). A row is born disabled and only Update writes the
// switch — DeclarePorts, fed by step 3's container.DeclaredPorts, never does.
// A disable or a retire deletes the port's preview sessions in its transaction
// and then tells Revoked, which the server wires to
// Proxy.CloseWhere(PortID == id): open websockets close at once. MintSlug is
// <repo>-<port>-<4>, drawn again under a savepoint when the UNIQUE on slug —
// retired rows included — refuses it, never reused. Spent names a switched-off
// or retired slug, for authorize's Clear grant: that host's /.drydock/session
// answers it with Clear-Site-Data (PF §10.3). Proxy.Probe is the proxy's own
// dial, its outcomes and sentences (OutcomeSentence) the failure pages'.
package preview

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/krelinga/drydock/internal/events"
	"github.com/krelinga/drydock/internal/sys"
)

// CookieName is the preview cookie's one name, everywhere: what the session
// handler sets, what the front door reads, and what the proxy strips on the
// way to the container (PF §7's warning box).
//
// The __Host- prefix is not decoration. Two preview hosts are same-site with
// each other (PF §10.4), so without it a hostile preview could set
// `Domain=<preview-domain>` and toss a cookie of this name at every other
// preview — not a credential (sessions are host-bound), but a way to wedge
// another preview's handshake. A browser refuses a __Host- cookie that names a
// Domain, so that toss is impossible.
const CookieName = "__Host-drydock-preview"

// TokenTTL is the one-time token's life (PF §7 step 4).
const TokenTTL = 60 * time.Second

// IdleLifetime is the preview session's own idle window, deliberately much
// shorter than the auth session's 14 days (PF §5): a captured preview cookie
// should not stay live for long, and running the handshake again costs a
// signed-in device nothing it can see.
const IdleLifetime = 12 * time.Hour

// touchEvery bounds how often a request writes last_seen_at: a dev server's
// page is dozens of requests, and none of them needs its own write.
const touchEvery = time.Minute

// maxPending bounds the in-memory token table. Only a signed-in device can
// mint, but a loop that never consumes must not grow memory without end.
const maxPending = 4096

// maxPendingPerSession bounds one auth session's share of it, so one tab
// looping on /preview/authorize refuses only its own device, never every
// other device's preview. A real handshake has one token pending per
// preview host being opened; 64 is far above any honest burst.
const maxPendingPerSession = 64

// ReservedLabel is the name the installer's final check asks for. It must
// never be a slug, or that check could someday land on a real preview; the
// schema refuses it too.
const ReservedLabel = "drydock-check"

// Paths the preview mux keeps for itself (PF §6). Everything else under a
// preview host is the previewed app's.
const (
	ReservedPrefix = "/.drydock/"
	SessionPath    = "/.drydock/session"
	DeniedPath     = "/.drydock/denied"
)

// Slug reports the preview slug a Host names under domain: exactly one DNS
// label, lowercase letters, digits and inner hyphens, and never the reserved
// probe name. A port on the Host is ignored, as the API's own Host check
// ignores one. Anything else — another domain, two labels, the bare domain, no
// domain configured — is not a preview host.
func Slug(host, domain string) (string, bool) {
	if domain == "" {
		return "", false
	}
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	host = strings.ToLower(host)
	label, ok := strings.CutSuffix(host, "."+domain)
	if !ok || !validLabel(label) || label == ReservedLabel {
		return "", false
	}
	return label, true
}

func validLabel(l string) bool {
	if l == "" || len(l) > 63 || l[0] == '-' || l[len(l)-1] == '-' {
		return false
	}
	for _, c := range l {
		if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-') {
			return false
		}
	}
	return true
}

// Target is what a preview host resolves to: an enabled, unretired port on a
// running workspace. It carries no address — the upstream is derived from the
// workspace's container at dial time, never stored (PF §5, §8.1).
type Target struct {
	PortID         string
	WorkspaceID    string
	ContainerPort  int
	Slug           string
	Host           string // <slug>.<domain>
	UpstreamScheme string // http | https
	HostHeader     string // localhost | passthrough (PF §8.3)
	// AuthSessionID is the auth session the preview session was minted
	// under, when the Target came from a preview cookie (Session): what the
	// proxy closes an open websocket by when that session is revoked.
	AuthSessionID string
}

// Grant is what a one-time token is good for: one auth session, one preview
// host, one port, and the path to land on once the cookie is set.
type Grant struct {
	AuthSessionID string
	Host          string
	PortID        string
	// Path is the request URI to land on (path and query), from the
	// validated `return`. It never leaves the server between mint and
	// consume, so nothing in the token URL can change where the browser
	// lands.
	Path string
	// Clear marks a grant that is never a session: authorize mints one for
	// a slug whose port was disabled or retired, so the preview host's own
	// /.drydock/session can answer it with Clear-Site-Data (PF §10.3) — the
	// one response on that origin Drydock can make for a signed-in device
	// that is not an oracle to anyone else.
	Clear bool
}

// ErrNotPreviewable is a slug that resolves to no enabled port on a running
// workspace — absent, disabled, retired, or its workspace not running. The
// caller sends it to the denied page and says none of which.
var ErrNotPreviewable = errors.New("no enabled port on a running workspace")

// ErrTooManyPending refuses a mint while the token table is full.
var ErrTooManyPending = errors.New("too many preview sign-ins pending")

// Service is the handshake's state: the database half and the in-memory
// tokens.
type Service struct {
	DB     *sql.DB
	Clock  sys.Clock
	Random io.Reader
	// Domain is the preview domain; empty means previews are off and no
	// Host is a preview host.
	Domain string
	// AuthIdle is the auth session's idle lifetime, which a preview session
	// also dies by: a device the UI has forgotten keeps no preview. It is
	// auth.IdleLifetime in production (a parameter so this package does not
	// import auth).
	AuthIdle time.Duration
	// Events is where the port registry's changes are written, each in the
	// same transaction as the row it describes (events.Log.Commit). Nil
	// writes the rows alone, for a test of the handshake.
	Events *events.Log
	// Revoked is told, after the commit, of every port a disable or a
	// retire has just ended the preview sessions of — the server closes
	// that port's open websockets with it (Proxy.CloseWhere), so a disable
	// ends a live HMR socket at once rather than at its next recheck (PF
	// §13.4). Nil tells no one.
	Revoked func(portID string)

	// beforeInsert, in a test, runs between StartSession's checks and its
	// insert: where a disable can land (export_test.go).
	beforeInsert func()

	mu      sync.Mutex
	pending map[string]pendingToken // keyed by the token's SHA-256
}

type pendingToken struct {
	grant   Grant
	expires time.Time
}

// Slug is Slug under the service's domain.
func (s *Service) Slug(host string) (string, bool) { return Slug(host, s.Domain) }

// HostFor is the preview host a slug is served on.
func (s *Service) HostFor(slug string) string { return slug + "." + s.Domain }

// Mint makes a one-time token for g. The token is returned once and kept only
// as its hash.
func (s *Service) Mint(g Grant) (string, error) {
	raw := make([]byte, 32)
	if _, err := io.ReadFull(s.Random, raw); err != nil {
		return "", fmt.Errorf("preview token: %w", err)
	}
	tok := base64.RawURLEncoding.EncodeToString(raw)
	now := s.Clock.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.pending == nil {
		s.pending = map[string]pendingToken{}
	}
	for k, p := range s.pending {
		if !now.Before(p.expires) {
			delete(s.pending, k)
		}
	}
	if len(s.pending) >= maxPending {
		return "", ErrTooManyPending
	}
	mine := 0
	for _, p := range s.pending {
		if p.grant.AuthSessionID == g.AuthSessionID {
			mine++
		}
	}
	if mine >= maxPendingPerSession {
		return "", ErrTooManyPending
	}
	s.pending[hash(tok)] = pendingToken{grant: g, expires: now.Add(TokenTTL)}
	return tok, nil
}

// Consume spends a token presented on host. The lookup and the delete are one
// step under one lock, so of two racing presentations exactly one can win;
// and the delete happens whatever the outcome, so a token shown on the wrong
// host, or late, is spent too and cannot be tried again. A token that is
// unknown, spent, expired, or for another host is false — and the caller
// answers all four alike.
func (s *Service) Consume(token, host string) (Grant, bool) {
	if token == "" {
		return Grant{}, false
	}
	k := hash(token)
	now := s.Clock.Now()
	s.mu.Lock()
	p, ok := s.pending[k]
	delete(s.pending, k)
	s.mu.Unlock()
	if !ok || !now.Before(p.expires) {
		return Grant{}, false
	}
	if subtle.ConstantTimeCompare([]byte(strings.ToLower(hostOnly(host))), []byte(p.grant.Host)) != 1 {
		return Grant{}, false
	}
	return p.grant, true
}

// Pending is how many tokens are outstanding, for tests.
func (s *Service) Pending() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.pending)
}

const targetCols = `fp.id, fp.workspace_id, fp.container_port, fp.slug, fp.upstream_scheme, fp.host_header`

const previewable = `fp.enabled = 1 AND fp.retired_at IS NULL AND w.state = 'running'`

// Resolve finds the enabled port a slug names on a running workspace.
func (s *Service) Resolve(ctx context.Context, slug string) (Target, error) {
	row := s.DB.QueryRowContext(ctx, `SELECT `+targetCols+`
		FROM forwarded_port fp JOIN workspace w ON w.id = fp.workspace_id
		WHERE fp.slug = ? AND `+previewable, slug)
	t, err := s.scanTarget(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Target{}, ErrNotPreviewable
	}
	return t, err
}

func (s *Service) scanTarget(r interface{ Scan(...any) error }, extra ...any) (Target, error) {
	var t Target
	err := r.Scan(append([]any{&t.PortID, &t.WorkspaceID, &t.ContainerPort, &t.Slug, &t.UpstreamScheme, &t.HostHeader}, extra...)...)
	t.Host = s.HostFor(t.Slug)
	return t, err
}

// ErrSessionGone is a grant whose auth session ended between mint and
// consume — revoked, signed out or expired — or whose port did. The caller
// denies it like any other refusal.
var ErrSessionGone = errors.New("the session that minted this grant is gone")

// StartSession writes the preview session a consumed grant becomes and
// returns the cookie value to set. The value is never stored.
func (s *Service) StartSession(ctx context.Context, g Grant) (string, Target, error) {
	slug, ok := s.Slug(g.Host)
	if !ok || g.Clear {
		return "", Target{}, ErrNotPreviewable
	}
	t, err := s.Resolve(ctx, slug)
	if err != nil {
		return "", Target{}, err
	}
	if t.PortID != g.PortID {
		return "", Target{}, ErrNotPreviewable
	}
	if alive, err := s.authAlive(ctx, g.AuthSessionID); err != nil {
		return "", Target{}, err
	} else if !alive {
		return "", Target{}, ErrSessionGone
	}
	raw := make([]byte, 32)
	if _, err := io.ReadFull(s.Random, raw); err != nil {
		return "", Target{}, fmt.Errorf("preview cookie: %w", err)
	}
	cookie := base64.RawURLEncoding.EncodeToString(raw)
	now := ts(s.Clock.Now())
	if s.beforeInsert != nil {
		s.beforeInsert()
	}
	// One statement that inserts only while the port is still enabled,
	// unretired and on a running workspace: a disable or retire committed
	// since the Resolve above leaves no session, rather than one its own
	// DELETE has already run past. The foreign key refuses a row for an auth
	// session deleted since the check above, so a revoke that races this
	// insert still wins too.
	res, err := s.DB.ExecContext(ctx, `INSERT INTO preview_session
		(id, auth_session_id, forwarded_port_id, preview_host, created_at, last_seen_at)
		SELECT ?, ?, fp.id, ?, ?, ? FROM forwarded_port fp JOIN workspace w ON w.id = fp.workspace_id
		WHERE fp.id = ? AND `+previewable, hash(cookie), g.AuthSessionID, g.Host, now, now, t.PortID)
	if err != nil {
		if isConstraint(err) {
			return "", Target{}, ErrSessionGone
		}
		return "", Target{}, fmt.Errorf("store preview session: %w", err)
	}
	if n, err := res.RowsAffected(); err != nil || n != 1 {
		return "", Target{}, ErrNotPreviewable
	}
	return cookie, t, nil
}

// Session validates a preview cookie presented on host and resolves what it
// may reach. A stopped workspace's sessions are refused here, not deleted: a
// stop is not a revocation (the device would mint a new one on its next
// request anyway), so after a start within the idle window the same cookie
// works again (PF §13.2). Disable, retire, removal and sign-out delete. It is false — and the caller answers alike — for a cookie that
// is unknown, for another host, idle past IdleLifetime, whose auth session has
// ended, whose port is disabled or retired, or whose workspace is not
// running. A live one slides its idle window.
func (s *Service) Session(ctx context.Context, cookie, host string) (Target, bool) {
	if cookie == "" {
		return Target{}, false
	}
	id := hash(cookie)
	var previewHost, seen, authSeen, authExpires, authID string
	row := s.DB.QueryRowContext(ctx, `SELECT `+targetCols+`, ps.preview_host, ps.last_seen_at, a.last_seen_at, a.absolute_expires_at, a.id
		FROM preview_session ps
		JOIN auth_session a ON a.id = ps.auth_session_id
		JOIN forwarded_port fp ON fp.id = ps.forwarded_port_id
		JOIN workspace w ON w.id = fp.workspace_id
		WHERE ps.id = ? AND `+previewable, id)
	t, err := s.scanTarget(row, &previewHost, &seen, &authSeen, &authExpires, &authID)
	if err != nil {
		return Target{}, false
	}
	t.AuthSessionID = authID
	if subtle.ConstantTimeCompare([]byte(strings.ToLower(hostOnly(host))), []byte(previewHost)) != 1 || previewHost != t.Host {
		return Target{}, false
	}
	now := s.Clock.Now().UTC()
	last, err1 := parseTS(seen)
	aSeen, err2 := parseTS(authSeen)
	aExp, err3 := parseTS(authExpires)
	if err1 != nil || err2 != nil || err3 != nil {
		return Target{}, false
	}
	if !now.Before(last.Add(IdleLifetime)) {
		_, _ = s.DB.ExecContext(ctx, `DELETE FROM preview_session WHERE id = ?`, id)
		return Target{}, false
	}
	if !now.Before(aExp) || (s.AuthIdle > 0 && !now.Before(aSeen.Add(s.AuthIdle))) {
		return Target{}, false
	}
	if now.Sub(last) >= touchEvery {
		_, _ = s.DB.ExecContext(ctx, `UPDATE preview_session SET last_seen_at = ? WHERE id = ?`, ts(now), id)
	}
	return t, true
}

func (s *Service) authAlive(ctx context.Context, id string) (bool, error) {
	var seen, expires string
	err := s.DB.QueryRowContext(ctx, `SELECT last_seen_at, absolute_expires_at FROM auth_session WHERE id = ?`, id).Scan(&seen, &expires)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	a, err1 := parseTS(seen)
	e, err2 := parseTS(expires)
	if err1 != nil || err2 != nil {
		return false, nil
	}
	now := s.Clock.Now().UTC()
	return now.Before(e) && (s.AuthIdle <= 0 || now.Before(a.Add(s.AuthIdle))), nil
}

// RetireWorkspacePorts is what removing a workspace does to its ports, inside
// the removal's own transaction: every live row retired, its sessions gone.
// forwarded_port has no foreign key to workspace for exactly this reason — a
// cascade would delete the rows and free their slugs.
func RetireWorkspacePorts(ctx context.Context, tx *sql.Tx, workspaceID string, now time.Time) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM preview_session WHERE forwarded_port_id IN
		(SELECT id FROM forwarded_port WHERE workspace_id = ?)`, workspaceID); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `UPDATE forwarded_port SET enabled = 0, retired_at = ?
		WHERE workspace_id = ? AND retired_at IS NULL`, ts(now), workspaceID)
	return err
}

func hostOnly(host string) string {
	if h, _, err := net.SplitHostPort(host); err == nil {
		return h
	}
	return host
}

// hash is the only transformation between a token or cookie and what is kept.
func hash(v string) string {
	sum := sha256.Sum256([]byte(v))
	return hex.EncodeToString(sum[:])
}

func ts(t time.Time) string               { return t.UTC().Format(time.RFC3339Nano) }
func parseTS(s string) (time.Time, error) { return time.Parse(time.RFC3339Nano, s) }

func isConstraint(err error) bool {
	return err != nil && strings.Contains(strings.ToLower(err.Error()), "constraint")
}
