package preview_test

import (
	"context"
	"crypto/rand"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"net/http/httputil"
	"net/textproto"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/krelinga/drydock/internal/auth"
	"github.com/krelinga/drydock/internal/preview"
	"github.com/krelinga/drydock/internal/store"
	"github.com/krelinga/drydock/internal/sys"
)

const (
	domain = "drydock-preview.test"
	slug   = "myapp-5173-p2mq"
	host   = slug + "." + domain
)

type fixture struct {
	db       *sql.DB
	clock    *sys.FakeClock
	svc      *preview.Service
	sessions *auth.Sessions
	authID   string
}

// newFixture is one running workspace with one enabled port, and one signed-in
// device, on a fake clock.
func newFixture(t *testing.T) *fixture {
	t.Helper()
	ctx := context.Background()
	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "drydock.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	clock := sys.NewFakeClock(time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC))
	f := &fixture{db: db.DB, clock: clock}
	f.exec(t, `INSERT INTO repository (id, installation_id, full_name, default_branch) VALUES (1, 1, 'o/myapp', 'main')`)
	f.exec(t, `INSERT INTO workspace (id, repository_id, host_path, branch, state) VALUES ('w1', 1, '/x', 'main', 'running')`)
	f.exec(t, `INSERT INTO forwarded_port (id, workspace_id, container_port, slug, enabled) VALUES ('p1', 'w1', 5173, ?, 1)`, slug)
	f.sessions = &auth.Sessions{DB: db.DB, Clock: clock, Random: rand.Reader}
	_, sess, err := f.sessions.Create(ctx, "test", "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	f.authID = sess.ID
	f.svc = &preview.Service{DB: db.DB, Clock: clock, Random: rand.Reader, Domain: domain, AuthIdle: auth.IdleLifetime}
	return f
}

func (f *fixture) exec(t *testing.T, q string, args ...any) {
	t.Helper()
	if _, err := f.db.Exec(q, args...); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
}

func (f *fixture) grant() preview.Grant {
	return preview.Grant{AuthSessionID: f.authID, Host: host, PortID: "p1", Path: "/app?x=1"}
}

// handshake runs mint, consume and start, and returns the cookie.
func (f *fixture) handshake(t *testing.T) string {
	t.Helper()
	tok, err := f.svc.Mint(f.grant())
	if err != nil {
		t.Fatal(err)
	}
	g, ok := f.svc.Consume(tok, host)
	if !ok {
		t.Fatal("a fresh token on its own host was refused")
	}
	cookie, _, err := f.svc.StartSession(context.Background(), g)
	if err != nil {
		t.Fatal(err)
	}
	return cookie
}

func (f *fixture) valid(cookie string) bool {
	_, ok := f.svc.Session(context.Background(), cookie, host)
	return ok
}

func TestSlug(t *testing.T) {
	for h, want := range map[string]string{
		host:                        slug,
		host + ":443":               slug,
		"MyApp-5173-P2MQ." + domain: "myapp-5173-p2mq",
		"a." + domain:               "a",
	} {
		if got, ok := preview.Slug(h, domain); !ok || got != want {
			t.Errorf("Slug(%q) = %q, %v; want %q", h, got, ok, want)
		}
	}
	for _, h := range []string{
		domain, "x.y." + domain, "drydock-check." + domain, "-a." + domain, "a-." + domain,
		"a_b." + domain, slug + ".drydock.test", slug + "." + domain + ".evil.example", "",
		strings.Repeat("a", 64) + "." + domain, slug + "." + domain + ".",
	} {
		if got, ok := preview.Slug(h, domain); ok {
			t.Errorf("Slug(%q) = %q; want no preview host", h, got)
		}
	}
	if _, ok := preview.Slug(host, ""); ok {
		t.Error("with no preview domain, a host was a preview host")
	}
}

// TestParseReturn is authorize's open-redirect guard: the only URLs it accepts
// are https URLs on one preview host, and what it lands on is a path there.
func TestParseReturn(t *testing.T) {
	good := map[string][2]string{
		"https://" + host + "/":               {host, "/"},
		"https://" + host:                     {host, "/"},
		"https://" + host + "/a/b?c=d&e=%2F":  {host, "/a/b?c=d&e=%2F"},
		"https://" + host + "//evil.example/": {host, "//evil.example/"}, // a path on the preview host, landed on absolutely
		"https://" + host + "/x#frag":         {host, "/x"},
	}
	for raw, want := range good {
		h, uri, ok := preview.ParseReturn(raw, domain)
		if !ok || h != want[0] || uri != want[1] {
			t.Errorf("ParseReturn(%q) = %q %q %v; want %q %q", raw, h, uri, ok, want[0], want[1])
		}
	}
	for _, raw := range []string{
		"", "/", "//" + host + "/", "http://" + host + "/", "https://evil.example/",
		"https://" + host + ".evil.example/", "https://evil.example/?https://" + host,
		"https://" + host + ":8443/", "https://" + host + ":/", "https://" + host + ":", "https://user@" + host + "/", "https://" + host + "@evil.example/",
		"https://drydock-check." + domain + "/", "https://" + domain + "/", "https://a.b." + domain + "/",
		"https://drydock.test/", "javascript:alert(1)", "https:" + host, "https:/" + host + "/",
		"https://" + host + "\\@evil.example/", "https://" + host + "/\t/x", " https://" + host + "/",
		"https://" + strings.ToUpper(host) + "/", "https://" + host + "/\x00",
	} {
		if h, uri, ok := preview.ParseReturn(raw, domain); ok {
			t.Errorf("ParseReturn(%q) accepted: %q %q", raw, h, uri)
		}
	}
}

// TestHandshakeOnAFakeClock: mint, consume, start, and the cookie it sets is a
// session for that host — until its idle window, which a use slides.
func TestHandshakeOnAFakeClock(t *testing.T) {
	f := newFixture(t)
	cookie := f.handshake(t)
	if !f.valid(cookie) {
		t.Fatal("the cookie the handshake set is not a session")
	}
	tgt, _ := f.svc.Session(context.Background(), cookie, host)
	if tgt.PortID != "p1" || tgt.ContainerPort != 5173 || tgt.WorkspaceID != "w1" || tgt.HostHeader != "localhost" || tgt.Host != host {
		t.Errorf("target = %+v", tgt)
	}
	// Slides: used at 11h, alive at 22h; then idle past the window.
	f.clock.Advance(11 * time.Hour)
	if !f.valid(cookie) {
		t.Fatal("control: an 11-hour-old session was refused")
	}
	f.clock.Advance(11 * time.Hour)
	if !f.valid(cookie) {
		t.Fatal("a session used 11 hours ago was refused: its idle window did not slide")
	}
	f.clock.Advance(preview.IdleLifetime)
	if f.valid(cookie) {
		t.Fatal("a session idle past IdleLifetime is still valid")
	}
}

// TestTokenIsSingleUseUnderARace is testing §8.5's row: a hundred concurrent
// consumes of one token, exactly one success — and that one completes the
// handshake.
func TestTokenIsSingleUseUnderARace(t *testing.T) {
	f := newFixture(t)
	tok, err := f.svc.Mint(f.grant())
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	var mu sync.Mutex
	var wins []preview.Grant
	start := make(chan struct{})
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			if g, ok := f.svc.Consume(tok, host); ok {
				mu.Lock()
				wins = append(wins, g)
				mu.Unlock()
			}
		}()
	}
	close(start)
	wg.Wait()
	if len(wins) != 1 {
		t.Fatalf("%d of 100 concurrent consumes succeeded; want exactly 1", len(wins))
	}
	if _, _, err := f.svc.StartSession(context.Background(), wins[0]); err != nil {
		t.Errorf("the one winner did not complete the handshake: %v", err)
	}
	if f.svc.Pending() != 0 {
		t.Errorf("%d tokens still pending", f.svc.Pending())
	}
}

