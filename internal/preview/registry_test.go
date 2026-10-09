package preview_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"io"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/krelinga/drydock/internal/events"
	"github.com/krelinga/drydock/internal/preview"
	"github.com/krelinga/drydock/internal/store"
	"github.com/krelinga/drydock/internal/sys"
)

// registry is one running workspace of repository o/myapp, with no ports, the
// event log, and a recorder of Revoked.
type registry struct {
	svc     *preview.Service
	log     *events.Log
	db      *store.DB
	revoked []string
}

func newRegistry(t *testing.T, random io.Reader) *registry {
	t.Helper()
	ctx := context.Background()
	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "drydock.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	for _, q := range []string{
		`INSERT INTO repository (id, installation_id, full_name, default_branch) VALUES (1, 1, 'o/myapp', 'main')`,
		`INSERT INTO workspace (id, repository_id, host_path, branch, state) VALUES ('w1', 1, '/x', 'main', 'running')`,
		`INSERT INTO workspace (id, repository_id, host_path, branch, state) VALUES ('w2', 1, '/y', 'main', 'running')`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	clock := sys.NewFakeClock(time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC))
	r := &registry{db: db, log: events.New(db.DB, clock)}
	r.svc = &preview.Service{DB: db.DB, Clock: clock, Random: random, Domain: domain, Events: r.log,
		Revoked: func(id string) { r.revoked = append(r.revoked, id) }}
	return r
}

// kinds is every port event written so far, in order, as "kind:port".
func (r *registry) kinds(t *testing.T) []string {
	t.Helper()
	es, err := r.log.Since(context.Background(), 0)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range es {
		var d struct {
			Port struct {
				ContainerPort int `json:"container_port"`
			} `json:"port"`
			ContainerPort int `json:"container_port"`
		}
		json.Unmarshal(e.Data, &d)
		n := d.Port.ContainerPort + d.ContainerPort
		out = append(out, e.Kind+":"+itoa(n))
	}
	return out
}

func itoa(n int) string { b, _ := json.Marshal(n); return string(b) }

// script is a random source that reads out exactly the bytes given, so a test
// decides every id and every slug suffix.
func script(chunks ...[]byte) io.Reader { return bytes.NewReader(bytes.Join(chunks, nil)) }

func fill(b byte, n int) []byte { return bytes.Repeat([]byte{b}, n) }

// TestARetiredSlugIsNeverReissued is PF §4's guarantee as the registry keeps
// it: a port retired and listed again gets a new slug even when the random
// draw lands on the retired one — the UNIQUE on slug covers retired rows, and
// the mint draws again. The control is the first draw, taken when nothing
// holds it.
func TestARetiredSlugIsNeverReissued(t *testing.T) {
	// Each insert reads 10 bytes for the row id, then 4 per slug draw:
	// 0,0,0,0 is "aaaa" and 1,1,1,1 is "bbbb".
	r := newRegistry(t, script(
		fill(1, 10), fill(0, 4), // the first add: "aaaa", free
		fill(2, 10), fill(0, 4), fill(1, 4), // the second: "aaaa" is spent, so "bbbb"
	))
	ctx := context.Background()
	first, err := r.svc.Add(ctx, "w1", preview.AddSpec{ContainerPort: 3000})
	if err != nil {
		t.Fatal(err)
	}
	if first.Slug != "myapp-3000-aaaa" {
		t.Fatalf("control: the first draw = %q", first.Slug)
	}
	if err := r.svc.Retire(ctx, "w1", first.ID); err != nil {
		t.Fatal(err)
	}
	again, err := r.svc.Add(ctx, "w1", preview.AddSpec{ContainerPort: 3000})
	if err != nil {
		t.Fatal(err)
	}
	if again.Slug == first.Slug || again.Slug != "myapp-3000-bbbb" {
		t.Errorf("the port listed again has slug %q; want a new one, not the retired %q", again.Slug, first.Slug)
	}
	var retired int
	r.db.QueryRow(`SELECT count(*) FROM forwarded_port WHERE slug = ? AND retired_at IS NOT NULL`, first.Slug).Scan(&retired)
	if retired != 1 {
		t.Errorf("the retired row is gone (%d): its slug is no longer spent", retired)
	}
	// A slug every draw collides with is an error, never a reuse.
	r2 := newRegistry(t, script(fill(1, 10), fill(0, 4), fill(2, 10), fill(0, 4*8)))
	p, _ := r2.svc.Add(ctx, "w1", preview.AddSpec{ContainerPort: 3000})
	r2.svc.Retire(ctx, "w1", p.ID)
	if _, err := r2.svc.Add(ctx, "w2", preview.AddSpec{ContainerPort: 3000}); err == nil {
		t.Error("eight draws of a spent slug were answered with a port")
	}
}

