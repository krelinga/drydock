package api

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/krelinga/drydock/internal/auth"
	"github.com/krelinga/drydock/internal/events"
	"github.com/krelinga/drydock/internal/store"
	"github.com/krelinga/drydock/internal/sys"
)

// bearerRig is the real gate over a real store: sessions, tokens, events.
type bearerRig struct {
	clock *sys.FakeClock
	svc   *auth.Service
	log   *events.Log
	gate  SessionGate
}

func newBearerRig(t *testing.T) *bearerRig {
	t.Helper()
	db, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "drydock.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	clock := sys.NewFakeClock(time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC))
	svc := auth.New(db.DB, sys.Env{Clock: clock, Random: rand.Reader})
	log := events.New(db.DB, clock)
	t.Cleanup(log.Close)
	svc.Tokens.Events = log
	return &bearerRig{clock: clock, svc: svc, log: log,
		gate: SessionGate{Sessions: svc.Sessions, Tokens: svc.Tokens, UIOrigin: "https://drydock.example.com", UIHost: "drydock.example.com"}}
}

func (b *bearerRig) mint(t *testing.T, scopes ...auth.Scope) (string, auth.Token) {
	t.Helper()
	v, tok, err := b.svc.Tokens.Create(context.Background(), "test laptop", scopes, 0)
	if err != nil {
		t.Fatal(err)
	}
	return v, tok
}