func TestTokenExpires(t *testing.T) {
	f := newFixture(t)
	early, _ := f.svc.Mint(f.grant())
	late, _ := f.svc.Mint(f.grant())
	f.clock.Advance(preview.TokenTTL - time.Second)
	if _, ok := f.svc.Consume(early, host); !ok {
		t.Fatal("control: a token one second short of its TTL was refused")
	}
	f.clock.Advance(time.Second)
	if _, ok := f.svc.Consume(late, host); ok {
		t.Fatal("a token at its TTL was accepted")
	}
}

// TestTokenIsBoundToItsHost: a token shown on another preview host is refused
// and spent, so it cannot then be carried to its own.
func TestTokenIsBoundToItsHost(t *testing.T) {
	f := newFixture(t)
	tok, _ := f.svc.Mint(f.grant())
	if _, ok := f.svc.Consume(tok, "other-1-abcd."+domain); ok {
		t.Fatal("a token was accepted on another preview host")
	}
	if _, ok := f.svc.Consume(tok, host); ok {
		t.Fatal("a token shown on the wrong host was still good on its own: it was not spent")
	}
	ok2, _ := f.svc.Mint(f.grant())
	if _, ok := f.svc.Consume(ok2, strings.ToUpper(host)+":443"); !ok {
		t.Error("control: the right host (case and port aside) was refused")
	}
	if _, ok := f.svc.Consume("forged-"+tok, host); ok {
		t.Error("a forged token was accepted")
	}
	if _, ok := f.svc.Consume("", host); ok {
		t.Error("no token was accepted")
	}
}

