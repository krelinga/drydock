package auth

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/argon2"

	"github.com/krelinga/drydock/internal/store"
	"github.com/krelinga/drydock/internal/sys"
)

var t0 = time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)

// seqRandom yields distinct, reproducible bytes so two sessions never collide
// and a failure reproduces exactly.
type seqRandom struct{ n byte }

func (r *seqRandom) Read(p []byte) (int, error) {
	for i := range p {
		r.n++
		p[i] = r.n
	}
	return len(p), nil
}

type rig struct {
	dir   string
	db    *store.DB
	clock *sys.FakeClock
	svc   *Service
}

func newRig(t *testing.T) *rig {
	t.Helper()
	dir := t.TempDir()
	db, err := store.Open(context.Background(), filepath.Join(dir, "drydock.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	clock := sys.NewFakeClock(t0)
	random := &seqRandom{}
	svc := &Service{
		DB: db.DB, Clock: clock, Random: random, Params: Floor,
		Sessions: &Sessions{DB: db.DB, Clock: clock, Random: random},
		Limiter:  &Limiter{DB: db.DB, Clock: clock, Policy: DefaultPolicy},
	}
	return &rig{dir: dir, db: db, clock: clock, svc: svc}
}

// ---- passwords ---------------------------------------------------------------

// TestHashMeetsFloorAndRoundTrips is testing §8.1's "argon2id cost is not
// silently lowered": a fresh hash's own encoded parameters meet the floor.
func TestHashMeetsFloorAndRoundTrips(t *testing.T) {
	h, err := Hash("correct horse battery staple", DefaultParams, &seqRandom{})
	if err != nil {
		t.Fatal(err)
	}
	p, err := ParamsOf(h)
	if err != nil {
		t.Fatal(err)
	}
	if !p.atLeast(Floor) {
		t.Errorf("fresh hash params %+v are below the floor %+v", p, Floor)
	}
	if ok, rehash, err := Verify(h, "correct horse battery staple"); err != nil || !ok || rehash {
		t.Errorf("right password: ok=%v rehash=%v err=%v; want true false nil", ok, rehash, err)
	}
	if ok, _, err := Verify(h, "correct horse battery stapl"); err != nil || ok {
		t.Errorf("wrong password: ok=%v err=%v; want false nil", ok, err)
	}
}

func TestHashRefusesBelowFloor(t *testing.T) {
	weak := Floor
	weak.Memory /= 2
	if _, err := Hash("x", weak, &seqRandom{}); !errors.Is(err, ErrWeakParams) {
		t.Errorf("Hash with %d KiB = %v; want ErrWeakParams", weak.Memory, err)
	}
	if _, err := Hash("x", Floor, &seqRandom{}); err != nil {
		t.Errorf("Hash at exactly the floor: %v", err) // control
	}
}

// TestVerifyKnownHash pins the encoding. A stored hash is long-lived; a change
// to how it is parsed must not silently sign the operator out forever.
func TestVerifyKnownHash(t *testing.T) {
	const known = "$argon2id$v=19$m=65536,t=3,p=1$BwcHBwcHBwcHBwcHBwcHBw$atEK+X8XRBGb1xNchRIdxYl5T5xdZGIAuK1Na+zxUIQ"
	if ok, _, err := Verify(known, "correct horse battery staple"); err != nil || !ok {
		t.Errorf("known hash did not verify: ok=%v err=%v", ok, err)
	}
	if ok, _, _ := Verify(known, "Correct horse battery staple"); ok {
		t.Error("known hash verified a different password")
	}
}

func TestVerifyFlagsWeakHashForRehash(t *testing.T) {
	salt := bytes.Repeat([]byte{1}, 16)
	key := argon2.IDKey([]byte("pw"), salt, 1, 8*1024, 1, 32)
	b := base64.RawStdEncoding
	weak := fmt.Sprintf("$argon2id$v=19$m=8192,t=1,p=1$%s$%s", b.EncodeToString(salt), b.EncodeToString(key))
	ok, rehash, err := Verify(weak, "pw")
	if err != nil || !ok {
		t.Fatalf("weak hash did not verify: ok=%v err=%v", ok, err)
	}
	if !rehash {
		t.Error("a hash below the floor was not flagged for rehash")
	}
}

func TestVerifyRejectsMalformed(t *testing.T) {
	good, _ := Hash("pw", Floor, &seqRandom{})
	if _, _, err := Verify(good, "pw"); err != nil {
		t.Fatalf("control: valid hash errored: %v", err)
	}
	for _, bad := range []string{
		"", "plaintext", "$argon2i$v=19$m=65536,t=3,p=1$AAAA$AAAA",
		"$argon2id$v=16$m=65536,t=3,p=1$AAAA$AAAA", "$argon2id$v=19$m=x,t=3,p=1$AAAA$AAAA",
		"$argon2id$v=19$m=65536,t=3,p=1$!!!!$AAAA", "$argon2id$v=19$m=65536,t=3,p=1$AAAA$",
	} {
		if ok, _, err := Verify(bad, "pw"); err == nil || ok {
			t.Errorf("Verify(%q) = ok:%v err:%v; want an error", bad, ok, err)
		}
	}
}

// ---- sessions ------------------------------------------------------------------

// TestSessionCookieNeverStored is the "stolen database file yields no usable
// cookie" property (§4), checked against the raw bytes of every file SQLite
// wrote — the main file, the WAL and the shared-memory index — the way the
// canary sweep does it (testing §4.2).
func TestSessionCookieNeverStored(t *testing.T) {
	r := newRig(t)
	cookie, sess, err := r.svc.Sessions.Create(context.Background(), "iPhone", "192.0.2.1")
	if err != nil {
		t.Fatal(err)
	}
	if len(cookie) < 40 {
		t.Fatalf("cookie %d chars; want 32 random bytes encoded", len(cookie))
	}
	sum := sha256.Sum256([]byte(cookie))
	wantID := hex.EncodeToString(sum[:])
	if sess.ID != wantID {
		t.Errorf("session id is not sha256(cookie)")
	}

	var raw []byte
	entries, _ := os.ReadDir(r.dir)
	for _, e := range entries {
		b, err := os.ReadFile(filepath.Join(r.dir, e.Name()))
		if err == nil {
			raw = append(raw, b...)
		}
	}
	if bytes.Contains(raw, []byte(cookie)) {
		t.Error("the cookie value appears in the database files")
	}
	// The expected hit, so the sweep above is known to be reading the data.
	if !bytes.Contains(raw, []byte(wantID)) {
		t.Fatal("the session's hash is not in the database files either: the sweep read nothing")
	}
}

// Both halves of the idle rule are tested well inside the 30-day absolute
// ceiling. An earlier version advanced past 30 days in total, so the absolute
// check expired the session and the idle check could be deleted without a
// single test failing — mutation testing caught it.
func TestSessionIdleExpiry(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	cookie, _, _ := r.svc.Sessions.Create(ctx, "", "")
	// 14 days unused, 16 days short of the absolute ceiling: only the idle
	// rule can end it.
	r.clock.Advance(IdleLifetime + time.Second)
	if _, err := r.svc.Sessions.Lookup(ctx, cookie); !errors.Is(err, ErrNoSession) {
		t.Errorf("lookup after %v idle = %v; want ErrNoSession", IdleLifetime, err)
	}
	// Control: a session used just inside the window survives.
	other, _, _ := r.svc.Sessions.Create(ctx, "", "")
	r.clock.Advance(IdleLifetime - time.Second)
	if _, err := r.svc.Sessions.Lookup(ctx, other); err != nil {
		t.Errorf("lookup just inside the idle window: %v", err)
	}
}

func TestSessionIdleWindowSlides(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	cookie, _, _ := r.svc.Sessions.Create(ctx, "", "")
	// 13 + 13 = 26 days: past one idle window measured from creation, under
	// the absolute ceiling. Only sliding last_seen on each use keeps it alive.
	for i := 0; i < 2; i++ {
		r.clock.Advance(13 * 24 * time.Hour)
		if _, err := r.svc.Sessions.Lookup(ctx, cookie); err != nil {
			t.Fatalf("lookup after 13 idle days (round %d): %v — the idle window is not sliding", i, err)
		}
	}
}

func TestSessionAbsoluteCeiling(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	cookie, _, _ := r.svc.Sessions.Create(ctx, "", "")
	for elapsed := time.Duration(0); elapsed < AbsoluteLifetime-time.Minute; elapsed += 10 * 24 * time.Hour {
		if _, err := r.svc.Sessions.Lookup(ctx, cookie); err != nil {
			t.Fatalf("active session refused at %v: %v", elapsed, err) // control
		}
		r.clock.Advance(10 * 24 * time.Hour)
	}
	// Now past 30 days, and active the whole time: still expired.
	if _, err := r.svc.Sessions.Lookup(ctx, cookie); !errors.Is(err, ErrNoSession) {
		t.Errorf("session past its 30-day ceiling = %v; want ErrNoSession", err)
	}
}

func TestSessionLookupRejectsUnknownAndEmpty(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	cookie, _, _ := r.svc.Sessions.Create(ctx, "", "")
	if _, err := r.svc.Sessions.Lookup(ctx, cookie); err != nil {
		t.Fatalf("control: real cookie refused: %v", err)
	}
	for _, c := range []string{"", "not-a-session", cookie + "x", strings.ToUpper(cookie)} {
		if _, err := r.svc.Sessions.Lookup(ctx, c); !errors.Is(err, ErrNoSession) {
			t.Errorf("Lookup(%q) = %v; want ErrNoSession", c, err)
		}
	}
}

func TestSessionRevokeAndList(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	a, sa, _ := r.svc.Sessions.Create(ctx, "phone", "")
	b, _, _ := r.svc.Sessions.Create(ctx, "laptop", "")
	if l, _ := r.svc.Sessions.List(ctx); len(l) != 2 {
		t.Fatalf("List = %d sessions; want 2", len(l))
	}
	if err := r.svc.Sessions.Revoke(ctx, sa.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := r.svc.Sessions.Lookup(ctx, a); !errors.Is(err, ErrNoSession) {
		t.Error("revoked session still resolves")
	}
	if _, err := r.svc.Sessions.Lookup(ctx, b); err != nil {
		t.Errorf("revoking one session ended another: %v", err) // control
	}
	if err := r.svc.Sessions.RevokeAll(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := r.svc.Sessions.Lookup(ctx, b); !errors.Is(err, ErrNoSession) {
		t.Error("RevokeAll left a session alive")
	}
}

// ---- lockout -------------------------------------------------------------------

func fail(t *testing.T, r *rig, ip string, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		if err := r.svc.Limiter.Record(context.Background(), ip, OutcomeBadPassword); err != nil {
			t.Fatal(err)
		}
	}
}

func check(t *testing.T, r *rig, ip string) Decision {
	t.Helper()
	d, err := r.svc.Limiter.Check(context.Background(), ip)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func TestLockoutFreeAttemptsThenDoublingBackoff(t *testing.T) {
	r := newRig(t)
	p := DefaultPolicy
	fail(t, r, "192.0.2.1", p.FreeAttempts-1)
	if d := check(t, r, "192.0.2.1"); !d.Allowed {
		t.Fatalf("refused after %d failures; the first %d are free", p.FreeAttempts-1, p.FreeAttempts)
	}
	want := p.BaseDelay
	for i := 0; i < 4; i++ {
		fail(t, r, "192.0.2.1", 1)
		d := check(t, r, "192.0.2.1")
		if d.Allowed || d.RetryAfter != want {
			t.Fatalf("after failure %d: %+v; want refused for %v", p.FreeAttempts+i, d, want)
		}
		r.clock.Advance(want)
		if d := check(t, r, "192.0.2.1"); !d.Allowed {
			t.Fatalf("still refused after waiting %v", want)
		}
		want *= 2
	}
}

func TestLockoutBackoffIsCapped(t *testing.T) {
	r := newRig(t)
	fail(t, r, "192.0.2.1", 40)
	if d := check(t, r, "192.0.2.1"); d.Allowed || d.RetryAfter != DefaultPolicy.MaxDelay {
		t.Errorf("after 40 failures: %+v; want refused for exactly %v", d, DefaultPolicy.MaxDelay)
	}
}

// TestLockoutRefusalsDoNotExtendIt: a device retrying while locked must not
// push its own unlock further away.
func TestLockoutRefusalsDoNotExtendIt(t *testing.T) {
	r := newRig(t)
	fail(t, r, "192.0.2.1", DefaultPolicy.FreeAttempts)
	before := check(t, r, "192.0.2.1")
	if before.Allowed {
		t.Fatal("control: not locked out")
	}
	for i := 0; i < 20; i++ {
		r.svc.Limiter.Record(context.Background(), "192.0.2.1", OutcomeLockedOut)
	}
	if after := check(t, r, "192.0.2.1"); after.RetryAfter != before.RetryAfter {
		t.Errorf("lockout grew from %v to %v after refused retries", before.RetryAfter, after.RetryAfter)
	}
}

func TestLockoutSuccessClearsTheIP(t *testing.T) {
	r := newRig(t)
	fail(t, r, "192.0.2.1", DefaultPolicy.FreeAttempts)
	if check(t, r, "192.0.2.1").Allowed {
		t.Fatal("control: not locked out")
	}
	r.clock.Advance(time.Hour) // let the lock lapse, then succeed
	r.svc.Limiter.Record(context.Background(), "192.0.2.1", OutcomeOK)
	r.clock.Advance(time.Second)
	fail(t, r, "192.0.2.1", DefaultPolicy.FreeAttempts-1)
	if !check(t, r, "192.0.2.1").Allowed {
		t.Error("failures before a success still counted against the IP")
	}
}

func TestLockoutIsPerIP(t *testing.T) {
	r := newRig(t)
	fail(t, r, "192.0.2.1", DefaultPolicy.FreeAttempts)
	if check(t, r, "192.0.2.1").Allowed {
		t.Fatal("the noisy IP is not locked out")
	}
	if !check(t, r, "192.0.2.2").Allowed {
		t.Error("one IP's lockout refused another IP: a stale phone could lock the operator out")
	}
}

// TestGlobalCapLocksEveryone covers the other half of §13.2: many sources at
// once, each under its own per-IP threshold.
func TestGlobalCapLocksEveryone(t *testing.T) {
	r := newRig(t)
	p := DefaultPolicy
	for i := 0; i < p.GlobalCap; i++ {
		fail(t, r, fmt.Sprintf("198.51.100.%d", i), 1)
	}
	d := check(t, r, "203.0.113.9") // never failed once
	if d.Allowed || !d.Global {
		t.Fatalf("fresh IP after %d failures from %d IPs: %+v; want a global refusal", p.GlobalCap, p.GlobalCap, d)
	}
	r.clock.Advance(p.GlobalWindow + time.Second)
	if !check(t, r, "203.0.113.9").Allowed {
		t.Error("global lock did not lift once the window passed")
	}
}

func TestLockoutForgetsOldFailures(t *testing.T) {
	r := newRig(t)
	fail(t, r, "192.0.2.1", DefaultPolicy.FreeAttempts)
	r.clock.Advance(DefaultPolicy.Window + time.Second)
	if !check(t, r, "192.0.2.1").Allowed {
		t.Error("failures older than the window still counted")
	}
}

// ---- the sign-in flow ---------------------------------------------------------

const pw = "a long enough password"

func TestSignInHappyPathAndFailedAttemptNotice(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	if err := r.svc.SetPassword(ctx, pw); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if _, err := r.svc.SignIn(ctx, "192.0.2.66", "wrong", ""); !errors.Is(err, ErrBadPassword) {
			t.Fatalf("wrong password: %v; want ErrBadPassword", err)
		}
	}
	res, err := r.svc.SignIn(ctx, "192.0.2.1", pw, "iPhone")
	if err != nil {
		t.Fatalf("right password: %v", err)
	}
	if res.Cookie == "" {
		t.Error("no cookie on success")
	}
	if res.FailedSinceLastSignIn != 2 || len(res.FailedSources) != 1 || res.FailedSources[0] != "192.0.2.66" {
		t.Errorf("notice = %d from %v; want 2 from [192.0.2.66]", res.FailedSinceLastSignIn, res.FailedSources)
	}
	// Shown once: the next success reports nothing new.
	r.clock.Advance(time.Second)
	res, _ = r.svc.SignIn(ctx, "192.0.2.1", pw, "iPhone")
	if res.FailedSinceLastSignIn != 0 {
		t.Errorf("second sign-in repeated the notice: %d", res.FailedSinceLastSignIn)
	}
}

// TestSignInLockoutPrecedesHashing: a locked-out guesser must not get to spend
// the server's argon2 CPU. The attempt is logged as locked_out, never as
// bad_password, which is the observable proof that Verify never ran.
func TestSignInLockoutPrecedesHashing(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	r.svc.SetPassword(ctx, pw)
	fail(t, r, "192.0.2.1", DefaultPolicy.FreeAttempts)
	_, err := r.svc.SignIn(ctx, "192.0.2.1", pw, "") // even the RIGHT password
	if _, ok := IsLockedOut(err); !ok {
		t.Fatalf("locked-out sign-in = %v; want ErrLockedOut", err)
	}
	var lockedOut, bad int
	r.db.QueryRow(`SELECT count(*) FROM auth_attempt WHERE outcome='locked_out'`).Scan(&lockedOut)
	r.db.QueryRow(`SELECT count(*) FROM auth_attempt WHERE outcome='bad_password'`).Scan(&bad)
	if lockedOut != 1 || bad != DefaultPolicy.FreeAttempts {
		t.Errorf("logged %d locked_out and %d bad_password; want 1 and %d", lockedOut, bad, DefaultPolicy.FreeAttempts)
	}
}

func TestSignInNotConfigured(t *testing.T) {
	r := newRig(t)
	if _, err := r.svc.SignIn(context.Background(), "192.0.2.1", pw, ""); !errors.Is(err, ErrNotConfigured) {
		t.Errorf("sign-in with no operator = %v; want ErrNotConfigured", err)
	}
}

func TestSetPasswordRevokesSessionsAndRefusesShort(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	if err := r.svc.SetPassword(ctx, "short"); err == nil {
		t.Error("a 5-character password was accepted")
	}
	r.svc.SetPassword(ctx, pw)
	res, err := r.svc.SignIn(ctx, "192.0.2.1", pw, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := r.svc.SetPassword(ctx, pw+" changed"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.svc.Sessions.Lookup(ctx, res.Cookie); !errors.Is(err, ErrNoSession) {
		t.Error("a password change left an existing session signed in")
	}
	if _, err := r.svc.SignIn(ctx, "192.0.2.1", pw+" changed", ""); err != nil {
		t.Errorf("control: the new password does not work: %v", err)
	}
}

func TestSignInUpgradesAWeakHash(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	salt := bytes.Repeat([]byte{2}, 16)
	b := base64.RawStdEncoding
	weak := fmt.Sprintf("$argon2id$v=19$m=8192,t=1,p=1$%s$%s",
		b.EncodeToString(salt), b.EncodeToString(argon2.IDKey([]byte(pw), salt, 1, 8*1024, 1, 32)))
	r.db.Exec(`INSERT INTO operator (id, password_hash) VALUES (1, ?)`, weak)
	if _, err := r.svc.SignIn(ctx, "192.0.2.1", pw, ""); err != nil {
		t.Fatalf("sign-in with a weak-but-valid hash: %v", err)
	}
	var stored string
	r.db.QueryRow(`SELECT password_hash FROM operator WHERE id = 1`).Scan(&stored)
	if p, _ := ParamsOf(stored); !p.atLeast(Floor) {
		t.Errorf("hash not upgraded: still %+v", p)
	}
}