func (b *bearerRig) cookie(t *testing.T) string {
	t.Helper()
	c, _, err := b.svc.Sessions.Create(context.Background(), "Mac Firefox", "192.0.2.1")
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func apiReq(authz []string, cookie string) *http.Request {
	r := httptest.NewRequest("GET", "https://drydock.example.com/api/workspaces", nil)
	r.Host = "drydock.example.com"
	for _, a := range authz {
		r.Header.Add("Authorization", a)
	}
	if cookie != "" {
		r.AddCookie(&http.Cookie{Name: auth.CookieName, Value: cookie})
	}
	return r
}

// TestAuthenticateAcceptsOneCredential is §4.3 step 1: a cookie or a bearer,
// never both — the cookie with any Authorization header is refused whatever
// the header's scheme, as are two Authorization headers, a scheme that is not
// Bearer, a malformed, unknown, revoked or expired token. The controls: the
// token alone and the cookie alone each authenticate, as the credential they
// are.
func TestAuthenticateAcceptsOneCredential(t *testing.T) {
	b := newBearerRig(t)
	tok, meta := b.mint(t, auth.ScopeRead)
	cookie := b.cookie(t)
	revoked, rmeta := b.mint(t, auth.ScopeRead)
	if _, err := b.svc.Tokens.Revoke(context.Background(), rmeta.ID); err != nil {
		t.Fatal(err)
	}

	if r, ok := b.gate.Authenticate(apiReq([]string{"Bearer " + tok}, "")); !ok {
		t.Fatal("control: the token alone did not authenticate")
	} else if c, _ := CredentialFrom(r.Context()); c.Kind != auth.CredentialToken || c.ID != meta.ID || !c.Has(auth.ScopeRead) || !c.Has(auth.ScopeSelf) || c.Has(auth.ScopeLifecycle) {
		t.Errorf("the token's credential = %+v", c)
	} else if _, isSession := SessionFrom(r.Context()); isSession {
		t.Error("a token's request carries a session")
	}
	if r, ok := b.gate.Authenticate(apiReq(nil, cookie)); !ok {
		t.Fatal("control: the cookie alone did not authenticate")
	} else if c, _ := CredentialFrom(r.Context()); c.Kind != auth.CredentialSession || c.Has(auth.ScopeSelf) || !c.Has(auth.ScopeSession) {
		t.Errorf("the cookie's credential = %+v", c)
	}

	for name, r := range map[string]*http.Request{
		"cookie and bearer":         apiReq([]string{"Bearer " + tok}, cookie),
		"cookie and Basic":          apiReq([]string{"Basic Zm9vOmJhcg=="}, cookie),
		"cookie and an empty authz": apiReq([]string{""}, cookie),
		"two bearers":               apiReq([]string{"Bearer " + tok, "Bearer " + tok}, ""),
		"Basic scheme":              apiReq([]string{"Basic " + tok}, ""),
		"no scheme":                 apiReq([]string{tok}, ""),
		"two spaces":                apiReq([]string{"Bearer  " + tok}, ""),
		"malformed":                 apiReq([]string{"Bearer drydock_short"}, ""),
		"unknown":                   apiReq([]string{"Bearer " + auth.TokenPrefix + strings.Repeat("A", 43)}, ""),
		"revoked":                   apiReq([]string{"Bearer " + revoked}, ""),
	} {
		if _, ok := b.gate.Authenticate(r); ok {
			t.Errorf("%s: authenticated", name)
		}
	}
	// "bearer" in any case is the scheme (RFC 9110 §11.1).
	if _, ok := b.gate.Authenticate(apiReq([]string{"bearer " + tok}, "")); !ok {
		t.Error("a lowercase scheme was refused")
	}
	// Expiry: past its expires_at, the same token is refused.
	b.clock.Advance(auth.DefaultTokenLifetime)
	if _, ok := b.gate.Authenticate(apiReq([]string{"Bearer " + tok}, "")); ok {
		t.Error("an expired token authenticated")
	}
}

// TestBearerNeverCountsTowardTheLockout (§4.3): a hundred bad bearer
// requests from one address leave that address's password sign-in open,
// while five bad passwords from it close it (the control: the lockout is
// live in this rig).
func TestBearerNeverCountsTowardTheLockout(t *testing.T) {
	b := newBearerRig(t)
	if err := b.svc.SetPassword(context.Background(), "correct horse battery staple"); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 100; i++ {
		r := apiReq([]string{"Bearer " + auth.TokenPrefix + strings.Repeat("x", 43)}, "")
		r.Header.Set("X-Forwarded-For", "192.0.2.50")
		if _, ok := b.gate.Authenticate(r); ok {
			t.Fatal("a made-up token authenticated")
		}
	}
	var attempts int
	b.svc.DB.QueryRow(`SELECT count(*) FROM auth_attempt`).Scan(&attempts)
	if attempts != 0 {
		t.Errorf("100 bad bearers recorded %d sign-in attempts; want none", attempts)
	}
	if _, err := b.svc.SignIn(context.Background(), "192.0.2.50", "correct horse battery staple", "x"); err != nil {
		t.Errorf("after 100 bad bearers the password sign-in = %v; want it open", err)
	}
	for i := 0; i < 6; i++ {
		b.svc.SignIn(context.Background(), "192.0.2.51", "wrong", "x")
	}
	if _, err := b.svc.SignIn(context.Background(), "192.0.2.51", "correct horse battery staple", "x"); err == nil {
		t.Error("control: six bad passwords did not lock the address out")
	}
}

// TestTokenRoutesOverTheRealGate drives the token routes, the meta route and
// sign-out through Build with the real gate: the operator mints (201, the
// token in the body and nowhere in the list or the event), lists, reads
// meta; a token reads meta with its own credential, cannot list or mint, and
// revokes itself, after which it is 401; the operator's revoke is 202 and
// a second is 404.
func TestTokenRoutesOverTheRealGate(t *testing.T) {
	b := newBearerRig(t)
	cookie := b.cookie(t)
	handlers := TokenRoutes{Tokens: b.svc.Tokens}.Handlers()
	for k, v := range (MetaRoutes{Version: "v9.9.9"}).Handlers() {
		handlers[k] = v
	}
	for k, v := range (SessionRoutes{Auth: b.svc}).Handlers() {
		handlers[k] = v
	}
	mux := Build(MuxAPI, b.gate, handlers)
	do := func(method, path, body, cookie, bearer string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, "https://drydock.example.com"+path, strings.NewReader(body))
		r.Host = "drydock.example.com"
		r.Header.Set("Origin", "https://drydock.example.com")
		if cookie != "" {
			r.AddCookie(&http.Cookie{Name: auth.CookieName, Value: cookie})
		}
		if bearer != "" {
			r.Header.Set("Authorization", "Bearer "+bearer)
		}
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, r)
		return rec
	}
	sub := b.log.Subscribe()
	defer b.log.Cancel(sub)

	rec := do("POST", "/api/tokens", `{"label":"VS Code on my laptop","scopes":["read","lifecycle"],"expires_in_days":30}`, cookie, "")
	var minted struct {
		ID, Label, Token string
		Scopes           []string
		ExpiresAt        time.Time `json:"expires_at"`
	}
	json.Unmarshal(rec.Body.Bytes(), &minted)
	if rec.Code != http.StatusCreated || !auth.WellFormedToken(minted.Token) || minted.Label != "VS Code on my laptop" ||
		strings.Join(minted.Scopes, ",") != "read,lifecycle" || !minted.ExpiresAt.Equal(b.clock.Now().Add(30*24*time.Hour)) {
		t.Fatalf("mint = %d %s", rec.Code, rec.Body)
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
		t.Errorf("the mint's Cache-Control = %q; want no-store", cc)
	}
	select {
	case ev := <-sub.C:
		if ev.Kind != events.KindAPITokenCreated || strings.Contains(string(ev.Data)+ev.Message, minted.Token) || !strings.Contains(string(ev.Data), minted.ID) {
			t.Errorf("the mint's event = %s %s %s", ev.Kind, ev.Message, ev.Data)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no api_token.created")
	}
	list := do("GET", "/api/tokens", "", cookie, "")
	if list.Code != 200 || !strings.Contains(list.Body.String(), minted.ID) || strings.Contains(list.Body.String(), minted.Token) ||
		strings.Contains(list.Body.String(), "hash") {
		t.Errorf("list = %d %s", list.Code, list.Body)
	}

	// The token: meta with its own credential.
	meta := do("GET", "/api/meta", "", "", minted.Token)
	var m Meta
	json.Unmarshal(meta.Body.Bytes(), &m)
	if meta.Code != 200 || m.Version != "v9.9.9" || m.API != APIVersion || m.Credential.Kind != auth.CredentialToken ||
		m.Credential.Label != "VS Code on my laptop" || strings.Join(scopeStrings(m.Credential.Scopes), ",") != "read,lifecycle,self" {
		t.Errorf("meta for the token = %d %s", meta.Code, meta.Body)
	}
	opMeta := do("GET", "/api/meta", "", cookie, "")
	if !strings.Contains(opMeta.Body.String(), `"kind":"session"`) {
		t.Errorf("meta for the operator = %s", opMeta.Body)
	}
	// …and nothing of the operator's.
	for _, c := range []struct{ method, path, body string }{
		{"GET", "/api/tokens", ""}, {"POST", "/api/tokens", `{"label":"x","scopes":["read"]}`},
		{"DELETE", "/api/tokens/" + minted.ID, ""}, {"GET", "/api/auth/session", ""},
		{"DELETE", "/api/auth/sessions", ""}, {"DELETE", "/api/auth/session", ""},
	} {
		if rec := do(c.method, c.path, c.body, "", minted.Token); rec.Code != http.StatusForbidden || errCodeOf(rec) != CodeForbiddenScope {
			t.Errorf("a token's %s %s = %d %s; want 403 forbidden_scope", c.method, c.path, rec.Code, rec.Body)
		}
	}
	// The cookie cannot use the token-only route.
	if rec := do("DELETE", "/api/auth/token", "", cookie, ""); rec.Code != http.StatusForbidden || errCodeOf(rec) != CodeForbiddenScope {
		t.Errorf("the cookie's DELETE /api/auth/token = %d %s; want 403 forbidden_scope", rec.Code, rec.Body)
	}
	// Self-revocation, then 401.
	if rec := do("DELETE", "/api/auth/token", "", "", minted.Token); rec.Code != http.StatusNoContent {
		t.Errorf("self-revoke = %d %s", rec.Code, rec.Body)
	}
	if rec := do("GET", "/api/meta", "", "", minted.Token); rec.Code != http.StatusUnauthorized {
		t.Errorf("after self-revoke = %d; want 401", rec.Code)
	}

	// The operator's revoke: 202, then 404.
	second, sm := b.mint(t, auth.ScopeRead)
	if rec := do("DELETE", "/api/tokens/"+sm.ID, "", cookie, ""); rec.Code != http.StatusAccepted {
		t.Errorf("revoke = %d %s", rec.Code, rec.Body)
	}
	if rec := do("DELETE", "/api/tokens/"+sm.ID, "", cookie, ""); rec.Code != http.StatusNotFound {
		t.Errorf("a second revoke = %d; want 404", rec.Code)
	}
	if rec := do("GET", "/api/meta", "", "", second); rec.Code != http.StatusUnauthorized {
		t.Errorf("a revoked token = %d; want 401", rec.Code)
	}

	// Refused mints write nothing.
	before := do("GET", "/api/tokens", "", cookie, "").Body.String()
	for _, body := range []string{``, `{}`, `{"label":"","scopes":["read"]}`, `{"label":"x","scopes":[]}`,
		`{"label":"x","scopes":["session"]}`, `{"label":"x","scopes":["lifecycle"]}`, `{"label":"x","scopes":["lifecycle","approve"]}`, `{"label":"x","scopes":["self"]}`, `{"label":"x","scopes":["read","read"]}`,
		`{"label":"x","scopes":["read"],"expires_in_days":0}`, `{"label":"x","scopes":["read"],"expires_in_days":366}`,
		`{"label":"a\u0007b","scopes":["read"]}`, `{"label":"x","scopes":["read"],"extra":1}`,
		`{"label":"` + strings.Repeat("x", 101) + `","scopes":["read"]}`} {
		if rec := do("POST", "/api/tokens", body, cookie, ""); rec.Code != http.StatusBadRequest {
			t.Errorf("mint %s = %d; want 400", body, rec.Code)
		}
	}
	if after := do("GET", "/api/tokens", "", cookie, "").Body.String(); after != before {
		t.Error("a refused mint changed the list")
	}

	// Sign-out-everywhere is its own route; the old ?all=true is refused
	// rather than read as "this device".
	if rec := do("DELETE", "/api/auth/session?all=true", "", cookie, ""); rec.Code != http.StatusBadRequest {
		t.Errorf("DELETE /api/auth/session?all=true = %d; want 400", rec.Code)
	}
	other := b.cookie(t)
	if rec := do("DELETE", "/api/auth/sessions", "", cookie, ""); rec.Code != http.StatusNoContent {
		t.Errorf("sign out everywhere = %d", rec.Code)
	}
	for _, c := range []string{cookie, other} {
		if rec := do("GET", "/api/meta", "", c, ""); rec.Code != http.StatusUnauthorized {
			t.Errorf("a session after sign-out-everywhere = %d; want 401", rec.Code)
		}
	}
}

