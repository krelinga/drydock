package server

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/krelinga/drydock/internal/api"
	"github.com/krelinga/drydock/internal/events"
)

// The API tokens of vscode-extension phase T, end to end through the real
// server: its gate, its store, its stream.

// mintToken mints a token through POST /api/tokens with the operator's
// cookie, and returns the token, its id and the raw response body (the one
// place the token is ever delivered).
func (r *running) mintToken(t *testing.T, cookie, label string, scopes ...string) (token, id, body string) {
	t.Helper()
	b, _ := json.Marshal(map[string]any{"label": label, "scopes": scopes})
	resp := r.do(t, req{method: "POST", path: "/api/tokens", origin: uiOrigin, cookie: cookie, body: string(b)})
	raw, _ := io.ReadAll(resp.Body)
	var m struct{ ID, Token string }
	json.Unmarshal(raw, &m)
	if resp.StatusCode != http.StatusCreated || m.Token == "" || m.ID == "" {
		t.Fatalf("mint = %d %s", resp.StatusCode, raw)
	}
	return m.Token, m.ID, string(raw)
}

// streamRecorder is an open GET /api/events, its every byte recorded.
type streamRecorder struct {
	mu   sync.Mutex
	buf  strings.Builder
	done chan struct{}
}

func (r *running) record(t *testing.T, q req) *streamRecorder {
	t.Helper()
	q.method, q.path = "GET", "/api/events"
	resp := r.do(t, q)
	if resp.StatusCode != 200 {
		t.Fatalf("stream = %d", resp.StatusCode)
	}
	s := &streamRecorder{done: make(chan struct{})}
	go func() {
		defer close(s.done)
		sc := bufio.NewScanner(resp.Body)
		for sc.Scan() {
			s.mu.Lock()
			s.buf.WriteString(sc.Text() + "\n")
			s.mu.Unlock()
		}
	}()
	return s
}

func (s *streamRecorder) text() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.String()
}