func TestMintSlug(t *testing.T) {
	for _, c := range []struct {
		name string
		port int
		want string
	}{
		{"o/myapp", 5173, "myapp-5173-p2mq"},
		{"o/My_App.js", 3000, "my-app-js-3000-p2mq"},
		{"o/--x--", 80, "x-80-p2mq"},
		{"o/___", 8080, "port-8080-p2mq"},
		{"o/ünïcode", 1, "n-code-1-p2mq"},
		{"", 9, "port-9-p2mq"},
	} {
		if got := preview.MintSlug(c.name, c.port, "p2mq"); got != c.want {
			t.Errorf("MintSlug(%q, %d) = %q; want %q", c.name, c.port, got, c.want)
		}
	}
	long := preview.MintSlug("o/"+strings.Repeat("abc-", 30), 65535, "zzzz")
	if len(long) > 63 || !strings.HasSuffix(long, "-65535-zzzz") {
		t.Errorf("a long name's slug = %q (%d)", long, len(long))
	}
	for _, s := range []string{long, preview.MintSlug("o/drydock-check", 1, "abcd")} {
		if _, ok := preview.Slug(s+"."+domain, domain); !ok {
			t.Errorf("%q is not a preview host's slug", s)
		}
	}
}

// TestPortsAreOffUntilEnabled: an added port is disabled and has no URL; the
// enable is the one thing that gives it one, and each call writes exactly one
// event. Enabling is refused with no preview domain, and on a workspace being
// deleted — where a disable still works.
func TestPortsAreOffUntilEnabled(t *testing.T) {
	r := newRegistry(t, rand.Reader)
	ctx := context.Background()
	p, err := r.svc.Add(ctx, "w1", preview.AddSpec{ContainerPort: 5173, Label: " vite "})
	if err != nil {
		t.Fatal(err)
	}
	if p.Enabled || p.URL != nil || !p.Manual || p.Declared || p.Label == nil || *p.Label != "vite" || p.HostHeader != preview.HostLocalhost {
		t.Fatalf("an added port = %+v", p)
	}
	if p.Host == nil || *p.Host != p.Slug+"."+domain {
		t.Errorf("host = %v", p.Host)
	}
	on, err := r.svc.SetEnabled(ctx, "w1", p.ID, true)
	if err != nil || !on.Enabled || on.URL == nil || *on.URL != "https://"+p.Slug+"."+domain+"/" {
		t.Fatalf("enabled = %+v, %v", on, err)
	}
	pass := preview.HostPassthrough
	if u, err := r.svc.Update(ctx, "w1", p.ID, preview.Change{HostHeader: &pass}); err != nil || u.HostHeader != pass || !u.Enabled {
		t.Errorf("host_header switch = %+v, %v", u, err)
	}
	if _, err := r.svc.Add(ctx, "w1", preview.AddSpec{ContainerPort: 5173}); !errors.Is(err, preview.ErrPortExists) {
		t.Errorf("a second add of the same port = %v", err)
	}
	if _, err := r.svc.SetEnabled(ctx, "w2", p.ID, false); !errors.Is(err, preview.ErrNoPort) {
		t.Errorf("another workspace's port = %v; want ErrNoPort", err)
	}
	bad := "hostile"
	var inv *preview.InvalidError
	if _, err := r.svc.Update(ctx, "w1", p.ID, preview.Change{HostHeader: &bad}); !errors.As(err, &inv) {
		t.Errorf("host_header %q = %v", bad, err)
	}
	for _, port := range []int{0, -1, 65536} {
		if _, err := r.svc.Add(ctx, "w1", preview.AddSpec{ContainerPort: port}); !errors.As(err, &inv) {
			t.Errorf("port %d = %v", port, err)
		}
	}
	if _, err := r.svc.Add(ctx, "w1", preview.AddSpec{ContainerPort: 1, Label: "two\nlines"}); !errors.As(err, &inv) {
		t.Errorf("a two-line label = %v", err)
	}
	if got := r.kinds(t); strings.Join(got, " ") != "port.added:5173 port.enabled:5173 port.updated:5173" {
		t.Errorf("events = %v", got)
	}

	off := &preview.Service{DB: r.svc.DB, Clock: r.svc.Clock, Random: r.svc.Random, Events: r.log}
	if _, err := off.SetEnabled(ctx, "w1", p.ID, true); !errors.Is(err, preview.ErrPreviewsOff) {
		t.Errorf("enabling with no preview domain = %v", err)
	}
	r.db.Exec(`UPDATE workspace SET state = 'deleting' WHERE id = 'w1'`)
	if _, err := r.svc.SetEnabled(ctx, "w1", p.ID, true); !errors.Is(err, preview.ErrWorkspaceDeleting) {
		t.Errorf("enabling on a deleting workspace = %v", err)
	}
	if _, err := r.svc.Add(ctx, "w1", preview.AddSpec{ContainerPort: 9}); !errors.Is(err, preview.ErrWorkspaceDeleting) {
		t.Errorf("adding on a deleting workspace = %v", err)
	}
	if _, err := r.svc.SetEnabled(ctx, "w1", p.ID, false); err != nil {
		t.Errorf("control: disabling on a deleting workspace = %v", err)
	}
	if _, err := r.svc.Ports(ctx, "nosuch", false); !errors.Is(err, preview.ErrNoWorkspace) {
		t.Errorf("ports of no workspace = %v", err)
	}
}