// TestSessionIsBoundToItsHostAndForgeriesFail: a real cookie is good on one
// preview host only, and anything not issued is nothing.
func TestSessionIsBoundToItsHostAndForgeriesFail(t *testing.T) {
	f := newFixture(t)
	f.exec(t, `INSERT INTO forwarded_port (id, workspace_id, container_port, slug, enabled) VALUES ('p2', 'w1', 8080, 'myapp-8080-zzzz', 1)`)
	cookie := f.handshake(t)
	if !f.valid(cookie) {
		t.Fatal("control: the cookie is not valid on its own host")
	}
	if _, ok := f.svc.Session(context.Background(), cookie, "myapp-8080-zzzz."+domain); ok {
		t.Error("a preview cookie was accepted on another (enabled) preview host")
	}
	for _, forged := range []string{"", "forged", cookie + "x", strings.ToUpper(cookie)} {
		if f.valid(forged) {
			t.Errorf("forged cookie %q was accepted", forged)
		}
	}
}

// TestDisableAndRetireEndSessions is PF §5: a disable deletes the rows, so a
// re-enable revives none; a retire does the same and keeps the slug spent.
func TestDisableAndRetireEndSessions(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	cookie := f.handshake(t)
	if !f.valid(cookie) {
		t.Fatal("control: the session works before the disable")
	}
	if err := f.svc.SetEnabled(ctx, "p1", false); err != nil {
		t.Fatal(err)
	}
	var n int
	f.db.QueryRow(`SELECT count(*) FROM preview_session`).Scan(&n)
	if n != 0 {
		t.Errorf("%d preview sessions survived the disable", n)
	}
	if err := f.svc.SetEnabled(ctx, "p1", true); err != nil {
		t.Fatal(err)
	}
	if f.valid(cookie) {
		t.Error("a re-enable revived a session the disable ended")
	}
	again := f.handshake(t)
	if !f.valid(again) {
		t.Fatal("control: a new handshake after re-enable failed")
	}
	if err := f.svc.Retire(ctx, "p1"); err != nil {
		t.Fatal(err)
	}
	if f.valid(again) {
		t.Error("a retired port's session is still valid")
	}
	if _, err := f.svc.Resolve(ctx, slug); !errors.Is(err, preview.ErrNotPreviewable) {
		t.Errorf("a retired slug resolves: %v", err)
	}
	if err := f.svc.SetEnabled(ctx, "p1", true); err == nil {
		t.Error("a retired port was re-enabled")
	}
}

