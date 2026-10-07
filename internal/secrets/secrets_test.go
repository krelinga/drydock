package secrets

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/krelinga/drydock/internal/events"
	"github.com/krelinga/drydock/internal/store"
	"github.com/krelinga/drydock/internal/sys"
)

var t0 = time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)

type env struct {
	s     *Store
	db    *store.DB
	clock *sys.FakeClock
	log   *events.Log
}

func testKey(t *testing.T) *Key {
	t.Helper()
	raw := make([]byte, KeySize)
	rand.Read(raw)
	k, err := NewKey(raw)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func newEnv(t *testing.T) *env {
	t.Helper()
	ctx := context.Background()
	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "drydock.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	for _, q := range []string{
		`INSERT INTO repository (id, installation_id, full_name, default_branch) VALUES (101, 77, 'krelinga/alpha', 'main')`,
		`INSERT INTO repository (id, installation_id, full_name, default_branch) VALUES (202, 77, 'krelinga/beta', 'main')`,
		`INSERT INTO workspace (id, repository_id, host_path, branch, state) VALUES ('01JAAAAAAAAAAAAAAAAAAAAAAA', 101, '/x', 'main', 'running')`,
		`INSERT INTO workspace (id, repository_id, host_path, branch, state) VALUES ('01JBBBBBBBBBBBBBBBBBBBBBBB', 202, '/y', 'main', 'running')`,
		`INSERT INTO workspace (id, repository_id, host_path, branch, state) VALUES ('01JCCCCCCCCCCCCCCCCCCCCCCC', 101, '/z', 'main', 'stopped')`,
	} {
		if _, err := db.ExecContext(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	clock := sys.NewFakeClock(t0)
	log := events.New(db.DB, clock)
	return &env{
		s:  &Store{DB: db.DB, Key: testKey(t), Events: log, Env: sys.Env{Clock: clock, Random: sys.CryptoRandom{}}},
		db: db, clock: clock, log: log,
	}
}

func (e *env) put(t *testing.T, name, value string) PutResult {
	t.Helper()
	r, err := e.s.Put(context.Background(), name, value, "reaches a scratch database", "from the test")
	if err != nil {
		t.Fatalf("Put %s: %v", name, err)
	}
	return r
}

func names(es []Entry) string {
	var out []string
	for _, e := range es {
		out = append(out, e.Name+"="+e.Value)
	}
	return strings.Join(out, ",")
}

func code(err error) string {
	var inv *Invalid
	if errors.As(err, &inv) {
		return inv.Code
	}
	return ""
}

// ---- the key ---------------------------------------------------------------

func TestLoadKey(t *testing.T) {
	dir := t.TempDir()
	raw := make([]byte, KeySize)
	rand.Read(raw)
	good := filepath.Join(dir, "good")
	os.WriteFile(good, raw, 0o400)
	k, err := LoadKey(good)
	if err != nil {
		t.Fatalf("control: a 0400 key of 32 bytes was refused: %v", err)
	}
	want, _ := NewKey(raw)
	if !k.Equal(want) {
		t.Error("the loaded key is not the file's bytes")
	}
	for name, c := range map[string]struct {
		mode os.FileMode
		size int
		want string
	}{
		"group-readable": {0o440, KeySize, "0400"},
		"world-readable": {0o404, KeySize, "0400"},
		"short":          {0o400, KeySize - 1, "exactly 32"},
		"long":           {0o400, KeySize + 1, "exactly 32"},
		"empty":          {0o400, 0, "exactly 32"},
	} {
		p := filepath.Join(dir, name)
		b := make([]byte, c.size)
		rand.Read(b)
		os.WriteFile(p, b, c.mode)
		os.Chmod(p, c.mode)
		if _, err := LoadKey(p); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v; want an error naming %q", name, err, c.want)
		}
	}
	if _, err := LoadKey(filepath.Join(dir, "missing")); err == nil {
		t.Error("a missing key file was accepted")
	}
}

// A key never prints, and never marshals.
func TestKeyIsRedacted(t *testing.T) {
	raw := []byte(strings.Repeat("K", KeySize))
	k, _ := NewKey(raw)
	for _, s := range []string{fmt.Sprint(k), fmt.Sprintf("%v %+v %#v %s", k, k, k, k), fmt.Sprint(*k)} {
		if strings.Contains(s, "KKKK") {
			t.Errorf("a key formatted as %q", s)
		}
	}
	if b, err := json.Marshal(struct{ K *Key }{k}); err == nil {
		t.Errorf("a key marshalled: %s", b)
	}
	// Control: the bytes are in there, and Equal sees them.
	k2, _ := NewKey(raw)
	if !k.Equal(k2) {
		t.Error("control: equal keys compare unequal")
	}
}

// ---- validation (§10.1) -------------------------------------------------------

// Every reserved name is refused, by its exact spelling and by every prefix;
// and the control: ordinary names that merely resemble them are accepted.
func TestReservedNames(t *testing.T) {
	refused := []string{
		// §10.1, verbatim.
		"ANTHROPIC_BASE_URL", "DISABLE_TELEMETRY", "DO_NOT_TRACK", "CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC",
		"DISABLE_GROWTHBOOK", "GH_TOKEN", "GITHUB_TOKEN", "CLAUDE_CONFIG_DIR", "PATH", "HOME", "SHELL",
		"ANTHROPIC_API_KEY", "CLAUDE_CODE_OAUTH_TOKEN",
		// As built: the prefixes and the rest of the list.
		"CLAUDE_ENV_FILE", "DISABLE_AUTOUPDATER", "GH_HOST", "GH_ENTERPRISE_TOKEN", "GITHUB_ENTERPRISE_TOKEN",
		"GIT_CONFIG_COUNT", "GIT_CONFIG_KEY_0", "GIT_ASKPASS", "GIT_SSH_COMMAND",
		"DRYDOCK_BROKER_SOCK", "DRYDOCK_GITHUB_HOST", "LD_PRELOAD", "LD_LIBRARY_PATH", "LD_AUDIT",
		"BASH_ENV", "BASH_FUNC_X", "ENV", "IFS", "CDPATH", "PS4", "PROMPT_COMMAND", "SHELLOPTS", "BASHOPTS",
		"GLOBIGNORE", "UID", "EUID", "PPID", "_", "USER", "LOGNAME", "PWD",
		"NODE_OPTIONS", "NODE_EXTRA_CA_CERTS", "NODE_TLS_REJECT_UNAUTHORIZED",
		"HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "NO_PROXY", "SSL_CERT_FILE", "SSL_CERT_DIR", "CURL_CA_BUNDLE",
	}
	for _, n := range refused {
		if err := ValidateName(n); code(err) != CodeNameReserved {
			t.Errorf("%s: %v; want %s", n, err, CodeNameReserved)
		} else if !strings.Contains(err.(*Invalid).Message, n) {
			t.Errorf("%s: the refusal does not name it: %v", n, err)
		}
	}
	for _, n := range []string{"TEST_DATABASE_URL", "STRIPE_TEST_KEY", "MY_GITHUB_THING", "PATHS", "XHOME", "GITHUB_APP_ID",
		"ANTHROPIC", "CLAUDE", "NPM_TOKEN", "A", "_X", "UIDS", "DATABASE_URL"} {
		if err := ValidateName(n); err != nil {
			t.Errorf("control: %s refused: %v", n, err)
		}
	}
	for _, n := range []string{"", "lower", "1ABC", "A-B", "A B", "A.B", "Ä", "A\n", strings.Repeat("A", MaxNameLen+1)} {
		if err := ValidateName(n); code(err) != CodeNameInvalid {
			t.Errorf("%q: %v; want %s", n, err, CodeNameInvalid)
		}
	}
}

// Any control character is refused, and the refusal names the character but
// never quotes the value. Control: spaces, quotes, shell metacharacters and
// non-ASCII are all fine.
func TestValueValidation(t *testing.T) {
	const tail = "zq7Vx9-tail"
	for _, c := range []struct {
		v, names string
	}{
		{"hunter2\nGH_TOKEN ghp_" + tail, "newline"},
		{"a\r" + tail, "carriage return"},
		{"a\x00" + tail, "NUL"},
		{"a\t" + tail, "tab"},
		{"a\x1b[31m" + tail, "U+001B"},
		{"a\x7f" + tail, "U+007F"},
		{"a\u0085" + tail, "U+0085"},
	} {
		err := ValidateValue(c.v)
		if code(err) != CodeValueControl {
			t.Errorf("%q: %v; want %s", c.v, err, CodeValueControl)
			continue
		}
		if !strings.Contains(err.Error(), c.names) {
			t.Errorf("%q: the refusal does not name %s: %v", c.v, c.names, err)
		}
		if strings.Contains(err.Error(), tail) {
			t.Errorf("the refusal quotes the value: %v", err)
		}
	}
	if code(ValidateValue("")) != CodeValueEmpty {
		t.Error("an empty value was accepted")
	}
	if code(ValidateValue(strings.Repeat("x", MaxValueLen+1))) != CodeValueTooLong {
		t.Error("an over-long value was accepted")
	}
	if code(ValidateValue("a\xffb")) != CodeValueControl {
		t.Error("invalid UTF-8 was accepted")
	}
	for _, ok := range []string{"postgres://u:p@db:5432/x?sslmode=disable", `it's "quoted" \ $(id) ` + "`id`" + ` ; | & * ? [ ] ~ # !`,
		" leading and trailing spaces ", "ünïcödé 漢字 🙂 \u2028 \ufeff", strings.Repeat("x", MaxValueLen), "'", "\\'"} {
		if err := ValidateValue(ok); err != nil {
			t.Errorf("control: %q refused: %v", ok, err)
		}
	}
}

func TestReachIsRequired(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	for _, reach := range []string{"", "   ", "\t\n"} {
		if _, err := e.s.Put(ctx, "TEST_KEY", "v", reach, ""); code(err) != CodeReachRequired {
			t.Errorf("reach %q: %v", reach, err)
		}
	}
	if ms, _ := e.s.List(ctx); len(ms) != 0 {
		t.Error("a refused write stored something")
	}
	// The schema refuses it too, for a write that goes around Put.
	if _, err := e.db.ExecContext(ctx, `INSERT INTO secret (id, name, ciphertext, nonce, reach) VALUES ('x', 'X', x'00', x'00', '  ')`); err == nil {
		t.Error("the schema accepted a blank reach")
	}
	if _, err := e.s.Put(ctx, "TEST_KEY", "v", "reads a scratch bucket", ""); err != nil {
		t.Errorf("control: with a reach: %v", err)
	}
}

// A blank reach and a long one are two faults with two codes (frontend §4.5
// #14), and the description gets the same split: too long is not "not text".
// Controls: exactly at each limit is accepted, and a blank description is.
func TestReachAndDescriptionCodes(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	for _, c := range []struct {
		reach, description, want string
	}{
		{"", "", CodeReachRequired},
		{" \t", "", CodeReachRequired},
		{strings.Repeat("r", MaxReachLen+1), "", CodeReachTooLong},
		{"r", strings.Repeat("d", MaxDescriptionLen+1), CodeDescriptionTooLong},
		{"r", "not \xff text", CodeDescriptionBad},
	} {
		if _, err := e.s.Put(ctx, "TEST_KEY", "v", c.reach, c.description); code(err) != c.want {
			t.Errorf("reach %d bytes, description %d bytes: %v; want %s", len(c.reach), len(c.description), err, c.want)
		}
	}
	if ms, _ := e.s.List(ctx); len(ms) != 0 {
		t.Error("a refused write stored something")
	}
	if _, err := e.s.Put(ctx, "TEST_KEY", "v", strings.Repeat("r", MaxReachLen), strings.Repeat("d", MaxDescriptionLen)); err != nil {
		t.Errorf("control: at both limits: %v", err)
	}
	if _, err := e.s.Put(ctx, "TEST_KEY", "v", "r", ""); err != nil {
		t.Errorf("control: a blank description: %v", err)
	}
	if err := ValidateDescription("line one\nline two"); err != nil {
		t.Errorf("control: a multi-line description: %v", err)
	}
}

// TestCreateNeverReplaces is #29's review: New secret with a name already
// stored silently replaced its value, reach and description. Create refuses
// it with ErrExists and changes nothing — the delivered value, the reach and
// the event log all stay as they were. Controls in the same function: Create
// of a new name stores it, and Put of the same existing name still replaces
// it. Then two creates of one name at once: exactly one lands.
func TestCreateNeverReplaces(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	r, err := e.s.Create(ctx, "NPM_TOKEN", "original", "publishes the old package", "")
	if err != nil || !r.Created {
		t.Fatalf("control: Create of a new name = %+v, %v", r, err)
	}
	e.s.SetGrants(ctx, "NPM_TOKEN", []int64{101}, false)
	var before int
	e.db.QueryRow(`SELECT count(*) FROM event`).Scan(&before)

	if _, err := e.s.Create(ctx, "NPM_TOKEN", "another", "publishes the new package", "x"); !errors.Is(err, ErrExists) {
		t.Fatalf("Create of a stored name = %v; want ErrExists", err)
	}
	if got, _ := e.s.Resolve(ctx, 101); names(got) != "NPM_TOKEN=original" {
		t.Errorf("a refused create changed the delivered value: %q", names(got))
	}
	if m, _ := e.s.Get(ctx, "NPM_TOKEN"); m.Reach != "publishes the old package" || m.Description != "" {
		t.Errorf("a refused create changed the prose: %+v", m)
	}
	var after int
	e.db.QueryRow(`SELECT count(*) FROM event`).Scan(&after)
	if after != before {
		t.Errorf("a refused create wrote %d events", after-before)
	}

	// Control: the replacing write is still Put, and it still replaces.
	if r := e.put(t, "NPM_TOKEN", "another"); !r.Rotated {
		t.Errorf("control: Put of the stored name did not replace it: %+v", r)
	}
	if got, _ := e.s.Resolve(ctx, 101); names(got) != "NPM_TOKEN=another" {
		t.Errorf("control: after Put, delivered %q", names(got))
	}

	// Two devices creating one name at once: one wins, the other is told.
	for i := 0; i < 20; i++ {
		name := fmt.Sprintf("RACE_%d", i)
		errs := make(chan error, 2)
		for _, v := range []string{"a", "b"} {
			go func() { _, err := e.s.Create(ctx, name, v, "r", ""); errs <- err }()
		}
		won, lost := 0, 0
		for range 2 {
			switch err := <-errs; {
			case err == nil:
				won++
			case errors.Is(err, ErrExists):
				lost++
			default:
				t.Fatalf("a racing create: %v", err)
			}
		}
		if won != 1 || lost != 1 {
			t.Fatalf("%s: %d creates landed, %d refused; want exactly one of each", name, won, lost)
		}
	}
}

// PutProse keeps the stored value: the reach and description move, nothing
// is rotated, nothing is stale, no secret.rotated, and the next delivery is
// the old value byte for byte. Control in the same function: a Put with a
// different value is a rotation by every one of those measures. And a
// PutProse for a name with no secret is refused, storing nothing.
func TestPutProseKeepsTheValue(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.put(t, "TEST_KEY", "k1")
	e.s.SetGrants(ctx, "TEST_KEY", []int64{101}, false)
	countKind := func(kind string) int {
		var n int
		e.db.QueryRow(`SELECT count(*) FROM event WHERE kind = ?`, kind).Scan(&n)
		return n
	}

	e.clock.Advance(time.Hour)
	r, err := e.s.PutProse(ctx, "TEST_KEY", "a narrower reach", "rotated in the scratch console")
	if err != nil {
		t.Fatal(err)
	}
	if r.Created || r.Rotated || len(r.Stale.NewCommands)+len(r.Stale.NeedsSupervisorRestart) != 0 || r.Secret.RotatedAt != nil {
		t.Errorf("prose only: %+v", r)
	}
	if r.Secret.Reach != "a narrower reach" || r.Secret.Description != "rotated in the scratch console" {
		t.Errorf("the prose did not move: %+v", r.Secret)
	}
	if got, err := e.s.Resolve(ctx, 101); err != nil || names(got) != "TEST_KEY=k1" {
		t.Errorf("after a prose-only PUT, delivered %q, %v", names(got), err)
	}
	if countKind("secret.rotated") != 0 || countKind("secret.updated") != 1 {
		t.Errorf("events: %d rotated (want 0), %d updated (want 1)", countKind("secret.rotated"), countKind("secret.updated"))
	}

	// Control: a value change is a rotation, and the running workspace is stale.
	r2 := e.put(t, "TEST_KEY", "k2")
	if !r2.Rotated || len(r2.Stale.NewCommands) != 1 || r2.Secret.RotatedAt == nil {
		t.Errorf("control: a value change: %+v", r2)
	}
	if countKind("secret.rotated") != 1 {
		t.Error("control: a value change emitted no secret.rotated")
	}

	// No secret, no value to keep.
	if _, err := e.s.PutProse(ctx, "NEW_KEY", "r", ""); code(err) != CodeValueRequired {
		t.Errorf("PutProse of a new name: %v; want %s", err, CodeValueRequired)
	}
	if _, err := e.s.Get(ctx, "NEW_KEY"); !errors.Is(err, ErrNotFound) {
		t.Errorf("a refused PutProse stored something: %v", err)
	}
	// And an empty value is not "no value": it is refused, and keeps nothing.
	if _, err := e.s.Put(ctx, "TEST_KEY", "", "r", ""); code(err) != CodeValueEmpty {
		t.Errorf("an empty value: %v; want %s", err, CodeValueEmpty)
	}
	if got, _ := e.s.Resolve(ctx, 101); names(got) != "TEST_KEY=k2" {
		t.Errorf("after a refused empty value, delivered %q", names(got))
	}
}

// insertRaw writes a secret row around Put, as only a database edit can:
// value is sealed under the store's key, or the row gets ciphertext that
// opens under no key when value is "".
func (e *env) insertRaw(t *testing.T, id, name, value string, repo int64) {
	t.Helper()
	ctx := context.Background()
	ct, nonce := []byte("not ciphertext under any key"), make([]byte, 24)
	if value != "" {
		var err error
		if ct, nonce, err = e.s.seal(id, value); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := e.db.ExecContext(ctx, `INSERT INTO secret (id, name, ciphertext, nonce, reach) VALUES (?, ?, ?, ?, 'x')`, id, name, ct, nonce); err != nil {
		t.Fatal(err)
	}
	if _, err := e.db.ExecContext(ctx, `INSERT INTO secret_grant (secret_id, repository_id) VALUES (?, ?)`, id, repo); err != nil {
		t.Fatal(err)
	}
	e.s.Invalidate()
}

func (e *env) deliveryEvents(t *testing.T) []string {
	t.Helper()
	rows, err := e.db.Query(`SELECT kind, coalesce(data, '') FROM event WHERE kind IN ('secret.undeliverable', 'secret.deliverable') ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var kind, data string
		rows.Scan(&kind, &data)
		out = append(out, kind+" "+data)
	}
	return out
}

// The undeliverable condition is readable, names every broken row and why,
// and its end is announced (frontend §4.5 #12) — while delivery refuses
// throughout. Control first: a healthy store reports nothing and emits
// nothing. Then two broken rows, repaired one at a time: the set shrinking
// is a new report, and the last repair is secret.deliverable, once.
func TestUndeliverableIsReadableAndItsEndAnnounced(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.put(t, "GOOD", "fine")
	e.s.SetGrants(ctx, "GOOD", []int64{101}, false)
	if u, err := e.s.Undeliverable(ctx); err != nil || u != nil {
		t.Fatalf("control: a healthy store reports %+v, %v", u, err)
	}
	if got := e.deliveryEvents(t); len(got) != 0 {
		t.Fatalf("control: a healthy store emitted %v", got)
	}

	const forged = "hunter2\nGH_TOKEN ghp_forgedZq7"
	e.insertRaw(t, "01JYYYYYYYYYYYYYYYYYYYYYYY", "LOST_KEY", "", 101)
	e.insertRaw(t, "01JZZZZZZZZZZZZZZZZZZZZZZZ", "FORGED", forged, 202)
	u, err := e.s.Undeliverable(ctx)
	if err != nil || u == nil {
		t.Fatalf("two broken rows report %+v, %v", u, err)
	}
	want := []UndeliverableSecret{{"FORGED", ReasonBreaksRules}, {"LOST_KEY", ReasonDoesNotOpen}}
	if !slices.Equal(u.Secrets, want) || !u.Since.Equal(t0) {
		t.Errorf("undeliverable = %+v; want %v since %v", u, want, t0)
	}
	// Fail closed: every repository is refused, the healthy secret's too.
	for _, repo := range []int64{101, 202} {
		if got, err := e.s.Resolve(ctx, repo); err == nil || len(got) != 0 {
			t.Errorf("repo %d: delivered %q while undeliverable", repo, names(got))
		}
	}
	// Asking again, and an unrelated write, are not new reports.
	e.s.Undeliverable(ctx)
	e.s.SetGrants(ctx, "GOOD", []int64{101, 202}, false)
	if got := e.deliveryEvents(t); len(got) != 1 || !strings.HasPrefix(got[0], "secret.undeliverable ") ||
		!strings.Contains(got[0], `"name":"LOST_KEY","reason":"does_not_open"`) {
		t.Fatalf("events = %v; want one undeliverable naming both", got)
	}

	// Repair one: storing the value again. Still broken, by one row.
	e.clock.Advance(time.Minute)
	e.put(t, "LOST_KEY", "restored")
	u, _ = e.s.Undeliverable(ctx)
	if u == nil || !slices.Equal(u.Secrets, want[:1]) || !u.Since.Equal(t0) {
		t.Errorf("after one repair: %+v", u)
	}
	if got := e.deliveryEvents(t); len(got) != 2 || !strings.HasPrefix(got[1], "secret.undeliverable ") || strings.Contains(got[1], "LOST_KEY") {
		t.Errorf("events = %v; want a second report naming only FORGED", got)
	}
	if _, err := e.s.Resolve(ctx, 101); err == nil {
		t.Error("delivered while one row is still broken")
	}

	// Repair the other, by deleting it: delivery works, the GET says so, and
	// the stream says so — at the write, not at the next fetch.
	if err := e.s.Delete(ctx, "FORGED"); err != nil {
		t.Fatal(err)
	}
	got := e.deliveryEvents(t)
	if len(got) != 3 || got[2] != "secret.deliverable {}" {
		t.Errorf("events = %v; want secret.deliverable last", got)
	}
	if u, err := e.s.Undeliverable(ctx); err != nil || u != nil {
		t.Errorf("after the repair: %+v, %v", u, err)
	}
	if r, err := e.s.Resolve(ctx, 101); err != nil || names(r) != "GOOD=fine,LOST_KEY=restored" {
		t.Errorf("after the repair, delivered %q, %v", names(r), err)
	}
	if len(e.deliveryEvents(t)) != 3 {
		t.Error("a fetch after the repair announced it again")
	}
	// No value reached the log by any of this.
	var all string
	e.db.QueryRow(`SELECT group_concat(message || coalesce(data, ''), '|') FROM event`).Scan(&all)
	for _, v := range []string{"hunter2", "forgedZq7", "restored", "fine"} {
		if strings.Contains(all, v) {
			t.Errorf("the event log holds %q", v)
		}
	}
	if !strings.Contains(all, "FORGED") {
		t.Error("control: the log does not name the broken secret; the search read nothing")
	}
}

// A restart starts from the log: a new process that finds the rows repaired
// announces it, because the clients that saw the last process's report are
// waiting for it. Control: one whose log already ends in deliverable — or
// holds no report at all — says nothing.
func TestDeliverableAcrossARestart(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.insertRaw(t, "01JYYYYYYYYYYYYYYYYYYYYYYY", "LOST_KEY", "", 101)
	if u, _ := e.s.Undeliverable(ctx); u == nil {
		t.Fatal("control: the broken row was not reported")
	}
	// Repaired behind the process's back, as restoring the old key is.
	e.db.ExecContext(ctx, `DELETE FROM secret`)
	restarted := func() *Store {
		return &Store{DB: e.db.DB, Key: e.s.Key, Events: e.log, Env: e.s.Env}
	}
	if u, err := restarted().Undeliverable(ctx); err != nil || u != nil {
		t.Fatalf("after the restart: %+v, %v", u, err)
	}
	got := e.deliveryEvents(t)
	if len(got) != 2 || got[1] != "secret.deliverable {}" {
		t.Errorf("events = %v; want the restart to announce the repair", got)
	}
	restarted().Undeliverable(ctx)
	if len(e.deliveryEvents(t)) != 2 {
		t.Error("a second restart announced it again")
	}
	fresh := newEnv(t)
	fresh.s.Undeliverable(ctx)
	if got := fresh.deliveryEvents(t); len(got) != 0 {
		t.Errorf("control: a store that never reported anything emitted %v", got)
	}
}

// ---- storage (§10.2) ----------------------------------------------------------

// The id is the associated data: a row's ciphertext and nonce moved onto
// another row fails to open, both ways. Control: unswapped, both open.
func TestAADIsTheSecretID(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.put(t, "ONE", "first value")
	e.put(t, "TWO", "second value")
	e.s.SetGrants(ctx, "ONE", []int64{101}, false)
	e.s.SetGrants(ctx, "TWO", []int64{101}, false)
	if got, err := e.s.Resolve(ctx, 101); err != nil || names(got) != "ONE=first value,TWO=second value" {
		t.Fatalf("control: unswapped rows: %q, %v", names(got), err)
	}
	var ct1, n1, ct2, n2 []byte
	e.db.QueryRow(`SELECT ciphertext, nonce FROM secret WHERE name = 'ONE'`).Scan(&ct1, &n1)
	e.db.QueryRow(`SELECT ciphertext, nonce FROM secret WHERE name = 'TWO'`).Scan(&ct2, &n2)
	e.db.ExecContext(ctx, `UPDATE secret SET ciphertext = ?, nonce = ? WHERE name = 'ONE'`, ct2, n2)
	e.db.ExecContext(ctx, `UPDATE secret SET ciphertext = ?, nonce = ? WHERE name = 'TWO'`, ct1, n1)
	e.s.Invalidate()
	got, err := e.s.Resolve(ctx, 101)
	if err == nil || len(got) != 0 {
		t.Fatalf("swapped rows delivered %q", names(got))
	}
	if strings.Contains(err.Error(), "value") && strings.Contains(err.Error(), "first") {
		t.Errorf("the error quotes a value: %v", err)
	}
	// Each one, alone, fails too.
	rows, _ := e.db.QueryContext(ctx, `SELECT id, ciphertext, nonce FROM secret`)
	n := 0
	for rows.Next() {
		var id string
		var ct, nonce []byte
		rows.Scan(&id, &ct, &nonce)
		if _, err := e.s.open(id, ct, nonce); err == nil {
			t.Errorf("secret %s opened with another row's ciphertext", id)
		}
		n++
	}
	rows.Close()
	if n != 2 {
		t.Fatalf("%d rows", n)
	}
}

// The database holds ciphertext: the value's bytes are nowhere in the file.
// Control: the reach, which is not secret, is.
func TestDatabaseHoldsCiphertext(t *testing.T) {
	e := newEnv(t)
	value := "postgres://drydock:Zr7qWm4LxP2vKc9N@db.internal:5432/test"
	e.put(t, "TEST_DATABASE_URL", value)
	e.s.SetGrants(context.Background(), "TEST_DATABASE_URL", []int64{101}, false)
	if _, err := e.db.ExecContext(context.Background(), `PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
		t.Fatal(err)
	}
	var raw []byte
	for _, suffix := range []string{"", "-wal", "-shm"} {
		b, _ := os.ReadFile(e.dbPath() + suffix)
		raw = append(raw, b...)
	}
	if strings.Contains(string(raw), "Zr7qWm4LxP2vKc9N") {
		t.Error("the value is in the database file")
	}
	if !strings.Contains(string(raw), "reaches a scratch database") {
		t.Error("control: the reach is not in the database file; the sweep read nothing")
	}
}

func (e *env) dbPath() string {
	var seq int
	var name, file string
	e.db.QueryRow(`PRAGMA database_list`).Scan(&seq, &name, &file)
	return file
}

// ---- grants and delivery ----------------------------------------------------------

// Default deny: a stored secret reaches nothing; a grant reaches one
// repository; all_repos reaches every one; revoking takes it away.
func TestDefaultDenyAndGrants(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.put(t, "TEST_KEY", "k1")
	for _, repo := range []int64{101, 202} {
		if got, err := e.s.Resolve(ctx, repo); err != nil || len(got) != 0 {
			t.Errorf("ungranted, repo %d got %q, %v", repo, names(got), err)
		}
	}
	m, err := e.s.SetGrants(ctx, "TEST_KEY", []int64{101, 101}, false)
	if err != nil || len(m.Grants) != 1 || m.Grants[0].FullName != "krelinga/alpha" {
		t.Fatalf("SetGrants: %+v, %v", m, err)
	}
	if got, _ := e.s.Resolve(ctx, 101); names(got) != "TEST_KEY=k1" {
		t.Errorf("control: the granted repository got %q", names(got))
	}
	if got, _ := e.s.Resolve(ctx, 202); len(got) != 0 {
		t.Errorf("the ungranted repository got %q", names(got))
	}
	if _, err := e.s.SetGrants(ctx, "TEST_KEY", nil, true); err != nil {
		t.Fatal(err)
	}
	for _, repo := range []int64{101, 202} {
		if got, _ := e.s.Resolve(ctx, repo); names(got) != "TEST_KEY=k1" {
			t.Errorf("all_repos, repo %d got %q", repo, names(got))
		}
	}
	if _, err := e.s.SetGrants(ctx, "TEST_KEY", nil, false); err != nil {
		t.Fatal(err)
	}
	if got, _ := e.s.Resolve(ctx, 101); len(got) != 0 {
		t.Errorf("revoked, still delivered %q", names(got))
	}
	if _, err := e.s.SetGrants(ctx, "TEST_KEY", []int64{999}, false); code(err) != CodeUnknownRepo {
		t.Errorf("an unknown repository: %v", err)
	}
	if _, err := e.s.SetGrants(ctx, "NOPE", []int64{101}, false); !errors.Is(err, ErrNotFound) {
		t.Errorf("an unknown secret: %v", err)
	}
}

// The broker's path decrypts once per write, not once per call: N resolves
// after a warm-up make no decryption at all (§10.3 constraint 4). Control:
// each still returns the value, and a write is picked up by the next one.
func TestResolveIsCheapPerCall(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.put(t, "TEST_KEY", "k1")
	e.s.SetGrants(ctx, "TEST_KEY", []int64{101}, false)
	e.s.Resolve(ctx, 101)
	before := e.s.Decrypts()
	for i := 0; i < 100; i++ {
		if got, _ := e.s.Resolve(ctx, 101); names(got) != "TEST_KEY=k1" {
			t.Fatalf("call %d got %q", i, names(got))
		}
	}
	if d := e.s.Decrypts() - before; d != 0 {
		t.Errorf("100 resolves made %d decryptions; want 0", d)
	}
	e.put(t, "TEST_KEY", "k2")
	if got, _ := e.s.Resolve(ctx, 101); names(got) != "TEST_KEY=k2" {
		t.Errorf("after a rotation the next resolve got %q", names(got))
	}
}

// A row written around Put — a value with a newline, which the write path
// refuses — is never delivered, and neither is anything else: a forged
// line must not reach a workspace by any route.
func TestUndeliverableRowFailsClosed(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.put(t, "GOOD", "fine")
	e.s.SetGrants(ctx, "GOOD", []int64{101}, false)
	if got, err := e.s.Resolve(ctx, 101); err != nil || names(got) != "GOOD=fine" {
		t.Fatalf("control: %q, %v", names(got), err)
	}
	id := "01JZZZZZZZZZZZZZZZZZZZZZZZ"
	ct, nonce, _ := e.s.seal(id, "hunter2\nGH_TOKEN ghp_forged")
	e.db.ExecContext(ctx, `INSERT INTO secret (id, name, ciphertext, nonce, reach) VALUES (?, 'FORGED', ?, ?, 'x')`, id, ct, nonce)
	e.db.ExecContext(ctx, `INSERT INTO secret_grant (secret_id, repository_id) VALUES (?, 101)`, id)
	e.s.Invalidate()
	if got, err := e.s.Resolve(ctx, 101); err == nil || len(got) != 0 {
		t.Errorf("a row with a newline delivered %q", names(got))
	}
	var n int
	e.db.QueryRow(`SELECT count(*) FROM event WHERE kind = 'secret.undeliverable'`).Scan(&n)
	if n != 1 {
		t.Errorf("%d undeliverable events; want 1", n)
	}
}

// ---- rotation and staleness (§10.3) -------------------------------------------------

func TestRotationAndStaleness(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	r := e.put(t, "TEST_KEY", "k1")
	if !r.Created || r.Rotated || len(r.Stale.NewCommands)+len(r.Stale.NeedsSupervisorRestart) != 0 || r.Secret.RotatedAt != nil {
		t.Errorf("create: %+v", r)
	}
	e.s.SetGrants(ctx, "TEST_KEY", []int64{101}, false)

	// Same value, new prose: not a rotation, nothing stale.
	r2, err := e.s.Put(ctx, "TEST_KEY", "k1", "a different reach", "rotated monthly")
	if err != nil || r2.Created || r2.Rotated || len(r2.Stale.NewCommands) != 0 || r2.Secret.Reach != "a different reach" {
		t.Errorf("prose only: %+v %v", r2, err)
	}

	e.clock.Advance(time.Hour)
	r3 := e.put(t, "TEST_KEY", "k2")
	// The running workspace on the granted repository; not the stopped one
	// on the same repository, not the one on another repository.
	if !r3.Rotated || len(r3.Stale.NewCommands) != 1 || r3.Stale.NewCommands[0].WorkspaceID != "01JAAAAAAAAAAAAAAAAAAAAAAA" ||
		r3.Stale.NewCommands[0].FullName != "krelinga/alpha" || len(r3.Stale.NeedsSupervisorRestart) != 0 {
		t.Errorf("rotate: %+v", r3)
	}
	if r3.Secret.RotatedAt == nil || !r3.Secret.RotatedAt.Equal(t0.Add(time.Hour)) {
		t.Errorf("rotated_at = %v", r3.Secret.RotatedAt)
	}

	// The kind is decided by StaleKind, not guessed.
	e.s.StaleKind = func(_ context.Context, ws string) StaleKind { return StaleNeedsSupervisorRestart }
	r4 := e.put(t, "TEST_KEY", "k3")
	if len(r4.Stale.NeedsSupervisorRestart) != 1 || len(r4.Stale.NewCommands) != 0 {
		t.Errorf("with a supervisor that needs a restart: %+v", r4.Stale)
	}

	// The event carries the split for the reducer, and no value.
	var data string
	e.db.QueryRow(`SELECT data FROM event WHERE kind = 'secret.rotated' ORDER BY id DESC LIMIT 1`).Scan(&data)
	if !strings.Contains(data, `"needs_supervisor_restart":[{"workspace_id":"01JAAAAAAAAAAAAAAAAAAAAAAA"`) || strings.Contains(data, "k3") {
		t.Errorf("secret.rotated data = %s", data)
	}
}

// A grant change rotates nothing (frontend §6.4).
func TestGrantChangeIsNotStale(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.put(t, "TEST_KEY", "k1")
	before, _ := e.s.Get(ctx, "TEST_KEY")
	e.s.SetGrants(ctx, "TEST_KEY", []int64{101}, false)
	after, _ := e.s.Get(ctx, "TEST_KEY")
	if after.RotatedAt != nil || before.RotatedAt != nil {
		t.Error("a grant change set rotated_at")
	}
	var n int
	e.db.QueryRow(`SELECT count(*) FROM event WHERE kind = 'secret.rotated'`).Scan(&n)
	if n != 0 {
		t.Error("a grant change emitted secret.rotated")
	}
}

func TestDeleteAndAccessLog(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.put(t, "TEST_KEY", "k1")
	e.s.SetGrants(ctx, "TEST_KEY", []int64{101}, false)
	got, _ := e.s.Resolve(ctx, 101)
	e.s.RecordAccess(ctx, "01JAAAAAAAAAAAAAAAAAAAAAAA", got)
	e.clock.Advance(time.Minute)
	e.s.RecordAccess(ctx, "01JAAAAAAAAAAAAAAAAAAAAAAA", got)
	m, _ := e.s.Get(ctx, "TEST_KEY")
	if m.LastAccessAt == nil || !m.LastAccessAt.Equal(t0.Add(time.Minute)) || len(m.AccessedBy) != 1 {
		t.Errorf("access: %+v", m)
	}

	if err := e.s.Delete(ctx, "TEST_KEY"); err != nil {
		t.Fatal(err)
	}
	if got, _ := e.s.Resolve(ctx, 101); len(got) != 0 {
		t.Errorf("a deleted secret was delivered: %q", names(got))
	}
	var grants, access int
	e.db.QueryRow(`SELECT count(*) FROM secret_grant`).Scan(&grants)
	e.db.QueryRow(`SELECT count(*) FROM secret_access`).Scan(&access)
	if grants != 0 || access != 2 {
		t.Errorf("after delete: %d grants (want 0), %d access rows (want 2: the history outlives it)", grants, access)
	}
	if err := e.s.Delete(ctx, "TEST_KEY"); !errors.Is(err, ErrNotFound) {
		t.Errorf("deleting twice: %v", err)
	}
}

// A replaced master key: the old rows do not open, so nothing is delivered
// rather than garbage — and putting the value again repairs the row.
func TestReplacedMasterKey(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.put(t, "TEST_KEY", "k1")
	e.s.SetGrants(ctx, "TEST_KEY", []int64{101}, false)
	other := &Store{DB: e.db.DB, Key: testKey(t), Env: e.s.Env}
	if got, err := other.Resolve(ctx, 101); err == nil {
		t.Errorf("another key delivered %q", names(got))
	}
	if _, err := other.Put(ctx, "TEST_KEY", "k1", "reach", ""); err != nil {
		t.Fatal(err)
	}
	if got, err := other.Resolve(ctx, 101); err != nil || names(got) != "TEST_KEY=k1" {
		t.Errorf("after a re-put: %q, %v", names(got), err)
	}
}