// TestDisableAndRetireTellRevoked: the server closes a port's websockets on
// Revoked, so it is told of every disable and retire — after the commit,
// with the port's id — and of no enable (the control).
func TestDisableAndRetireTellRevoked(t *testing.T) {
	r := newRegistry(t, rand.Reader)
	ctx := context.Background()
	p, _ := r.svc.Add(ctx, "w1", preview.AddSpec{ContainerPort: 5173})
	q, _ := r.svc.Add(ctx, "w1", preview.AddSpec{ContainerPort: 8080})
	r.svc.SetEnabled(ctx, "w1", p.ID, true)
	hidden := true
	r.svc.Update(ctx, "w1", p.ID, preview.Change{Hidden: &hidden})
	if len(r.revoked) != 0 {
		t.Fatalf("control: an enable or a hide told Revoked %v", r.revoked)
	}
	r.svc.SetEnabled(ctx, "w1", p.ID, false)
	r.svc.Retire(ctx, "w1", q.ID)
	if strings.Join(r.revoked, ",") != p.ID+","+q.ID {
		t.Errorf("Revoked was told %v; want the disabled port, then the retired one", r.revoked)
	}
	if err := r.svc.Retire(ctx, "w1", q.ID); !errors.Is(err, preview.ErrNoPort) {
		t.Errorf("retiring a retired port = %v", err)
	}
	if ps, _ := r.svc.Ports(ctx, "w1", false); len(ps) != 0 {
		t.Errorf("the default list holds %d ports; the hidden one and the retired one belong to neither", len(ps))
	}
	if ps, _ := r.svc.Ports(ctx, "w1", true); len(ps) != 1 || ps[0].ID != p.ID {
		t.Errorf("?hidden=true lists %+v; want the hidden port alone", ps)
	}
}