func scopeStrings(s []auth.Scope) []string {
	out := make([]string, len(s))
	for i, x := range s {
		out[i] = string(x)
	}
	return out
}

// TestStoreFailureIsNot401 (review of #149): a 401 is a client's
// authoritative "this token is over", so only a bad token earns one. A
// failed last_used_at write is best-effort — the request still passes — and
// a failed read is 503 unavailable, the token kept. The controls: the token
// passes before either failure, and a revoked one is still 401.
func TestStoreFailureIsNot401(t *testing.T) {
	b := newBearerRig(t)
	tok, meta := b.mint(t, auth.ScopeRead)
	mux := Build(MuxAPI, b.gate, MetaRoutes{}.Handlers())
	get := func(bearer string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("GET", "https://drydock.example.com/api/meta", nil)
		r.Host = "drydock.example.com"
		r.Header.Set("Authorization", "Bearer "+bearer)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, r)
		return rec
	}
	if rec := get(tok); rec.Code != http.StatusOK {
		t.Fatalf("control: %d", rec.Code)
	}
	db := b.svc.DB
	used := func() string {
		var u string
		db.QueryRow(`SELECT last_used_at FROM api_token WHERE id = ?`, meta.ID).Scan(&u)
		return u
	}
	first := used()

	// The write fails; the request does not.
	if _, err := db.Exec(`CREATE TRIGGER no_touch BEFORE UPDATE OF last_used_at ON api_token BEGIN SELECT RAISE(ABORT, 'busy'); END`); err != nil {
		t.Fatal(err)
	}
	b.clock.Advance(2 * time.Minute)
	if rec := get(tok); rec.Code != http.StatusOK {
		t.Errorf("a failed last_used_at write = %d; want 200", rec.Code)
	}
	if first == "" || used() != first {
		t.Errorf("last_used_at %q → %q; the trigger should have refused the second write", first, used())
	}

	// The read fails: 503, never 401.
	if _, err := db.Exec(`ALTER TABLE api_token RENAME TO api_token_gone`); err != nil {
		t.Fatal(err)
	}
	rec := get(tok)
	if rec.Code != http.StatusServiceUnavailable || errCodeOf(rec) != CodeUnavailable || rec.Header().Get("Retry-After") == "" {
		t.Errorf("a failed read = %d %s; want 503 unavailable with Retry-After", rec.Code, rec.Body)
	}
	if _, err := db.Exec(`ALTER TABLE api_token_gone RENAME TO api_token`); err != nil {
		t.Fatal(err)
	}
	// Control: a bad token is still 401.
	if _, err := b.svc.Tokens.Revoke(context.Background(), meta.ID); err != nil {
		t.Fatal(err)
	}
	if rec := get(tok); rec.Code != http.StatusUnauthorized {
		t.Errorf("a revoked token = %d; want 401", rec.Code)
	}
}

