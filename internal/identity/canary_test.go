package identity_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/krelinga/drydock/internal/api"
	"github.com/krelinga/drydock/internal/events"
	"github.com/krelinga/drydock/internal/identity"
	"github.com/krelinga/drydock/internal/life"
	"github.com/krelinga/drydock/internal/store"
	"github.com/krelinga/drydock/internal/sys"
)

type served struct {
	creds, status []byte
	statusErr     error
}

// canarySource serves whatever the current case says, and remembers every
// byte it handed out so the test can prove the canary was really read.
type canarySource struct {
	mu   sync.Mutex
	cur  served
	seen bytes.Buffer
}

func (s *canarySource) Credentials(context.Context) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.seen.Write(s.cur.creds)
	return s.cur.creds, nil
}

func (s *canarySource) AuthStatus(context.Context) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cur.status, s.cur.statusErr
}

// TestCanarySweep is testing §8's sweep for the credential: high-entropy fake
// tokens go into the credential file, the watch reads it through every path
// — a good verdict and each way a read can be refused — and afterwards the
// canary appears in no event, no byte of the database or anything else under
// the temp root, no service-log line, no HTTP response and no error string.
//
// The positive control is the sweep itself: the same sweep, pointed at a
// file and a log line the test plants the canary in, finds it — and the
// source proves it really handed the canary out.
func TestCanarySweep(t *testing.T) {
	b := make([]byte, 24)
	rand.Read(b)
	canary := "sk-ant-oat01-CANARY" + hex.EncodeToString(b)
	refresh := "sk-ant-ort01-CANARY" + hex.EncodeToString(b[:12])
	needles := []string{canary, refresh, hex.EncodeToString(b)[:16]}

	root := t.TempDir()
	db, err := store.Open(context.Background(), filepath.Join(root, "drydock.db"))
	if err != nil {
		t.Fatal(err)
	}
	clock := sys.NewFakeClock(time.Date(2026, 10, 4, 6, 14, 37, 0, time.UTC))
	log := events.New(db.DB, clock)
	src := &canarySource{}
	var logged strings.Builder
	var mu sync.Mutex
	w := &identity.Watch{DB: db.DB, Events: log, Clock: clock, Source: src, Volume: "drydock-claude-config",
		Window: 72 * time.Hour,
		Logf: func(f string, a ...any) {
			mu.Lock()
			defer mu.Unlock()
			logged.WriteString(sprintf(f, a...) + "\n")
		}}
	// Started as Serve starts it, boot check included; stopped and waited
	// for before the database closes below.
	group := life.NewGroup(context.Background())
	t.Cleanup(func() { group.Wait(nil) })
	if err := w.Start(group); err != nil {
		t.Fatal(err)
	}
	read := api.ClaudeRoutes{Watch: w}.Handlers()["claude.identity.read"]

	future := clock.Now().Add(30 * 24 * time.Hour).UnixMilli()
	live := []byte(`{"claudeAiOauth":{"accessToken":"` + canary + `","refreshToken":"` + refresh +
		`","expiresAt":` + itoa(future) + `,"scopes":["user:inference"]}}`)
	valid := []byte(`{"loggedIn":true,"authMethod":"claude.ai","email":"fixture@example.invalid"}`)
	cases := []served{
		{creds: live, status: valid},
		{creds: live, status: nil, statusErr: &identity.ReadError{Problem: identity.ProblemImage, Detail: "no network"}},
		{creds: live, status: []byte(`{"loggedIn":false}`)},
		// The token unquoted: a syntax error at its first character.
		{creds: []byte(`{"claudeAiOauth":{"accessToken":` + canary + `}}`), status: valid},
		// The token where a number belongs: a type error.
		{creds: []byte(`{"claudeAiOauth":{"accessToken":"` + canary + `","refreshToken":"` + refresh + `","expiresAt":"` + canary + `"}}`), status: valid},
		// Partially blanked: one live token.
		{creds: []byte(`{"claudeAiOauth":{"accessToken":"` + canary + `","refreshToken":"","expiresAt":` + itoa(future) + `}}`), status: valid},
		// Truncated mid-token.
		{creds: live[:len(live)/2], status: valid},
	}
	var errs, bodies strings.Builder
	for _, c := range cases {
		clock.Advance(time.Minute)
		src.mu.Lock()
		src.cur = c
		src.mu.Unlock()
		if _, err := w.Check(context.Background()); err != nil {
			errs.WriteString(err.Error() + "\n")
		}
		rec := httptest.NewRecorder()
		read(rec, httptest.NewRequest(http.MethodGet, "/api/auth/claude", nil))
		bodies.Write(rec.Body.Bytes())
	}
	if errs.Len() == 0 || !strings.Contains(bodies.String(), `"state":"ok"`) {
		t.Fatalf("the cases did not exercise both paths: errors %q, bodies %q", errs.String(), bodies.String())
	}
	evs, err := log.Since(context.Background(), 0)
	if err != nil {
		t.Fatal(err)
	}
	var evText strings.Builder
	for _, e := range evs {
		evText.WriteString(e.Message + string(e.Data) + "\n")
	}
	group.Wait(nil)
	db.Close() // flush the WAL, so the files are everything that was written

	if !bytes.Contains(src.seen.Bytes(), []byte(canary)) {
		t.Fatal("control: the source never served the canary")
	}
	sinks := map[string]string{"errors": errs.String(), "HTTP": bodies.String(), "events": evText.String(), "log": logged.String()}
	sweep := func(dir string, sinks map[string]string) []string {
		var hits []string
		filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return err
			}
			raw, _ := os.ReadFile(p)
			for _, n := range needles {
				if bytes.Contains(raw, []byte(n)) {
					hits = append(hits, p)
				}
			}
			return nil
		})
		for name, s := range sinks {
			for _, n := range needles {
				if strings.Contains(s, n) {
					hits = append(hits, name)
				}
			}
		}
		return hits
	}
	if hits := sweep(root, sinks); len(hits) > 0 {
		t.Errorf("the canary reached: %v", hits)
	}

	// Control: the sweep finds a planted canary, in a file and in a sink.
	planted := t.TempDir()
	os.WriteFile(filepath.Join(planted, "x.db-wal"), append([]byte("\x00\x01"), canary...), 0o600)
	if hits := sweep(planted, map[string]string{"log": "line " + canary[:40] + refresh}); len(hits) < 2 {
		t.Errorf("control: the sweep missed a planted canary: %v", hits)
	}
}
