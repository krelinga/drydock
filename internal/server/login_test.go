package server

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
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
	"testing"
	"time"

	"github.com/krelinga/drydock/internal/claudetest"
	"github.com/krelinga/drydock/internal/login/logintest"
	"github.com/krelinga/drydock/internal/sys"
)

// okSource is a volume with a live login on it, for the check a successful
// handshake triggers.
type okSource struct{ creds []byte }

func (o okSource) Credentials(context.Context) ([]byte, error) { return o.creds, nil }
func (o okSource) AuthStatus(context.Context) ([]byte, error) {
	return []byte(`{"loggedIn":true,"authMethod":"claude.ai","email":"op@example.invalid","orgId":"org"}`), nil
}

type loginBody struct {
	Identity struct {
		State      *string `json:"state"`
		LoggedInAt *string `json:"logged_in_at"`
	} `json:"identity"`
	Login *struct {
		ID       string  `json:"login_id"`
		Phase    string  `json:"phase"`
		URL      *string `json:"url"`
		EndedAt  *string `json:"ended_at"`
		Attempts int     `json:"attempts"`
	} `json:"login"`
}

// TestLoginHandshakeEndToEnd is the handshake through the real gate, socket,
// routes, event stream and identity watch, with fakeclaude on the PTY in
// place of the container. A wrong code loops back, a half code is refused
// before the PTY with its rule named, a right one succeeds, the watch records
// the login's own moment, and a second start while one runs is 409.
//
// Then the canary sweep (testing §4.2), over every sink the code passed
// near: every HTTP response the server sent on any route — the path that
// carries the code is the one CLAUDE.md names as a sink that is not a file
// — the SSE transcript, every file under the temp root (the database and its
// WAL, the fake's state), the service log, every request URL, and every
// process's argv. The positive controls: fakeclaude confirms both codes
// arrived on its stdin, by hash; and the sweep finds a canary planted in a
// response it read.
func TestLoginHandshakeEndToEnd(t *testing.T) {
	dir := t.TempDir()
	cfg := testConfig(t, dir)
	srv, err := New(context.Background(), cfg, sys.Production())
	if err != nil {
		t.Fatal(err)
	}
	ok, err := os.ReadFile(filepath.Join("..", "..", "test", "fixtures", "credentials", "ok.json"))
	if err != nil {
		t.Fatal(err)
	}
	srv.Identity.Source = okSource{creds: ok}

	rnd := func() string { b := make([]byte, 16); rand.Read(b); return hex.EncodeToString(b) }
	good := "cnryGOOD" + rnd() + "#cnryGSTATE" + rnd()
	bad := "cnryBAD" + rnd() + "#cnryBSTATE" + rnd()
	fake := claudetest.Install(t, claudetest.Script{
		StateDir: filepath.Join(dir, "fake-state"),
		Login:    &claudetest.Login{Mode: claudetest.LoginAnswer, AcceptSHA256: claudetest.CodeSHA256(good)},
	})
	cfgDir := filepath.Join(dir, "claude-config")
	os.MkdirAll(cfgDir, 0o700)
	launcher := &logintest.Launcher{Fake: fake, ConfigDir: cfgDir}
	srv.Login.Launcher = launcher
	var logMu sync.Mutex
	var logged bytes.Buffer
	srv.Login.Logf = func(f string, a ...any) { logMu.Lock(); fmt.Fprintf(&logged, f+"\n", a...); logMu.Unlock() }

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- srv.Serve(ctx) }()
	t.Cleanup(func() { cancel(); <-done })
	r := &running{cfg: cfg, srv: srv, client: unixClient(cfg.APISocket)}
	cookie := r.signIn(t)

	// Everything the server says, kept for the sweep.
	var heardMu sync.Mutex
	var heard bytes.Buffer
	var urls []string
	call := func(q req) (int, []byte) {
		t.Helper()
		q.cookie = cookie
		if q.method != "GET" {
			q.origin = uiOrigin
		}
		urls = append(urls, q.path)
		resp := r.do(t, q)
		b, _ := io.ReadAll(resp.Body)
		heardMu.Lock()
		heard.Write(b)
		heardMu.Unlock()
		return resp.StatusCode, b
	}

	// The event stream, transcribed.
	streamCtx, stopStream := context.WithCancel(ctx)
	defer stopStream()
	sreq, _ := http.NewRequestWithContext(streamCtx, "GET", "http://socket/api/events", nil)
	sreq.Host = uiHost
	sreq.AddCookie(&http.Cookie{Name: "__Host-drydock", Value: cookie})
	sresp, err := r.client.Do(sreq)
	if err != nil || sresp.StatusCode != 200 {
		t.Fatalf("events: %v", err)
	}
	streamDone := make(chan struct{})
	go func() {
		defer close(streamDone)
		sc := bufio.NewScanner(sresp.Body)
		sc.Buffer(make([]byte, 1<<20), 1<<20)
		for sc.Scan() {
			heardMu.Lock()
			heard.Write(sc.Bytes())
			heard.WriteByte('\n')
			heardMu.Unlock()
		}
	}()

	read := func() loginBody {
		t.Helper()
		code, b := call(req{method: "GET", path: "/api/auth/claude"})
		if code != 200 {
			t.Fatalf("GET /api/auth/claude = %d", code)
		}
		var v loginBody
		if err := json.Unmarshal(b, &v); err != nil {
			t.Fatal(err)
		}
		return v
	}
	until := func(what string, ok func(loginBody) bool) loginBody {
		t.Helper()
		for deadline := time.Now().Add(15 * time.Second); ; time.Sleep(20 * time.Millisecond) {
			if v := read(); ok(v) {
				return v
			}
			if time.Now().After(deadline) {
				t.Fatalf("never %s: %+v", what, read().Login)
			}
		}
	}
	phaseIs := func(p string) func(loginBody) bool {
		return func(v loginBody) bool { return v.Login != nil && v.Login.Phase == p }
	}

	if v := read(); v.Login != nil {
		t.Fatalf("a login before any was started: %+v", v.Login)
	}
	code, b := call(req{method: "POST", path: "/api/auth/claude/login"})
	var begun struct {
		LoginID string `json:"login_id"`
	}
	json.Unmarshal(b, &begun)
	if code != http.StatusAccepted || begun.LoginID == "" {
		t.Fatalf("begin = %d %s", code, b)
	}
	if code, b := call(req{method: "POST", path: "/api/auth/claude/login"}); code != http.StatusConflict || !strings.Contains(string(b), `"in_progress"`) {
		t.Errorf("a second begin = %d %s; want 409 in_progress", code, b)
	}
	aw := until("awaiting the code", phaseIs("awaiting_code"))
	if aw.Login.ID != begun.LoginID || aw.Login.URL == nil || !strings.HasPrefix(*aw.Login.URL, "https://claude.com/cai/oauth/authorize?") {
		t.Fatalf("awaiting: %+v", aw.Login)
	}
	codePath := "/api/auth/claude/login/" + begun.LoginID + "/code"
	body := func(c string) string { j, _ := json.Marshal(map[string]string{"code": c}); return string(j) }

	// Without Origin, refused like every mutation — and the code in that
	// body goes nowhere either.
	if resp := r.do(t, req{method: "POST", path: codePath, cookie: cookie, body: body(bad)}); resp.StatusCode != http.StatusForbidden {
		t.Errorf("code without Origin = %d", resp.StatusCode)
	}
	half, _, _ := strings.Cut(bad, "#")
	if code, b := call(req{method: "POST", path: codePath, body: body(half)}); code != 400 ||
		!strings.Contains(string(b), `"login_code_invalid"`) || !strings.Contains(string(b), `"no_separator"`) {
		t.Errorf("half a code = %d %s", code, b)
	}
	for _, malformed := range []string{`{"code":1}`, `{"code":"a#b","x":1}`, `{"code":"a#b"} {}`, `not json ` + bad, ``} {
		if code, _ := call(req{method: "POST", path: codePath, body: malformed}); code != 400 {
			t.Errorf("body %q = %d; want 400", malformed, code)
		}
	}
	if code, _ := call(req{method: "POST", path: "/api/auth/claude/login/000000000000000000000000/code", body: body(bad)}); code != 404 {
		t.Errorf("unknown login = %d", code)
	}
	if code, _ := call(req{method: "POST", path: codePath, body: body(bad)}); code != http.StatusAccepted {
		t.Fatalf("wrong code = %d", code)
	}
	inv := until("invalid", phaseIs("invalid_code"))
	if *inv.Login.URL != *aw.Login.URL {
		t.Error("a wrong code changed the URL")
	}
	if code, _ := call(req{method: "POST", path: codePath, body: body(good)}); code != http.StatusAccepted {
		t.Fatalf("right code = %d", code)
	}
	final := until("signed in, recorded", func(v loginBody) bool {
		return v.Login != nil && v.Login.Phase == "succeeded" && v.Identity.LoggedInAt != nil && v.Login.EndedAt != nil &&
			*v.Identity.LoggedInAt == *v.Login.EndedAt
	})
	if final.Identity.State == nil || *final.Identity.State == "absent" || final.Login.Attempts != 2 {
		t.Errorf("after the login: %+v %+v", final.Identity, final.Login)
	}
	if code, b := call(req{method: "DELETE", path: "/api/auth/claude/login/" + begun.LoginID}); code != 409 || !strings.Contains(string(b), `"login_ended"`) {
		t.Errorf("cancel after the end = %d %s", code, b)
	}
	if code, _ := call(req{method: "POST", path: codePath, body: body(good)}); code != 409 {
		t.Errorf("a code after the end = %d", code)
	}

	// Control: both codes reached the PTY.
	subs := claudetest.Kind(fake.Events(t), claudetest.EventSubmission)
	if len(subs) != 2 || subs[0].SHA256 != claudetest.CodeSHA256(bad) || subs[1].SHA256 != claudetest.CodeSHA256(good) {
		t.Fatalf("control: fakeclaude saw %+v", subs)
	}
	fake.NoViolations(t)

	// The sweep.
	time.Sleep(100 * time.Millisecond) // the stream's last frames
	stopStream()
	<-streamDone
	srv.DB.DB.Exec(`PRAGMA wal_checkpoint(FULL)`)
	heardMu.Lock()
	said := heard.String()
	heardMu.Unlock()
	logMu.Lock()
	log := logged.String()
	logMu.Unlock()
	if !strings.Contains(said, "auth.login") || !strings.Contains(said, "invalid_code") {
		t.Fatalf("control: the transcript holds no login events:\n%.400s", said)
	}
	// sinks names every sink holding needle.
	sinks := func(needle string) []string {
		var where []string
		if strings.Contains(said, needle) {
			where = append(where, "what the server sent")
		}
		if strings.Contains(log, needle) {
			where = append(where, "the service log")
		}
		if strings.Contains(strings.Join(urls, "\n"), needle) {
			where = append(where, "a request URL")
		}
		filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() || d.Type()&fs.ModeSocket != 0 {
				return nil
			}
			if b, err := os.ReadFile(p); err == nil && bytes.Contains(b, []byte(needle)) {
				where = append(where, p)
			}
			return nil
		})
		procs, _ := os.ReadDir("/proc")
		for _, d := range procs {
			if b, err := os.ReadFile(filepath.Join("/proc", d.Name(), "cmdline")); err == nil && bytes.Contains(b, []byte(needle)) {
				where = append(where, "/proc/"+d.Name()+"/cmdline")
			}
		}
		return where
	}
	for _, c := range []string{good, bad} {
		x, y, _ := strings.Cut(c, "#")
		for _, part := range []string{c, x, y} {
			if where := sinks(part); len(where) > 0 {
				t.Errorf("a login code is in %v", where)
			}
		}
	}
	// Control: the same sweep finds the login id where it really is — in
	// the responses and the stream, and in the database's event rows.
	where := strings.Join(sinks(begun.LoginID), "\n")
	if !strings.Contains(where, "what the server sent") || !strings.Contains(where, "drydock.db") {
		t.Errorf("control: the sweep found the login id only in %q", where)
	}
}