// TestSessionStoreFailureIsNot401 is TestStoreFailureIsNot401 for the
// cookie: a 401 is what sends the web UI to /signin, so a session store
// Drydock cannot read answers 503 unavailable with Retry-After on every door
// a cookie comes through — a route, a navigation (AuthRedirect, which would
// otherwise bounce to the sign-in page) and an undeclared path — and a
// failed slide is best-effort. The controls: the cookie passes before the
// failures and after the store recovers, and a revoked cookie is still 401
// (and still a redirect to sign in on a navigation).
func TestSessionStoreFailureIsNot401(t *testing.T) {
	b := newBearerRig(t)
	cookie, sess, err := b.svc.Sessions.Create(context.Background(), "Mac Firefox", "192.0.2.1")
	if err != nil {
		t.Fatal(err)
	}
	mux := Build(MuxAPI, b.gate, MetaRoutes{}.Handlers())
	front := Unrouted(b.gate, mux)
	get := func(path string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("GET", "https://drydock.example.com"+path, nil)
		r.Host = "drydock.example.com"
		r.AddCookie(&http.Cookie{Name: auth.CookieName, Value: cookie})
		rec := httptest.NewRecorder()
		front.ServeHTTP(rec, r)
		return rec
	}
	// What each door answers a cookie that got through auth: the route,
	// the navigation's nil handler (501, after auth), the undeclared 404.
	passed := map[string]int{
		"/api/meta":          http.StatusOK,
		"/preview/authorize": http.StatusNotImplemented,
		"/api/no-such-route": http.StatusNotFound,
	}
	check := func(when string) {
		t.Helper()
		for path, want := range passed {
			if rec := get(path); rec.Code != want {
				t.Errorf("%s: %s = %d; want %d", when, path, rec.Code, want)
			}
		}
	}
	check("control")

	// The slide fails; the request does not.
	db := b.svc.DB
	if _, err := db.Exec(`CREATE TRIGGER no_slide BEFORE UPDATE OF last_seen_at ON auth_session BEGIN SELECT RAISE(ABORT, 'busy'); END`); err != nil {
		t.Fatal(err)
	}
	b.clock.Advance(time.Minute)
	check("a failed slide")
	if _, err := db.Exec(`DROP TRIGGER no_slide`); err != nil {
		t.Fatal(err)
	}

	// The read fails: 503 with Retry-After at every door, never 401 or a
	// redirect to sign in.
	if _, err := db.Exec(`ALTER TABLE auth_session RENAME TO auth_session_gone`); err != nil {
		t.Fatal(err)
	}
	for path := range passed {
		rec := get(path)
		if rec.Code != http.StatusServiceUnavailable || errCodeOf(rec) != CodeUnavailable || rec.Header().Get("Retry-After") == "" {
			t.Errorf("a failed read: %s = %d %q %s; want 503 unavailable with Retry-After", path, rec.Code, rec.Header().Get("Location"), rec.Body)
		}
	}
	if _, err := db.Exec(`ALTER TABLE auth_session_gone RENAME TO auth_session`); err != nil {
		t.Fatal(err)
	}
	check("the store recovered")

	// Control: a bad cookie is still 401, and a navigation still goes to
	// sign in.
	if err := b.svc.Sessions.Revoke(context.Background(), sess.ID); err != nil {
		t.Fatal(err)
	}
	if rec := get("/api/meta"); rec.Code != http.StatusUnauthorized || errCodeOf(rec) != CodeUnauthenticated {
		t.Errorf("a revoked cookie = %d %s; want 401", rec.Code, rec.Body)
	}
	if rec := get("/api/no-such-route"); rec.Code != http.StatusUnauthorized {
		t.Errorf("a revoked cookie on an undeclared path = %d; want 401", rec.Code)
	}
	if rec := get("/preview/authorize"); rec.Code != http.StatusFound || !strings.HasPrefix(rec.Header().Get("Location"), "/signin?") {
		t.Errorf("a revoked cookie's navigation = %d %q; want 302 to /signin", rec.Code, rec.Header().Get("Location"))
	}
}