// TestRevokeAllEndsPreviews is §13.2's one button, reaching previews: every
// preview session derived from a revoked auth session stops on its next use,
// and a grant minted before the revoke cannot start one after it.
func TestRevokeAllEndsPreviews(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	cookie := f.handshake(t)
	if !f.valid(cookie) {
		t.Fatal("control: the session works before the revoke")
	}
	tok, _ := f.svc.Mint(f.grant())
	if err := f.sessions.RevokeAll(ctx); err != nil {
		t.Fatal(err)
	}
	if f.valid(cookie) {
		t.Error("revoke-all left a preview session working")
	}
	g, ok := f.svc.Consume(tok, host)
	if !ok {
		t.Fatal("the token itself is in memory and still unspent; only its session is gone")
	}
	if _, _, err := f.svc.StartSession(ctx, g); !errors.Is(err, preview.ErrSessionGone) {
		t.Errorf("a grant from a revoked session started a preview session: %v", err)
	}
}

// TestAuthSessionExpiryEndsPreviews: an auth session past its own idle window
// is a device the UI has forgotten, and its previews go with it, even though
// the row is only deleted lazily.
func TestAuthSessionExpiryEndsPreviews(t *testing.T) {
	f := newFixture(t)
	cookie := f.handshake(t)
	// Keep the preview itself busy, so only the auth session idles out.
	for d := time.Duration(0); d < auth.IdleLifetime; d += 6 * time.Hour {
		if !f.valid(cookie) {
			t.Fatalf("control: refused at %v, before the auth session's idle window", d)
		}
		f.clock.Advance(6 * time.Hour)
	}
	if f.valid(cookie) {
		t.Error("a preview outlived its auth session's idle window")
	}
}

// TestStoppedWorkspaceIsNotPreviewable: access follows the workspace's state.
func TestStoppedWorkspaceIsNotPreviewable(t *testing.T) {
	f := newFixture(t)
	cookie := f.handshake(t)
	f.exec(t, `UPDATE workspace SET state = 'stopped' WHERE id = 'w1'`)
	if f.valid(cookie) {
		t.Error("a stopped workspace's preview is still valid")
	}
	if _, err := f.svc.Resolve(context.Background(), slug); !errors.Is(err, preview.ErrNotPreviewable) {
		t.Errorf("a stopped workspace's slug resolves: %v", err)
	}
	f.exec(t, `UPDATE workspace SET state = 'running' WHERE id = 'w1'`)
	if !f.valid(cookie) {
		t.Error("control: running again, the session was refused")
	}
}

