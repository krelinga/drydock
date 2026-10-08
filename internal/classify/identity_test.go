package classify

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The credential fixtures hold ABSOLUTE expiresAt values, computed when they
// were recorded. Every `now` in this file is therefore derived from fixture
// data — the `.meta`'s recorded_at — and never from the wall clock, or
// "expiring" would quietly become "expired" two days after recording and the
// suite would rot. TestClassifyIdentityDoesNotDependOnWhenItRuns pins that.

func identityFixturePath(kind, name string) string {
	return filepath.Join("..", "..", "test", "fixtures", kind, name)
}

// loadIdentityFixture returns the bytes of a fixture and the recorded_at from
// its .meta sidecar. The credentials "absent" case is a marker file meaning
// "the file does not exist", so it yields nil bytes — exactly what the poller
// passes when os.ReadFile reports ErrNotExist.
func loadIdentityFixture(t *testing.T, kind, name string) ([]byte, time.Time) {
	t.Helper()
	file := name + ".json"
	if kind == "credentials" && name == "absent" {
		file = "absent.txt"
	}
	path := identityFixturePath(kind, file)
	meta, err := os.Open(path + ".meta")
	if err != nil {
		t.Fatalf("fixture %s has no .meta: %v", path, err)
	}
	defer meta.Close()
	var recorded time.Time
	sc := bufio.NewScanner(meta)
	for sc.Scan() {
		k, v, ok := strings.Cut(sc.Text(), ":")
		if ok && strings.TrimSpace(k) == "recorded_at" {
			recorded, err = time.Parse(time.RFC3339, strings.TrimSpace(v))
			if err != nil {
				t.Fatalf("%s.meta: recorded_at: %v", path, err)
			}
		}
	}
	if recorded.IsZero() {
		t.Fatalf("%s.meta carries no recorded_at", path)
	}
	if file == "absent.txt" {
		marker, err := os.ReadFile(path)
		if err != nil || !bytes.Contains(marker, []byte("Pass nil")) {
			t.Fatalf("absent marker %s missing or no longer says to pass nil: %v", path, err)
		}
		return nil, recorded
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b, recorded
}

// fixtureExpiresAt reads claudeAiOauth.expiresAt straight out of a fixture, so
// tests can reason about it relative to recorded_at.
func fixtureExpiresAt(t *testing.T, b []byte) time.Time {
	t.Helper()
	e, _ := fixtureDates(t, b)
	return e
}

// fixtureDates reads both dates out of a fixture: expiresAt (the access
// token's) and refreshTokenExpiresAt (the login's), the zero time when the
// file has none.
func fixtureDates(t *testing.T, b []byte) (access, login time.Time) {
	t.Helper()
	var c struct {
		O struct {
			E int64  `json:"expiresAt"`
			L *int64 `json:"refreshTokenExpiresAt"`
		} `json:"claudeAiOauth"`
	}
	if err := json.Unmarshal(b, &c); err != nil {
		t.Fatal(err)
	}
	if c.O.L != nil {
		login = time.UnixMilli(*c.O.L).UTC()
	}
	return time.UnixMilli(c.O.E).UTC(), login
}

// synthCreds builds a credential document in the recorded shape, with no
// refreshTokenExpiresAt. The token values are obviously fake; this
// repository is public.
func synthCreds(access, refresh string, expiresAt time.Time) []byte {
	return synthLogin(access, refresh, expiresAt, time.Time{})
}

// synthLogin is synthCreds with a refreshTokenExpiresAt, as 2.1.289 writes at
// every login; the zero time leaves it out.
func synthLogin(access, refresh string, expiresAt, loginExpiresAt time.Time) []byte {
	rt := ""
	if !loginExpiresAt.IsZero() {
		rt = fmt.Sprintf(`"refreshTokenExpiresAt":%d,`, loginExpiresAt.UnixMilli())
	}
	return []byte(fmt.Sprintf(`{"claudeAiOauth":{"accessToken":%q,"refreshToken":%q,"expiresAt":%d,%s"scopes":["user:inference"],"subscriptionType":"max"}}`,
		access, refresh, expiresAt.UnixMilli(), rt))
}

const fakeAccess, fakeRefresh = "sk-ant-oat01-TEST-FAKE", "sk-ant-ort01-TEST-FAKE"

var stateName = map[IdentityState]string{
	IdentityOK: "ok", IdentityExpiring: "expiring", IdentityExpired: "expired",
	IdentityBlanked: "blanked", IdentityAbsent: "absent",
}

func (s IdentityState) testString() string {
	if n, ok := stateName[s]; ok {
		return n
	}
	return fmt.Sprintf("IdentityState(%d)", s)
}

// TestClassifyIdentityThreeJoins is testing §7 "Identity": the three joins that
// carry the whole point of §7.3, each a separately named assertion.
func TestClassifyIdentityThreeJoins(t *testing.T) {
	t.Run("loggedIn:true with past expiresAt is expired, not ok", func(t *testing.T) {
		auth, _ := loadIdentityFixture(t, "authstatus", "expired")
		creds, now := loadIdentityFixture(t, "credentials", "expired")
		if !bytes.Contains(auth, []byte(`"loggedIn": true`)) {
			t.Fatal("precondition: the recorded auth status for an expired credential must say loggedIn:true — that is the trap")
		}
		got, err := ClassifyIdentity(auth, creds, now)
		if err != nil {
			t.Fatal(err)
		}
		if got.State != IdentityExpired {
			t.Errorf("state = %s, want expired: a classifier that trusts loggedIn reports a healthy login", got.State.testString())
		}
		if want := fixtureExpiresAt(t, creds); !got.ExpiresAt.Equal(want) {
			t.Errorf("ExpiresAt = %v, want %v from the file", got.ExpiresAt, want)
		}
		// Positive control: the identical auth status beside a healthy file is
		// ok, so "expired" came from the file and is not a constant.
		okCreds, _ := loadIdentityFixture(t, "credentials", "ok")
		ctl, err := ClassifyIdentity(auth, okCreds, now)
		if err != nil || ctl.State != IdentityOK {
			t.Errorf("control: same auth status + ok file = %s, %v; want ok", ctl.State.testString(), err)
		}
	})

	t.Run("loggedIn:false with empty tokens is blanked", func(t *testing.T) {
		auth, _ := loadIdentityFixture(t, "authstatus", "blanked")
		creds, now := loadIdentityFixture(t, "credentials", "blanked")
		got, err := ClassifyIdentity(auth, creds, now)
		if err != nil {
			t.Fatal(err)
		}
		if got.State != IdentityBlanked {
			t.Errorf("state = %s, want blanked: every workspace just died", got.State.testString())
		}
		if !got.ExpiresAt.IsZero() {
			t.Errorf("blanked ExpiresAt = %v, want zero (not 1970 from expiresAt:0)", got.ExpiresAt)
		}
	})

	t.Run("loggedIn:false with no file is absent", func(t *testing.T) {
		auth, _ := loadIdentityFixture(t, "authstatus", "absent")
		creds, now := loadIdentityFixture(t, "credentials", "absent")
		if creds != nil {
			t.Fatal("precondition: the absent case is nil credentials")
		}
		got, err := ClassifyIdentity(auth, creds, now)
		if err != nil {
			t.Fatal(err)
		}
		if got.State != IdentityAbsent {
			t.Errorf("state = %s, want absent: nobody has signed in yet", got.State.testString())
		}
	})

	t.Run("blanked and absent share the auth status; only the file separates them", func(t *testing.T) {
		absentAuth, _ := loadIdentityFixture(t, "authstatus", "absent")
		blankedAuth, _ := loadIdentityFixture(t, "authstatus", "blanked")
		// The recorded documents are byte-identical, which is the finding.
		if !bytes.Equal(absentAuth, blankedAuth) {
			t.Log("note: the recorded absent and blanked auth status documents now differ; the join below still must hold")
		}
		blankedCreds, now := loadIdentityFixture(t, "credentials", "blanked")
		b, err1 := ClassifyIdentity(blankedAuth, blankedCreds, now)
		a, err2 := ClassifyIdentity(blankedAuth, nil, now)
		if err1 != nil || err2 != nil {
			t.Fatal(err1, err2)
		}
		if a.State == b.State {
			t.Errorf("blanked and absent both classified %s: the worst failure and a first run must not look alike", a.State.testString())
		}
		if a.State != IdentityAbsent || b.State != IdentityBlanked {
			t.Errorf("got absent=%s blanked=%s", a.State.testString(), b.State.testString())
		}
	})
}

// TestClassifyIdentityMatrix joins all four recorded auth status documents with
// all seven credential shapes (testing §7). An empty want means an error.
func TestClassifyIdentityMatrix(t *testing.T) {
	auths := []string{"absent", "valid", "expired", "blanked"}
	creds := []string{"ok", "fresh-login", "expiring", "expired", "blanked", "absent", "corrupt"}
	// Rows: credential shape. Columns: auth fixture, in the order above.
	// "error" for live tokens beside loggedIn:false is rule 7.
	want := map[string][4]string{
		"ok":          {"error", "ok", "ok", "error"},
		"fresh-login": {"error", "ok", "ok", "error"},
		"expiring":    {"error", "expiring", "expiring", "error"},
		"expired":     {"error", "expired", "expired", "error"},
		"blanked":     {"blanked", "blanked", "blanked", "blanked"},
		"absent":      {"absent", "absent", "absent", "absent"},
		"corrupt":     {"error", "error", "error", "error"},
	}
	var sawVerdict, sawError bool
	for _, c := range creds {
		credBytes, now := loadIdentityFixture(t, "credentials", c)
		for i, a := range auths {
			authBytes, _ := loadIdentityFixture(t, "authstatus", a)
			w := want[c][i]
			t.Run("creds="+c+"/auth="+a, func(t *testing.T) {
				got, err := ClassifyIdentity(authBytes, credBytes, now)
				if w == "error" {
					if err == nil {
						t.Errorf("got %s, want an error", got.State.testString())
					}
					sawError = true
					return
				}
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				if got.State.testString() != w {
					t.Errorf("state = %s, want %s", got.State.testString(), w)
				}
				sawVerdict = true
			})
		}
	}
	// Positive control: the matrix exercised both outcomes, so the error
	// rows are not passing because everything errors.
	if !sawVerdict || !sawError {
		t.Errorf("matrix saw verdict=%t error=%t; want both", sawVerdict, sawError)
	}
}

// TestClassifyIdentityDoesNotDependOnWhenItRuns pins the fixture hazard: the
// verdicts are a function of expiresAt − now and refreshTokenExpiresAt − now,
// and every now here comes from recorded_at. Translating all three by decades
// in either direction changes nothing.
func TestClassifyIdentityDoesNotDependOnWhenItRuns(t *testing.T) {
	auth, _ := loadIdentityFixture(t, "authstatus", "valid")
	shifts := []time.Duration{0, -20 * 365 * 24 * time.Hour, 50 * 365 * 24 * time.Hour, 7 * time.Minute}
	for _, c := range []struct{ name, want string }{
		{"ok", "ok"}, {"fresh-login", "ok"}, {"expiring", "expiring"}, {"expired", "expired"},
	} {
		name := c.name
		creds, recorded := loadIdentityFixture(t, "credentials", name)
		access, login := fixtureDates(t, creds)
		delta := access.Sub(recorded)
		base, err := ClassifyIdentity(auth, creds, recorded)
		if err != nil {
			t.Fatal(err)
		}
		if base.State.testString() != c.want {
			t.Fatalf("%s fixture at its own recorded_at = %s; the .meta's must_yield (%s) no longer holds", name, base.State.testString(), c.want)
		}
		for _, s := range shifts {
			now := recorded.Add(s)
			var shiftedLogin time.Time
			if !login.IsZero() {
				shiftedLogin = now.Add(login.Sub(recorded))
			}
			got, err := ClassifyIdentity(auth, synthLogin(fakeAccess, fakeRefresh, now.Add(delta), shiftedLogin), now)
			if err != nil {
				t.Fatal(err)
			}
			if got.State != base.State {
				t.Errorf("%s shifted by %v = %s, want %s", name, s, got.State.testString(), base.State.testString())
			}
		}
	}

	// Positive control: the hazard is real. The same expiring fixture,
	// judged three days after it was recorded — which is what a wall-clock
	// now would do on a later run — is no longer expiring: both its dates
	// have passed. Invariance above is therefore not vacuous.
	creds, recorded := loadIdentityFixture(t, "credentials", "expiring")
	later, err := ClassifyIdentity(auth, creds, recorded.Add(3*24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if later.State != IdentityExpired {
		t.Errorf("expiring fixture three days on = %s; want expired, or the hazard this test guards is not real", later.State.testString())
	}

	// And nothing here or in the implementation reads the wall clock.
	call := "time." + "Now("
	for _, f := range []string{"identity.go", "identity_test.go"} {
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(src, []byte(call)) {
			t.Errorf("%s calls %s); derive now from fixture data", f, call)
		}
		// Control: the scan read the real file.
		if !bytes.Contains(src, []byte("ClassifyIdentity(")) {
			t.Errorf("%s does not mention ClassifyIdentity; the scan read the wrong file", f)
		}
	}
}

// TestClassifyIdentityBoundaries: the access token's expiry has one boundary
// — exactly now is expired, one millisecond ahead is ok, never expiring — and
// the login's has two: exactly three days out is still expiring, and exactly
// now is no longer (Claude Code's own `m > 3d || m <= 0`).
func TestClassifyIdentityBoundaries(t *testing.T) {
	auth, _ := loadIdentityFixture(t, "authstatus", "valid")
	_, now := loadIdentityFixture(t, "credentials", "ok")
	ms := time.Millisecond
	if ExpiringWindow != 72*time.Hour {
		t.Fatalf("ExpiringWindow = %v; §7.3 (and Claude Code's own notice) says three days", ExpiringWindow)
	}

	t.Run("the access token alone", func(t *testing.T) {
		for _, c := range []struct {
			name  string
			delta time.Duration
			want  IdentityState
		}{
			{"one ms ago", -ms, IdentityExpired},
			{"exactly now", 0, IdentityExpired},
			{"one ms ahead", ms, IdentityOK},
			{"eight hours (a real access token)", 8 * time.Hour, IdentityOK},
			{"exactly three days", ExpiringWindow, IdentityOK},
			{"a year out", 365 * 24 * time.Hour, IdentityOK},
		} {
			t.Run(c.name, func(t *testing.T) {
				exp := now.Add(c.delta)
				got, err := ClassifyIdentity(auth, synthCreds(fakeAccess, fakeRefresh, exp), now)
				if err != nil {
					t.Fatal(err)
				}
				if got.State != c.want {
					t.Errorf("state = %s, want %s", got.State.testString(), c.want.testString())
				}
				if !got.ExpiresAt.Equal(exp) || !got.LoginExpiresAt.IsZero() {
					t.Errorf("ExpiresAt = %v, LoginExpiresAt = %v; want %v and zero", got.ExpiresAt, got.LoginExpiresAt, exp)
				}
			})
		}
	})

	t.Run("the login, beside an eight-hour access token", func(t *testing.T) {
		access := now.Add(8 * time.Hour)
		for _, c := range []struct {
			name  string
			delta time.Duration
			want  IdentityState
		}{
			{"one ms ago", -ms, IdentityOK},
			{"exactly now", 0, IdentityOK},
			{"one ms ahead", ms, IdentityExpiring},
			{"one ms inside three days", ExpiringWindow - ms, IdentityExpiring},
			{"exactly three days", ExpiringWindow, IdentityExpiring},
			{"one ms past three days", ExpiringWindow + ms, IdentityOK},
			{"thirty days (2.1.289's default)", 30 * 24 * time.Hour, IdentityOK},
		} {
			t.Run(c.name, func(t *testing.T) {
				login := now.Add(c.delta)
				got, err := ClassifyIdentity(auth, synthLogin(fakeAccess, fakeRefresh, access, login), now)
				if err != nil {
					t.Fatal(err)
				}
				if got.State != c.want {
					t.Errorf("state = %s, want %s", got.State.testString(), c.want.testString())
				}
				if !got.ExpiresAt.Equal(access) || !got.LoginExpiresAt.Equal(login) {
					t.Errorf("ExpiresAt = %v, LoginExpiresAt = %v; want %v, %v", got.ExpiresAt, got.LoginExpiresAt, access, login)
				}
			})
		}
	})

	t.Run("a login expiring outranks a lapsed access token", func(t *testing.T) {
		got, err := ClassifyIdentity(auth, synthLogin(fakeAccess, fakeRefresh, now.Add(-time.Hour), now.Add(24*time.Hour)), now)
		if err != nil || got.State != IdentityExpiring {
			t.Errorf("lapsed access, login a day out = %s, %v; want expiring", got.State.testString(), err)
		}
		// Control: the same lapsed token with the login far off is expired.
		got, err = ClassifyIdentity(auth, synthLogin(fakeAccess, fakeRefresh, now.Add(-time.Hour), now.Add(30*24*time.Hour)), now)
		if err != nil || got.State != IdentityExpired {
			t.Errorf("control: lapsed access, login thirty days out = %s, %v; want expired", got.State.testString(), err)
		}
	})

	t.Run("an access token outliving the login by more than the window", func(t *testing.T) {
		// Claude Code's own guard: then the recorded login date is not what
		// bounds this login, and it says nothing.
		login := now.Add(24 * time.Hour)
		got, err := ClassifyIdentity(auth, synthLogin(fakeAccess, fakeRefresh, login.Add(ExpiringWindow+ms), login), now)
		if err != nil || got.State != IdentityOK {
			t.Errorf("access past login+window = %s, %v; want ok", got.State.testString(), err)
		}
		got, err = ClassifyIdentity(auth, synthLogin(fakeAccess, fakeRefresh, login.Add(ExpiringWindow), login), now)
		if err != nil || got.State != IdentityExpiring {
			t.Errorf("control: access at exactly login+window = %s, %v; want expiring", got.State.testString(), err)
		}
	})
}

// TestAnEightHourAccessTokenIsNotTheLoginExpiring is the bug the real
// deployment found on 2026-10-08: a fresh login's expiresAt was about eight
// hours out, because it dates the access token, and the old rule — expiresAt
// within three days — called every login "expiring" from the moment it was
// made. Every hour of a live access token's life is ok when the login is
// not ending, with or without a recorded refreshTokenExpiresAt.
func TestAnEightHourAccessTokenIsNotTheLoginExpiring(t *testing.T) {
	auth, _ := loadIdentityFixture(t, "authstatus", "valid")
	fresh, now := loadIdentityFixture(t, "credentials", "fresh-login")
	access, login := fixtureDates(t, fresh)
	if d := access.Sub(now); d != 8*time.Hour {
		t.Fatalf("precondition: fresh-login's access token is %v out; want the measured 8h", d)
	}
	if d := login.Sub(now); d != 30*24*time.Hour {
		t.Fatalf("precondition: fresh-login's login is %v out; want 2.1.289's 30 days", d)
	}
	got, err := ClassifyIdentity(auth, fresh, now)
	if err != nil || got.State != IdentityOK {
		t.Fatalf("fresh-login fixture = %s, %v; want ok", got.State.testString(), err)
	}
	for h := 1; h < 72; h++ {
		exp := now.Add(time.Duration(h) * time.Hour)
		for name, creds := range map[string][]byte{
			"without refreshTokenExpiresAt": synthCreds(fakeAccess, fakeRefresh, exp),
			"with the login 30 days out":    synthLogin(fakeAccess, fakeRefresh, exp, now.Add(30*24*time.Hour)),
		} {
			got, err := ClassifyIdentity(auth, creds, now)
			if err != nil || got.State != IdentityOK {
				t.Errorf("access token %dh out, %s = %s, %v; want ok", h, name, got.State.testString(), err)
			}
		}
	}

	// Positive control: the same eight-hour access token beside a login
	// that really ends in two days is expiring — the verdict exists, and it
	// comes from the login's date.
	soon, _ := loadIdentityFixture(t, "credentials", "expiring")
	if a, _ := fixtureDates(t, soon); !a.Equal(access) {
		t.Fatalf("precondition: the expiring fixture's access token (%v) is not fresh-login's (%v)", a, access)
	}
	got, err = ClassifyIdentity(auth, soon, now)
	if err != nil || got.State != IdentityExpiring {
		t.Errorf("control: expiring fixture = %s, %v; want expiring", got.State.testString(), err)
	}
}

// TestClassifyIdentityWithinMovesOnlyTheBoundary: the configured window moves
// the expiring/ok line and nothing else. A login one day out is expiring
// under the default window (the control) and ok under a twelve-hour one; the
// expired boundary does not move; and a window that is not positive is
// refused.
func TestClassifyIdentityWithinMovesOnlyTheBoundary(t *testing.T) {
	auth, _ := loadIdentityFixture(t, "authstatus", "valid")
	_, now := loadIdentityFixture(t, "credentials", "ok")
	day := synthLogin(fakeAccess, fakeRefresh, now.Add(8*time.Hour), now.Add(24*time.Hour))
	if got, err := ClassifyIdentity(auth, day, now); err != nil || got.State != IdentityExpiring {
		t.Fatalf("control: one day out under the default window = %v, %v; want expiring", got.State.testString(), err)
	}
	if got, err := ClassifyIdentityWithin(auth, day, now, 12*time.Hour); err != nil || got.State != IdentityOK {
		t.Errorf("one day out under a 12h window = %v, %v; want ok", got.State.testString(), err)
	}
	past := synthCreds(fakeAccess, fakeRefresh, now.Add(-time.Millisecond))
	if got, err := ClassifyIdentityWithin(auth, past, now, time.Hour); err != nil || got.State != IdentityExpired {
		t.Errorf("past expiry under a 1h window = %v, %v; want expired", got.State.testString(), err)
	}
	for _, w := range []time.Duration{0, -time.Hour} {
		if _, err := ClassifyIdentityWithin(auth, day, now, w); err == nil {
			t.Errorf("window %s was accepted", w)
		}
	}
}

// TestClassifyIdentityRefusesWhatItCannotRead: every input that cannot be
// classified honestly is an error, never a plausible Absent or OK.
func TestClassifyIdentityRefusesWhatItCannotRead(t *testing.T) {
	validAuth, now := loadIdentityFixture(t, "authstatus", "valid")
	falseAuth, _ := loadIdentityFixture(t, "authstatus", "absent")
	okCreds, _ := loadIdentityFixture(t, "credentials", "ok")
	corrupt, _ := loadIdentityFixture(t, "credentials", "corrupt")
	live := synthCreds(fakeAccess, fakeRefresh, now.Add(30*24*time.Hour))

	// Positive control: the same machinery classifies a good pair.
	if got, err := ClassifyIdentity(validAuth, live, now); err != nil || got.State != IdentityOK {
		t.Fatalf("control: valid auth + live file = %s, %v; want ok", got.State.testString(), err)
	}

	cases := []struct {
		name        string
		auth, creds []byte
	}{
		{"corrupt credential fixture", validAuth, corrupt},
		{"empty credential file (exists, zero bytes)", validAuth, []byte{}},
		{"credential file is JSON null", validAuth, []byte("null")},
		{"credential file is an empty object", validAuth, []byte("{}")},
		{"claudeAiOauth is null", validAuth, []byte(`{"claudeAiOauth":null}`)},
		{"accessToken missing", validAuth, []byte(`{"claudeAiOauth":{"refreshToken":"x","expiresAt":1}}`)},
		{"refreshToken missing", validAuth, []byte(`{"claudeAiOauth":{"accessToken":"x","expiresAt":1}}`)},
		{"accessToken not a string", validAuth, []byte(`{"claudeAiOauth":{"accessToken":1,"refreshToken":"x","expiresAt":1}}`)},
		{"partial blank: access empty", validAuth, synthCreds("", fakeRefresh, now.Add(time.Hour))},
		{"partial blank: refresh empty", validAuth, synthCreds(fakeAccess, "", now.Add(time.Hour))},
		{"live tokens, expiresAt missing", validAuth, []byte(`{"claudeAiOauth":{"accessToken":"x","refreshToken":"y"}}`)},
		{"live tokens, expiresAt null", validAuth, []byte(`{"claudeAiOauth":{"accessToken":"x","refreshToken":"y","expiresAt":null}}`)},
		{"live tokens, expiresAt zero", validAuth, []byte(`{"claudeAiOauth":{"accessToken":"x","refreshToken":"y","expiresAt":0}}`)},
		{"live tokens, expiresAt negative", validAuth, []byte(`{"claudeAiOauth":{"accessToken":"x","refreshToken":"y","expiresAt":-5}}`)},
		{"live tokens, expiresAt a string", validAuth, []byte(`{"claudeAiOauth":{"accessToken":"x","refreshToken":"y","expiresAt":"1791267277000"}}`)},
		{"live tokens, expiresAt fractional", validAuth, []byte(`{"claudeAiOauth":{"accessToken":"x","refreshToken":"y","expiresAt":1.5}}`)},
		{"live tokens, refreshTokenExpiresAt zero", validAuth, []byte(`{"claudeAiOauth":{"accessToken":"x","refreshToken":"y","expiresAt":1793686477000,"refreshTokenExpiresAt":0}}`)},
		{"live tokens, refreshTokenExpiresAt negative", validAuth, []byte(`{"claudeAiOauth":{"accessToken":"x","refreshToken":"y","expiresAt":1793686477000,"refreshTokenExpiresAt":-5}}`)},
		{"live tokens, refreshTokenExpiresAt a string", validAuth, []byte(`{"claudeAiOauth":{"accessToken":"x","refreshToken":"y","expiresAt":1793686477000,"refreshTokenExpiresAt":"1793686477000"}}`)},
		{"live tokens, refreshTokenExpiresAt fractional", validAuth, []byte(`{"claudeAiOauth":{"accessToken":"x","refreshToken":"y","expiresAt":1793686477000,"refreshTokenExpiresAt":1.5}}`)},
		{"contradiction: loggedIn:false beside live tokens", falseAuth, okCreds},
		{"auth status nil", nil, okCreds},
		{"auth status empty", []byte{}, okCreds},
		{"auth status truncated", []byte(`{"loggedIn":`), okCreds},
		{"auth status without loggedIn", []byte(`{"authMethod":"none"}`), okCreds},
		{"auth status loggedIn not a boolean", []byte(`{"loggedIn":"true"}`), okCreds},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := ClassifyIdentity(c.auth, c.creds, now)
			if err == nil {
				t.Errorf("got %s, want an error", got.State.testString())
			}
			if got != (Identity{}) {
				t.Errorf("an error came with a non-zero Identity %+v; a caller might use it", got)
			}
		})
	}
}

// TestClassifyIdentityFields: where AccountEmail and ExpiresAt come from, and
// that unknown keys (2.1.289's projectsDirectory, configDirectory) are ignored.
func TestClassifyIdentityFields(t *testing.T) {
	validAuth, _ := loadIdentityFixture(t, "authstatus", "valid")
	creds, now := loadIdentityFixture(t, "credentials", "expiring")
	if !bytes.Contains(validAuth, []byte("projectsDirectory")) || !bytes.Contains(validAuth, []byte("configDirectory")) {
		t.Fatal("precondition: the recorded fixture carries the 2.1.289 keys")
	}

	got, err := ClassifyIdentity(validAuth, creds, now)
	if err != nil {
		t.Fatal(err)
	}
	if got.AccountEmail != "fixture@example.invalid" {
		t.Errorf("AccountEmail = %q, want the auth status email", got.AccountEmail)
	}
	want, wantLogin := fixtureDates(t, creds)
	if !got.ExpiresAt.Equal(want) || got.ExpiresAt.Location() != time.UTC {
		t.Errorf("ExpiresAt = %v, want %v in UTC", got.ExpiresAt, want)
	}
	if !got.LoginExpiresAt.Equal(wantLogin) || got.LoginExpiresAt.Location() != time.UTC {
		t.Errorf("LoginExpiresAt = %v, want %v in UTC", got.LoginExpiresAt, wantLogin)
	}
	// The fixture's expiresAt is eight hours after recording and its
	// refreshTokenExpiresAt two days: milliseconds, not seconds, or these
	// would land in 1970 or the year 58,000.
	if d := got.ExpiresAt.Sub(now); d != 8*time.Hour {
		t.Errorf("expiresAt − recorded_at = %v; want 8h (is expiresAt being read as ms?)", d)
	}
	if d := got.LoginExpiresAt.Sub(now); d != 48*time.Hour {
		t.Errorf("refreshTokenExpiresAt − recorded_at = %v; want 48h (is it being read as ms?)", d)
	}

	// No email in auth status: still classified, just no email.
	noEmail := []byte(`{"loggedIn":true,"authMethod":"claude.ai","somethingNew":{"x":1}}`)
	got, err = ClassifyIdentity(noEmail, creds, now)
	if err != nil || got.State != IdentityExpiring || got.AccountEmail != "" {
		t.Errorf("no-email auth = %+v, %v; want expiring with empty email", got, err)
	}

	// Blanked and absent beside loggedIn:true: the file wins, and neither
	// verdict carries an email or a countdown from the other input.
	blanked, _ := loadIdentityFixture(t, "credentials", "blanked")
	for name, c := range map[string][]byte{"blanked": blanked, "absent": nil} {
		got, err := ClassifyIdentity(validAuth, c, now)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if got.State.testString() != name {
			t.Errorf("%s beside loggedIn:true = %s", name, got.State.testString())
		}
		if got.AccountEmail != "" || !got.ExpiresAt.IsZero() || !got.LoginExpiresAt.IsZero() {
			t.Errorf("%s carried email %q / ExpiresAt %v / LoginExpiresAt %v; want none", name, got.AccountEmail, got.ExpiresAt, got.LoginExpiresAt)
		}
	}

	// Blanked is decided by the tokens, not by expiresAt: a tombstone with a
	// non-zero expiresAt is still blanked.
	got, err = ClassifyIdentity(validAuth, synthCreds("", "", now.Add(time.Hour)), now)
	if err != nil || got.State != IdentityBlanked {
		t.Errorf("empty tokens with future expiresAt = %s, %v; want blanked", got.State.testString(), err)
	}
}

// TestClassifyIdentityFileVerdictsSurviveBrokenAuthStatus: Blanked and Absent
// are decided by the file alone, so an `auth status` that fails or changes
// shape — it is the less stable input — cannot hide the tombstone. The live
// verdicts genuinely need it, and still refuse without it.
func TestClassifyIdentityFileVerdictsSurviveBrokenAuthStatus(t *testing.T) {
	blanked, now := loadIdentityFixture(t, "credentials", "blanked")
	okCreds, _ := loadIdentityFixture(t, "credentials", "ok")
	validAuth, _ := loadIdentityFixture(t, "authstatus", "valid")
	broken := map[string][]byte{
		"nil":                 nil,
		"empty":               {},
		"garbage":             []byte("Not logged in"),
		"truncated":           []byte(`{"loggedIn":`),
		"no loggedIn":         []byte(`{"authMethod":"none"}`),
		"loggedIn not a bool": []byte(`{"loggedIn":"false"}`),
	}
	for name, auth := range broken {
		t.Run(name, func(t *testing.T) {
			got, err := ClassifyIdentity(auth, blanked, now)
			if err != nil || got.State != IdentityBlanked {
				t.Errorf("blanked file + %s auth status = %s, %v; want blanked — the tombstone must not be masked", name, got.State.testString(), err)
			}
			got, err = ClassifyIdentity(auth, nil, now)
			if err != nil || got.State != IdentityAbsent {
				t.Errorf("no file + %s auth status = %s, %v; want absent", name, got.State.testString(), err)
			}
			// Positive control: where auth status is genuinely needed, the
			// same broken input is still refused...
			got, err = ClassifyIdentity(auth, okCreds, now)
			if err == nil {
				t.Errorf("ok file + %s auth status = %s; want an error", name, got.State.testString())
			}
			// ...and the same file with a good auth status is classified.
			if got, err := ClassifyIdentity(validAuth, okCreds, now); err != nil || got.State != IdentityOK {
				t.Errorf("control: ok file + valid auth status = %s, %v; want ok", got.State.testString(), err)
			}
		})
	}
}