// TestDeclaredPortsAreListedNeverEnabled is the declared half: the
// configuration's ports become rows, declared and off; a row it stops naming
// loses the flag, and goes if nothing else holds it; a port the operator
// enabled stays enabled whatever the configuration says.
func TestDeclaredPortsAreListedNeverEnabled(t *testing.T) {
	r := newRegistry(t, rand.Reader)
	ctx := context.Background()
	if err := r.svc.DeclarePorts(ctx, "w1", []preview.Declared{{Port: 5173, Label: "vite"}, {Port: 8080}, {Port: 5173}, {Port: 0}}); err != nil {
		t.Fatal(err)
	}
	ps, _ := r.svc.Ports(ctx, "w1", true)
	if len(ps) != 2 || ps[0].ContainerPort != 5173 || ps[1].ContainerPort != 8080 {
		t.Fatalf("declared rows = %+v", ps)
	}
	for _, p := range ps {
		if p.Enabled || !p.Declared || p.Manual {
			t.Errorf("a declared row = %+v; want declared and off", p)
		}
	}
	if ps[0].Label == nil || *ps[0].Label != "vite" {
		t.Errorf("the declared label = %v", ps[0].Label)
	}
	// Idempotent: the same declaration writes nothing.
	before := len(r.kinds(t))
	r.svc.DeclarePorts(ctx, "w1", []preview.Declared{{Port: 5173, Label: "vite"}, {Port: 8080}})
	if after := len(r.kinds(t)); after != before {
		t.Errorf("declaring the same ports again wrote %d events", after-before)
	}
	// The operator enables 8080 and adds 9000; the configuration then names
	// 5173 alone, then nothing.
	r.svc.SetEnabled(ctx, "w1", ps[1].ID, true)
	r.svc.Add(ctx, "w1", preview.AddSpec{ContainerPort: 9000})
	r.svc.DeclarePorts(ctx, "w1", []preview.Declared{{Port: 5173}, {Port: 9000}})
	r.svc.DeclarePorts(ctx, "w1", nil)
	ps, _ = r.svc.Ports(ctx, "w1", true)
	got := map[int]preview.Port{}
	for _, p := range ps {
		got[p.ContainerPort] = p
	}
	if _, ok := got[5173]; ok {
		t.Error("a declared port nothing else holds outlived its declaration")
	}
	if p, ok := got[8080]; !ok || !p.Enabled || p.Declared {
		t.Errorf("the enabled port after its declaration went = %+v, %v; want kept, enabled, undeclared", p, ok)
	}
	if p, ok := got[9000]; !ok || !p.Manual || p.Declared || p.Enabled {
		t.Errorf("the hand-added port = %+v, %v", p, ok)
	}
	want := "port.added:5173 port.added:8080 port.enabled:8080 port.added:9000 " +
		"port.updated:9000 port.updated:8080 port.retired:5173 port.updated:9000"
	if strings.Join(r.kinds(t), " ") != want {
		t.Errorf("events =\n%v\nwant\n%v", strings.Join(r.kinds(t), " "), want)
	}

	// Bounded: a configuration naming hundreds of ports gets MaxDeclared rows.
	var many []preview.Declared
	for i := 1; i <= 500; i++ {
		many = append(many, preview.Declared{Port: 10000 + i})
	}
	r.svc.DeclarePorts(ctx, "w2", many)
	if ps, _ := r.svc.Ports(ctx, "w2", true); len(ps) != preview.MaxDeclared {
		t.Errorf("500 declared ports made %d rows; want %d", len(ps), preview.MaxDeclared)
	}
	// A workspace being deleted declares nothing.
	r.db.Exec(`UPDATE workspace SET state = 'deleting' WHERE id = 'w1'`)
	if err := r.svc.DeclarePorts(ctx, "w1", []preview.Declared{{Port: 1234}}); err != nil {
		t.Fatal(err)
	}
	if ps, _ := r.svc.Ports(ctx, "w1", true); len(ps) != 2 {
		t.Errorf("a deleting workspace gained a declared port: %+v", ps)
	}
}