// TestStoredValuesAreHashes: the database holds the cookie's SHA-256, never
// the cookie; and no token is ever written.
func TestStoredValuesAreHashes(t *testing.T) {
	f := newFixture(t)
	tok, _ := f.svc.Mint(f.grant())
	cookie := f.handshake(t)
	var id string
	f.db.QueryRow(`SELECT id FROM preview_session`).Scan(&id)
	if id == "" || id == cookie || strings.Contains(id, cookie) || len(id) != 64 {
		t.Errorf("preview_session.id = %q; want the cookie's 64-hex SHA-256", id)
	}
	rows, _ := f.db.Query(`SELECT * FROM preview_session`)
	defer rows.Close()
	cols, _ := rows.Columns()
	for rows.Next() {
		vals := make([]sql.RawBytes, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		rows.Scan(ptrs...)
		for i, v := range vals {
			if strings.Contains(string(v), cookie) || strings.Contains(string(v), tok) {
				t.Errorf("preview_session.%s holds a credential", cols[i])
			}
		}
	}
}

// TestPendingTokensAreBounded: one auth session looping on authorize is
// refused after its own share, while another device still mints; and the
// table as a whole has a ceiling however many sessions mint.
func TestPendingTokensAreBounded(t *testing.T) {
	f := newFixture(t)
	mintUntilRefused := func(sess string, limit int) int {
		g := f.grant()
		g.AuthSessionID = sess
		for n := 0; n < limit; n++ {
			if _, err := f.svc.Mint(g); err != nil {
				if !errors.Is(err, preview.ErrTooManyPending) {
					t.Fatal(err)
				}
				return n
			}
		}
		return limit
	}
	looping := mintUntilRefused(f.authID, 5000)
	if looping == 5000 || looping > 100 {
		t.Fatalf("one session minted %d pending tokens: its share is unbounded", looping)
	}
	if got := mintUntilRefused("another-device", 1); got != 1 {
		t.Fatal("one looping session refused another device's mint: the cap is global, not per session")
	}
	// The ceiling: many sessions together.
	total := looping + 1
	for i := 0; total < 5000 && i < 200; i++ {
		total += mintUntilRefused(fmt.Sprintf("s%d", i), 5000)
	}
	if total >= 5000 {
		t.Fatalf("%d pending tokens across sessions: no global ceiling", total)
	}
	if got := mintUntilRefused("yet-another", 1); got != 0 {
		t.Error("a new session minted past the global ceiling")
	}
	f.clock.Advance(preview.TokenTTL)
	if got := mintUntilRefused(f.authID, 1); got != 1 {
		t.Error("control: after the pending tokens expired, a mint was refused")
	}
}

// TestStripCookie is PF §7's second hop: the preview cookie is gone from what
// the upstream sees, and every other cookie — the app's — passes, in order.
func TestStripCookie(t *testing.T) {
	r := httptest.NewRequest("GET", "https://"+host+"/", nil)
	r.Header.Add("Cookie", "app-a=1; "+preview.CookieName+"=c4n4ry; app-b=2")
	r.Header.Add("Cookie", strings.ToLower(preview.CookieName)+"=again; app-c=3")
	out := preview.StripCookie(r)
	got := out.Header.Get("Cookie")
	if strings.Contains(got, "c4n4ry") || strings.Contains(got, "again") || strings.Contains(strings.ToLower(got), "drydock-preview") {
		t.Errorf("Cookie after the strip = %q; the preview cookie survived", got)
	}
	if got != "app-a=1; app-b=2; app-c=3" {
		t.Errorf("Cookie after the strip = %q; want the app's three, in order", got)
	}
	if r.Header.Get("Cookie") == got {
		t.Error("the strip changed the caller's request rather than a copy")
	}
	only := httptest.NewRequest("GET", "/", nil)
	only.Header.Set("Cookie", preview.CookieName+"=x")
	if v, ok := preview.StripCookie(only).Header["Cookie"]; ok {
		t.Errorf("a request with only the preview cookie still carries Cookie %q", v)
	}
}

// TestCookieGuard: an upstream cannot set or clear the preview cookie, and
// every other Set-Cookie passes — on an explicit WriteHeader and an implicit
// one alike.
func TestCookieGuard(t *testing.T) {
	for name, write := range map[string]func(http.ResponseWriter){
		"WriteHeader": func(w http.ResponseWriter) { w.WriteHeader(201) },
		"Write":       func(w http.ResponseWriter) { w.Write([]byte("hi")) },
		"Flush":       func(w http.ResponseWriter) { w.(http.Flusher).Flush() },
	} {
		rec := httptest.NewRecorder()
		g := &preview.CookieGuard{ResponseWriter: rec}
		g.Header().Add("Set-Cookie", "app=kept; Path=/")
		g.Header().Add("Set-Cookie", preview.CookieName+"=hijack; Path=/; Secure")
		g.Header().Add("Set-Cookie", strings.ToUpper(preview.CookieName)+"=x")
		g.Header().Add("Set-Cookie", " "+preview.CookieName+"=; Max-Age=0")
		g.Header().Add("Set-Cookie", "other=also-kept")
		write(g)
		got := rec.Result().Header.Values("Set-Cookie")
		if len(got) != 2 || got[0] != "app=kept; Path=/" || got[1] != "other=also-kept" {
			t.Errorf("%s: Set-Cookie = %q; want the app's two and nothing claiming %s", name, got, preview.CookieName)
		}
	}
}

// TestPlaceholderNamesNoValue: step 2's stand-in upstream shows cookie names
// so the strip can be seen in a browser, and never a value.
func TestPlaceholderNamesNoValue(t *testing.T) {
	r := httptest.NewRequest("GET", "https://"+host+"/", nil)
	r.Header.Set("Cookie", "app-own=s3cr3t; <b>=x")
	rec := httptest.NewRecorder()
	preview.Placeholder.ServePreview(rec, r, preview.Target{ContainerPort: 5173})
	body := rec.Body.String()
	if !strings.Contains(body, "app-own") || strings.Contains(body, "s3cr3t") {
		t.Errorf("placeholder body = %q; want the name app-own and no value", body)
	}
	if strings.Contains(body, "<b>") {
		t.Error("a cookie name reached the page unescaped")
	}
}

// hostileEarlyHints is a dev server that sends a 103 Early Hints carrying the
// preview cookie, then plants another after the 103 for the final response.
// Beside each, an app cookie that must pass: the control.
func hostileEarlyHints(w http.ResponseWriter, _ *http.Request) {
	w.Header().Add("Link", "</app.css>; rel=preload")
	w.Header().Add("Set-Cookie", preview.CookieName+"=early")
	w.Header().Add("Set-Cookie", "app-early=kept")
	w.WriteHeader(http.StatusEarlyHints)
	w.Header().Del("Set-Cookie")
	w.Header().Add("Set-Cookie", preview.CookieName+"=planted; Path=/; Secure; HttpOnly")
	w.Header().Add("Set-Cookie", "app=kept")
	w.WriteHeader(http.StatusOK)
	w.Write([]byte("ok"))
}

// TestCookieGuardFiltersInformationalResponses is the guard against a 1xx: a
// guard that stopped filtering at the first WriteHeader would send the 103's
// preview cookie and let the one added after it ride the final 200. Through
// httputil.ReverseProxy — which forwards an upstream's 1xx by default, and is
// step 3's likely proxy — and with the upstream writing to the guard directly.
func TestCookieGuardFiltersInformationalResponses(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(hostileEarlyHints))
	defer upstream.Close()
	target, _ := url.Parse(upstream.URL)
	for name, inner := range map[string]http.Handler{
		"through httputil.ReverseProxy": httputil.NewSingleHostReverseProxy(target),
		"written to the guard directly": http.HandlerFunc(hostileEarlyHints),
	} {
		front := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			inner.ServeHTTP(&preview.CookieGuard{ResponseWriter: w}, r)
		}))
		var infos []http.Header
		trace := &httptrace.ClientTrace{Got1xxResponse: func(code int, h textproto.MIMEHeader) error {
			infos = append(infos, http.Header(h).Clone())
			return nil
		}}
		req, _ := http.NewRequestWithContext(httptrace.WithClientTrace(context.Background(), trace), "GET", front.URL, nil)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		resp.Body.Close()
		front.Close()
		// Controls: the 103 really arrived, with its Link and the app's
		// cookie; the final response carries the app's cookie.
		if len(infos) != 1 || infos[0].Get("Link") == "" || !strings.Contains(strings.Join(infos[0].Values("Set-Cookie"), "|"), "app-early=kept") {
			t.Fatalf("%s: control: the 103 did not arrive as sent: %v", name, infos)
		}
		final := resp.Header.Values("Set-Cookie")
		if !strings.Contains(strings.Join(final, "|"), "app=kept") {
			t.Errorf("%s: control: the app's cookie did not reach the final response: %q", name, final)
		}
		for what, vals := range map[string][]string{"the 103": infos[0].Values("Set-Cookie"), "the final response": final} {
			for _, v := range vals {
				if strings.Contains(strings.ToLower(v), strings.ToLower(preview.CookieName)) {
					t.Errorf("%s: %s carried %q", name, what, v)
				}
			}
		}
	}
}