// waitFor waits until the transcript holds needle.
func (s *streamRecorder) waitFor(t *testing.T, needle string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !strings.Contains(s.text(), needle) {
		if time.Now().After(deadline) {
			t.Fatalf("the stream never carried %q; it holds:\n%s", needle, s.text())
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestTokensEndToEnd: a token minted in Settings reads GET /api/meta with
// its own credential; one without approve gets 403 forbidden_scope from the
// approve route, which one with approve passes (to the handler's own 404);
// no token reaches secrets or the preview handshake, or authenticates on the
// preview socket; a cookie with a bearer is 401; a revoked token is 401 on
// its next request.
func TestTokensEndToEnd(t *testing.T) {
	r := start(t)
	seedPreview(t, r)
	cookie := r.signIn(t)
	tok, id, _ := r.mintToken(t, cookie, "VS Code on my laptop", "read", "lifecycle")
	approver, _, _ := r.mintToken(t, cookie, "approver", "read", "approve")

	resp := r.do(t, req{method: "GET", path: "/api/meta", bearer: tok})
	var m api.Meta
	json.NewDecoder(resp.Body).Decode(&m)
	if resp.StatusCode != 200 || m.Version != "dev" || m.API != api.APIVersion || m.Credential.Kind != "token" ||
		m.Credential.Label != "VS Code on my laptop" || fmt.Sprint(m.Credential.Scopes) != "[read lifecycle self]" {
		t.Fatalf("meta = %d %+v", resp.StatusCode, m)
	}

	approve := req{method: "POST", path: "/api/workspaces/01JNOPE0000000000000000000/config-approval", origin: uiOrigin, body: `{"hash":"sha256:ab"}`}
	q := approve
	q.bearer = tok
	if resp := r.do(t, q); resp.StatusCode != http.StatusForbidden || code(t, resp) != api.CodeForbiddenScope {
		t.Errorf("approve without the scope = %d; want 403 forbidden_scope", resp.StatusCode)
	}
	q.bearer = approver
	if resp := r.do(t, q); resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusUnauthorized {
		t.Errorf("control: approve with the scope = %d; want past the gate", resp.StatusCode)
	}
	// Origin still binds a token on a mutating route.
	q.origin = ""
	if resp := r.do(t, q); resp.StatusCode != http.StatusForbidden || code(t, resp) != api.CodeForbiddenOrigin {
		t.Errorf("a token with no Origin = %d; want 403 forbidden_origin", resp.StatusCode)
	}

	if resp := r.do(t, req{method: "GET", path: "/api/secrets", bearer: tok}); resp.StatusCode != http.StatusForbidden {
		t.Errorf("a token's secrets list = %d; want 403", resp.StatusCode)
	}
	if resp := r.do(t, req{method: "GET", path: "/preview/authorize?return=https://" + previewHost + "/", bearer: tok}); resp.StatusCode != http.StatusFound ||
		!strings.HasPrefix(resp.Header.Get("Location"), "/signin?return=") {
		t.Errorf("a token at /preview/authorize = %d %q; want the sign-in redirect", resp.StatusCode, resp.Header.Get("Location"))
	}
	if resp, _ := r.previewDo(t, "GET", "https://"+previewHost+"/app", http.Header{"Authorization": {"Bearer " + tok}}); resp.StatusCode != http.StatusFound ||
		!strings.HasPrefix(resp.Header.Get("Location"), uiOrigin+"/preview/authorize?") {
		t.Errorf("a token on the preview socket = %d %q; want the handshake's redirect", resp.StatusCode, resp.Header.Get("Location"))
	}
	if resp := r.do(t, req{method: "GET", path: "/api/meta", bearer: tok, cookie: cookie}); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("cookie and bearer together = %d; want 401", resp.StatusCode)
	}
	if resp := r.do(t, req{method: "GET", path: "/api/nope", bearer: tok}); resp.StatusCode != http.StatusForbidden {
		t.Errorf("a token on an undeclared path = %d; want 403 (no scope)", resp.StatusCode)
	}

	if resp := r.do(t, req{method: "DELETE", path: "/api/tokens/" + id, origin: uiOrigin, cookie: cookie}); resp.StatusCode != http.StatusAccepted {
		t.Fatalf("revoke = %d", resp.StatusCode)
	}
	if resp := r.do(t, req{method: "GET", path: "/api/meta", bearer: tok}); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("a revoked token = %d; want 401", resp.StatusCode)
	}
	if resp := r.do(t, req{method: "GET", path: "/api/meta", bearer: approver}); resp.StatusCode != http.StatusOK {
		t.Errorf("control: the other token after the revoke = %d", resp.StatusCode)
	}
}

// TestTokenStreamHidesSessionKindsEndToEnd is §10.0a's: a read token's
// stream and its replay carry no secret.* or api_token.* event. Over one
// window a canary-named secret is created, granted and rotated, and a token
// minted, between workspace.state events; the operator's stream receives
// every one of them (the control), and the token's received the
// workspace.state events (it was live) and nothing naming the canary, live or
// replayed from before the window.
func TestTokenStreamHidesSessionKindsEndToEnd(t *testing.T) {
	key := make([]byte, 32)
	rand.Read(key)
	r := secretsServer(t, t.TempDir(), key)
	cookie := r.signIn(t)
	tok, _, _ := r.mintToken(t, cookie, "reader", "read")
	canary := "CANARY_" + strings.ToUpper(randomAlnum(20))

	op := r.record(t, req{cookie: cookie})
	bearer := r.record(t, req{bearer: tok})
	op.waitFor(t, ": connected")
	bearer.waitFor(t, ": connected")

	ctx := context.Background()
	marker := func(name string) events.Event {
		e, err := r.srv.Events.Emit(ctx, wsGranted, events.Info, events.KindWorkspaceState, "marker", map[string]string{"state": name})
		if err != nil {
			t.Fatal(err)
		}
		return e
	}
	c := secretsClient{t, r, cookie}
	first := marker("marker-a")
	if st, b := c.call("PUT", "/api/secrets/"+canary, `{"value":"v-one","reach":"a scratch bucket"}`); st != 200 {
		t.Fatalf("create = %d %s", st, b)
	}
	c.call("PUT", "/api/secrets/"+canary+"/grants", `{"repository_ids":[101]}`)
	marker("marker-b")
	if st, b := c.call("PUT", "/api/secrets/"+canary, `{"value":"v-two","reach":"a scratch bucket"}`); st != 200 {
		t.Fatalf("rotate = %d %s", st, b)
	}
	r.mintToken(t, cookie, "second "+canary, "read")
	last := marker("marker-c")

	op.waitFor(t, fmt.Sprintf("id: %d", last.ID))
	bearer.waitFor(t, fmt.Sprintf("id: %d", last.ID))
	for _, k := range []string{"secret.created", "secret.grants", "secret.rotated", "api_token.created", canary} {
		if !strings.Contains(op.text(), k) {
			t.Errorf("control: the operator's stream lacks %s", k)
		}
	}
	check := func(name, transcript string) {
		t.Helper()
		for _, m := range []string{"marker-a", "marker-b", "marker-c"} {
			if !strings.Contains(transcript, m) {
				t.Errorf("control: the %s lacks %s, so it was not live", name, m)
			}
		}
		for _, bad := range []string{canary, "secret.", "api_token."} {
			if strings.Contains(transcript, bad) {
				t.Errorf("the %s holds %q", name, bad)
			}
		}
	}
	check("token's live stream", bearer.text())

	// The replay, from before the window.
	resp := r.doStream(t, req{bearer: tok}, fmt.Sprint(first.ID-1))
	check("token's replay", readUntil(t, resp, ": connected"))
}

// doStream opens GET /api/events with Last-Event-ID.
func (r *running) doStream(t *testing.T, q req, lastEventID string) *http.Response {
	t.Helper()
	hr, _ := http.NewRequest("GET", "http://socket/api/events", nil)
	hr.Host = uiHost
	hr.Header.Set("Last-Event-ID", lastEventID)
	if q.bearer != "" {
		hr.Header.Set("Authorization", "Bearer "+q.bearer)
	}
	if q.cookie != "" {
		hr.AddCookie(&http.Cookie{Name: "__Host-drydock", Value: q.cookie})
	}
	resp, err := r.client.Do(hr)
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("stream: %v %v", err, resp)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

func readUntil(t *testing.T, resp *http.Response, line string) string {
	t.Helper()
	got := make(chan string, 1)
	go func() {
		var b strings.Builder
		sc := bufio.NewScanner(resp.Body)
		for sc.Scan() {
			b.WriteString(sc.Text() + "\n")
			if sc.Text() == line {
				break
			}
		}
		got <- b.String()
	}()
	select {
	case s := <-got:
		return s
	case <-time.After(10 * time.Second):
		t.Fatalf("no %q within 10s", line)
		return ""
	}
}

// captureStderr points this process's stderr at <dir>/stderr.log until the
// test ends, so a canary sweep of dir reads what the server wrote to stderr —
// the journal, in production — as the browser tier reads drydock.log. The
// server runs in-process and writes os.Stderr from several packages; swapping
// descriptors races with nothing, where assigning os.Stderr would race with
// every goroutine reading it. Nothing in the package runs in parallel.
//
// Two descriptors, because os.Stderr is not always fd 2: under `go test
// -json` (the nightly stress job, #177) the testing package sets os.Stderr =
// os.Stdout so test2json reads one ordered stream, and every write through
// os.Stderr lands on fd 1. So the descriptor behind os.Stderr is swapped, and
// fd 2 as well: the log package's default logger and any child that inherits
// it write there. Under -json the first also carries the test's own framing
// while the capture lasts; it is copied back, in order, before the test ends.
// On the way out what was captured is copied to the real stderr; a crash
// before then leaves it in the file, whose path is printed first.
func captureStderr(t *testing.T, dir string) {
	t.Helper()
	path := filepath.Join(dir, "stderr.log")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	fds := []int{2}
	if fd := int(os.Stderr.Fd()); fd != 2 {
		fds = append(fds, fd)
	}
	saved := make([]int, len(fds))
	for i, fd := range fds {
		if saved[i], err = syscall.Dup(fd); err != nil {
			t.Fatal(err)
		}
	}
	fmt.Fprintf(os.Stderr, "%s: the server's stderr is in %s until the test ends\n", t.Name(), path)
	for _, fd := range fds {
		if err := syscall.Dup3(int(f.Fd()), fd, 0); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		for i, fd := range fds {
			syscall.Dup3(saved[i], fd, 0)
			syscall.Close(saved[i])
		}
		f.Close()
		if b, _ := os.ReadFile(path); len(b) > 0 {
			os.Stderr.Write(b)
		}
	})
}

// TestTokenCanarySweep (testing §4.2, vscode-extension §10.0a): a token
// minted through the API and then used — meta, a list, the stream, a
// refusal for scope, revoking itself — is found nowhere under the server's
// temp root in any encoded form, nor in any process's argv. The one place it
// travelled is the mint's response (the control: the sweep's needle is in
// that body), and its SHA-256 is in the database (the expected hit, asserted
// present). The tree includes the server's stderr (captureStderr), and a
// canary written there is found by the same walk: the control that the walk
// reads it.
func TestTokenCanarySweep(t *testing.T) {
	dir := t.TempDir()
	captureStderr(t, dir)
	r := startIn(t, dir)
	cookie := r.signIn(t)
	tok, _, body := r.mintToken(t, cookie, "sweep", "read", "lifecycle")
	if !strings.Contains(body, tok) {
		t.Fatal("control: the mint's body does not hold the token")
	}
	r.do(t, req{method: "GET", path: "/api/meta", bearer: tok})
	r.do(t, req{method: "GET", path: "/api/workspaces", bearer: tok})
	r.do(t, req{method: "GET", path: "/api/secrets", bearer: tok})
	s := r.record(t, req{bearer: tok})
	s.waitFor(t, ": connected")
	r.do(t, req{method: "GET", path: "/api/tokens", cookie: cookie})
	if resp := r.do(t, req{method: "DELETE", path: "/api/auth/token", origin: uiOrigin, bearer: tok}); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("self-revoke = %d", resp.StatusCode)
	}
	r.stop()
	planted := "CANARY_" + randomAlnum(24)
	fmt.Fprintln(os.Stderr, "drydock: a planted line", planted)

	sum := sha256.Sum256([]byte(tok))
	dbBytes := ""
	for _, suffix := range []string{"", "-wal"} {
		b, _ := os.ReadFile(r.cfg.DatabasePath + suffix)
		dbBytes += string(b)
	}
	if !strings.Contains(dbBytes, hex.EncodeToString(sum[:])) {
		t.Fatal("control: the token's SHA-256 is not in the database; the sweep is not reading it")
	}
	needles := forms(tok)
	needles["the random part"] = strings.TrimPrefix(tok, "drydock_")
	var files int
	var plantedIn []string
	filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || d.Type()&fs.ModeSocket != 0 {
			return nil
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return nil
		}
		files++
		if strings.Contains(string(b), planted) {
			plantedIn = append(plantedIn, filepath.Base(p))
		}
		for form, n := range needles {
			if strings.Contains(string(b), n) {
				t.Errorf("%s holds the token (%s)", p, form)
			}
		}
		return nil
	})
	if files < 2 {
		t.Fatalf("control: the sweep read %d files", files)
	}
	if len(plantedIn) != 1 || plantedIn[0] != "stderr.log" {
		t.Fatalf("control: a canary written to the server's stderr was found in %v; want [stderr.log]", plantedIn)
	}
	procs, _ := os.ReadDir("/proc")
	for _, d := range procs {
		if b, err := os.ReadFile(filepath.Join("/proc", d.Name(), "cmdline")); err == nil && strings.Contains(string(b), tok) {
			t.Errorf("/proc/%s/cmdline holds the token", d.Name())
		}
	}
	for form, n := range needles {
		if strings.Contains(s.text(), n) {
			t.Errorf("the token's own stream holds it (%s)", form)
		}
	}
}
