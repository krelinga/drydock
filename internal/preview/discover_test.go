package preview_test

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/krelinga/drydock/internal/events"
	"github.com/krelinga/drydock/internal/life"
	"github.com/krelinga/drydock/internal/preview"
	"github.com/krelinga/drydock/internal/sys"
)

// fakeSource is a container whose sockets a test sets, per workspace.
type fakeSource struct {
	mu    sync.Mutex
	ls    map[string][]preview.Listener
	err   map[string]error
	calls map[string]int
}

func newSource() *fakeSource {
	return &fakeSource{ls: map[string][]preview.Listener{}, err: map[string]error{}, calls: map[string]int{}}
}

func (f *fakeSource) Listeners(_ context.Context, id string) ([]preview.Listener, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls[id]++
	if err := f.err[id]; err != nil {
		return nil, err
	}
	if ls, ok := f.ls[id]; ok {
		return append([]preview.Listener(nil), ls...), nil
	}
	return nil, preview.ErrNotRunning
}

// listen sets what workspace id listens on: "0.0.0.0:5173", "[::1]:3000".
func (f *fakeSource) listen(id string, socks ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := []preview.Listener{}
	for _, s := range socks {
		ap := netip.MustParseAddrPort(s)
		out = append(out, preview.Listener{Port: int(ap.Port()), Addr: ap.Addr()})
	}
	f.ls[id] = out
	delete(f.err, id)
}

func (f *fakeSource) fail(id string, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.err[id] = err
}

type discovery struct {
	*registry
	src   *fakeSource
	sc    *preview.Scanner
	clock *sys.FakeClock
	logs  []string
}

func newDiscovery(t *testing.T) *discovery {
	t.Helper()
	r := newRegistry(t, nil)
	d := &discovery{registry: r, src: newSource(), clock: r.svc.Clock.(*sys.FakeClock)}
	r.svc.Random = rand.Reader
	d.sc = &preview.Scanner{Registry: r.svc, Source: d.src, Clock: d.clock, Interval: -1,
		Logf: func(f string, a ...any) { d.logs = append(d.logs, fmt.Sprintf(f, a...)) }}
	return d
}

// scan advances the clock by one interval and runs a round.
func (d *discovery) scan(asks ...string) {
	d.clock.Advance(preview.DefaultScanInterval)
	d.sc.Round(context.Background(), asks...)
}

// rows are a workspace's live rows, hidden ones included, by port.
func (d *discovery) rows(t *testing.T, ws string) map[int]preview.Port {
	t.Helper()
	ps, err := d.svc.Ports(context.Background(), ws, true)
	if err != nil {
		t.Fatal(err)
	}
	out := map[int]preview.Port{}
	for _, p := range ps {
		out[p.ContainerPort] = p
	}
	return out
}

func state(p preview.Port) string {
	if p.ObservedState == nil {
		return ""
	}
	return *p.ObservedState
}

func bind(p preview.Port) string {
	if p.BindAddr == nil {
		return ""
	}
	return *p.BindAddr
}

// TestDiscoveryListsAPortOffAndNeverEnablesIt: §13 step 5's done-when, as the
// registry sees it — a server on an undeclared port appears as a row, off,
// observed, with its bind address, announced by port.added — and no scan, of
// a new port or of one already switched on or off, writes the switch.
// Mutation-checked: Observe inserting the row enabled fails the first
// assertion; Observe writing enabled = 0 on a listed row fails the control.
func TestDiscoveryListsAPortOffAndNeverEnablesIt(t *testing.T) {
	d := newDiscovery(t)
	ctx := context.Background()
	on, err := d.svc.Add(ctx, "w1", preview.AddSpec{ContainerPort: 3000})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.svc.SetEnabled(ctx, "w1", on.ID, true); err != nil {
		t.Fatal(err)
	}
	d.src.listen("w1", "0.0.0.0:5173", "0.0.0.0:3000")
	d.scan()
	d.scan()
	rows := d.rows(t, "w1")
	p := rows[5173]
	if p.ID == "" || p.Enabled || p.URL != nil || !p.Observed || p.Declared || p.Manual {
		t.Fatalf("discovered row = %+v; want listed, observed and off", p)
	}
	if state(p) != "listening" || bind(p) != "0.0.0.0" || p.Loopback || p.LastSeenAt == nil {
		t.Errorf("observation = %q %q loopback=%v; want listening on 0.0.0.0", state(p), bind(p), p.Loopback)
	}
	// The control: the enabled one stays enabled, the merge only observes.
	if q := rows[3000]; !q.Enabled || !q.Observed || state(q) != "listening" || q.ID != on.ID {
		t.Errorf("an enabled row the scan saw = %+v; want still enabled, now observed", q)
	}
	var enabled int
	d.db.QueryRow(`SELECT count(*) FROM forwarded_port WHERE enabled = 1`).Scan(&enabled)
	if enabled != 1 {
		t.Errorf("%d rows enabled; want the one the operator enabled", enabled)
	}
	if got := strings.Join(d.kinds(t), " "); got != "port.added:3000 port.enabled:3000 port.updated:3000 port.added:5173" {
		t.Errorf("events = %s", got)
	}
}

// TestDiscoveryIsDebounced: one scan is not enough for a row (PF §8.2: two
// consecutive), a server restarting inside the grace period changes nothing
// and writes nothing, a bind address that changes takes two scans too, and a
// port unseen for the grace period goes — marked gone, and retired only
// RetireAfter later when discovery alone held it. Mutation-checked:
// AppearAfter 1 lists the port after one scan; a grace of zero (gone at the
// first scan that misses it) churns the restart.
func TestDiscoveryIsDebounced(t *testing.T) {
	d := newDiscovery(t)
	d.src.listen("w1", "0.0.0.0:5173")
	d.scan()
	if n := len(d.rows(t, "w1")); n != 0 || len(d.kinds(t)) != 0 {
		t.Fatalf("after one scan: %d rows, events %v; want none — a port appears after two", n, d.kinds(t))
	}
	d.scan()
	first := d.rows(t, "w1")[5173]
	if first.ID == "" {
		t.Fatal("after two scans the port is not listed (the control)")
	}
	// The dev server restarts: one scan misses it, the next sees it.
	d.src.listen("w1")
	d.scan()
	d.src.listen("w1", "0.0.0.0:5173")
	d.scan()
	d.scan()
	if got := d.kinds(t); len(got) != 1 {
		t.Errorf("a restart inside the grace wrote %v; want nothing past the first port.added", got)
	}
	if p := d.rows(t, "w1")[5173]; p.ID != first.ID || state(p) != "listening" {
		t.Errorf("after a restart the row is %+v; want the same row, still listening", p)
	}
	// Restarted on loopback: one scan is not a change, two are.
	d.src.listen("w1", "127.0.0.1:5173")
	d.scan()
	if p := d.rows(t, "w1")[5173]; bind(p) != "0.0.0.0" {
		t.Errorf("after one scan at the new bind: %q; want still 0.0.0.0", bind(p))
	}
	d.scan()
	if p := d.rows(t, "w1")[5173]; bind(p) != "127.0.0.1" || !p.Loopback || p.ID != first.ID {
		t.Errorf("after two at the new bind: %+v; want the same row, on loopback", p)
	}
	// Stopped: listed until the grace is over, then gone.
	d.src.listen("w1")
	for i := time.Duration(0); i+preview.DefaultScanInterval < preview.DefaultGrace; i += preview.DefaultScanInterval {
		d.scan()
		if _, ok := d.rows(t, "w1")[5173]; !ok {
			t.Fatalf("gone %s after the last sighting; the grace is %s", i+preview.DefaultScanInterval, preview.DefaultGrace)
		}
	}
	d.scan()
	if p := d.rows(t, "w1")[5173]; p.ID != first.ID || state(p) != "gone" {
		t.Errorf("after the grace: %+v; want the row kept, gone", p)
	}
	d.clock.Advance(preview.RetireAfter)
	d.scan()
	if _, ok := d.rows(t, "w1")[5173]; ok {
		t.Error("still listed RetireAfter after it went; want retired, as only discovery held it")
	}
	if got := strings.Join(d.kinds(t), " "); got != "port.added:5173 port.updated:5173 port.updated:5173 port.retired:5173" {
		t.Errorf("events = %s", got)
	}
}

// TestAServerThatComesBackKeepsItsRow: a port that stops for longer than the
// grace and listens again inside RetireAfter is the same row and the same
// slug — a dev server stopped for a coffee, or a test suite cycling one port,
// spends no slugs and adds no rows. Mutation-checked: RetireAfter zero (retire
// at the grace, as before) mints a row per cycle.
func TestAServerThatComesBackKeepsItsRow(t *testing.T) {
	d := newDiscovery(t)
	d.src.listen("w1", "0.0.0.0:5173")
	d.scan()
	d.scan()
	first := d.rows(t, "w1")[5173]
	for cycle := 0; cycle < 5; cycle++ {
		d.src.listen("w1")
		for i := 0; i < 4; i++ {
			d.scan() // past the grace: gone
		}
		if p := d.rows(t, "w1")[5173]; p.ID != first.ID || state(p) != "gone" {
			t.Fatalf("cycle %d, stopped: %+v; want the same row, gone", cycle, p)
		}
		d.src.listen("w1", "0.0.0.0:5173")
		d.scan()
		d.scan()
		if p := d.rows(t, "w1")[5173]; p.ID != first.ID || p.Slug != first.Slug || state(p) != "listening" {
			t.Fatalf("cycle %d, back: %+v; want the same row and slug, listening", cycle, p)
		}
	}
	var n int
	d.db.QueryRow(`SELECT count(*) FROM forwarded_port`).Scan(&n)
	if n != 1 {
		t.Errorf("%d rows, retired ones included; want one", n)
	}
}

// TestDiscoveryChurnIsBudgeted: a container that opens 32 fresh ports, closes
// them past the grace and does it again — the review's worst case, and any
// test suite on ephemeral ports — for two simulated hours, against a control
// with the same budget idle. New rows stay within MintBurst plus one per
// MintEvery, events within ChangeBurst plus one per ChangeEvery, and the
// journal hears of it once. Mutation-checked: budget passing every change
// through mints a row per port per cycle.
func TestDiscoveryChurnIsBudgeted(t *testing.T) {
	d := newDiscovery(t)
	const span = 2 * time.Hour
	scans := int(span / preview.DefaultScanInterval)
	base := 20000
	for i := 0; i < scans; i++ {
		if i%6 < 2 { // up for two scans, then down for four (past the grace)
			var socks []string
			for p := 0; p < 32; p++ {
				socks = append(socks, fmt.Sprintf("0.0.0.0:%d", base+p))
			}
			d.src.listen("w1", socks...)
		} else {
			if i%6 == 2 {
				base += 32 // the next cycle's ports are new ones
			}
			d.src.listen("w1")
		}
		d.scan()
	}
	var rows int
	d.db.QueryRow(`SELECT count(*) FROM forwarded_port`).Scan(&rows)
	maxRows := preview.MintBurst + int(span/preview.MintEvery) + 1
	if rows == 0 || rows > maxRows {
		t.Errorf("%d rows minted in %s; want some, and at most %d", rows, span, maxRows)
	}
	es, _ := d.log.Since(context.Background(), 0)
	// Every row change spends a change token; the state reports
	// (port.discovery, "limited" and back) have their own budget.
	maxEvents := preview.ChangeBurst + int(span/preview.ChangeEvery) + 1 + preview.StatusBurst + int(span/preview.StatusEvery) + 1
	if len(es) > maxEvents {
		t.Errorf("%d events in %s; want at most %d", len(es), span, maxEvents)
	}
	n := 0
	for _, l := range d.logs {
		if strings.Contains(l, "holding back") {
			n++
		}
	}
	if n == 0 || n > scans/6 {
		t.Errorf("journal said it was holding back %d times; want it said, not every scan", n)
	}
	// The control: a workspace within its budget is never held back — its
	// first ports are listed at once.
	c := newDiscovery(t)
	c.src.listen("w1", "0.0.0.0:3000", "0.0.0.0:3001")
	c.scan()
	c.scan()
	if len(c.rows(t, "w1")) != 2 || len(c.logs) != 0 {
		t.Errorf("within budget: %d rows, journal %q", len(c.rows(t, "w1")), c.logs)
	}
}

// blockingSource is a source whose read for one workspace never ends until
// its context does — a docker call hung on a wedged daemon — and answers the
// rest from inner.
type blockingSource struct {
	inner  *fakeSource
	block  string
	inside chan struct{}
}

func (b *blockingSource) Listeners(ctx context.Context, id string) ([]preview.Listener, error) {
	if id == b.block {
		close(b.inside)
		<-ctx.Done()
		return nil, ctx.Err()
	}
	return b.inner.Listeners(ctx, id)
}

// TestAHungReadCostsOneWorkspace: one workspace's read hangs; the round
// waits for it only DefaultScanTimeout on the injected clock, answers its
// rescan "unavailable", and goes on to scan the next workspace, whose rescan
// is answered "ok" and whose port is seen. Mutation-checked: the read under
// the round's own context hangs the round until shutdown.
func TestAHungReadCostsOneWorkspace(t *testing.T) {
	d := newDiscovery(t)
	d.src.listen("w2", "0.0.0.0:8080")
	src := &blockingSource{inner: d.src, block: "w1", inside: make(chan struct{})}
	d.sc.Source = src
	done := make(chan struct{})
	go func() {
		defer close(done)
		d.sc.Round(context.Background(), "w1", "w2")
	}()
	<-src.inside
	deadline := time.Now().Add(10 * time.Second)
	for d.clock.Waiting() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("the hung read set no timer on the injected clock")
		}
		time.Sleep(time.Millisecond)
	}
	d.clock.Advance(preview.DefaultScanTimeout)
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the round is still waiting on the hung read")
	}
	es, _ := d.log.Since(context.Background(), 0)
	verdict := map[string]string{}
	for _, e := range es {
		if e.Kind == preview.KindPortScanned {
			var v struct{ Discovery string }
			json.Unmarshal(e.Data, &v)
			verdict[e.WorkspaceID] = v.Discovery
		}
	}
	if verdict["w1"] != "unavailable" || verdict["w2"] != "ok" {
		t.Errorf("rescans answered %v; want w1 unavailable, w2 ok", verdict)
	}
	if d.src.calls["w2"] != 1 {
		t.Errorf("w2 was read %d times; want once, after w1's read was cut off", d.src.calls["w2"])
	}
	if len(d.logs) != 1 || !strings.Contains(d.logs[0], "longer than") {
		t.Errorf("journal = %q; want the cut-off said once", d.logs)
	}
}

// TestDiscoveryBacksOffWhileNobodyWatches: with no SSE stream open, a round
// nobody asked for runs at most every IdleInterval; a rescan runs at once;
// and the moment someone watches, every interval scans again.
func TestDiscoveryBacksOffWhileNobodyWatches(t *testing.T) {
	d := newDiscovery(t)
	watched := false
	d.sc.Watched = func() bool { return watched }
	d.src.listen("w1", "0.0.0.0:8080")
	reads := func() int { d.src.mu.Lock(); defer d.src.mu.Unlock(); return d.src.calls["w1"] }
	d.scan() // the first round always runs
	if reads() != 1 {
		t.Fatalf("control: %d reads", reads())
	}
	for i := 0; i < 5; i++ {
		d.scan() // 25 s, unwatched
	}
	if reads() != 1 {
		t.Errorf("unwatched, %d reads in 25 s; want none past the first", reads())
	}
	d.scan("w1")
	if reads() != 2 {
		t.Errorf("a rescan read %d times; want it at once", reads()-1)
	}
	for i := 0; i < int(preview.IdleInterval/preview.DefaultScanInterval); i++ {
		d.scan()
	}
	if reads() != 3 {
		t.Errorf("after an idle interval: %d reads; want one more", reads())
	}
	watched = true
	d.scan()
	d.scan()
	if reads() != 5 {
		t.Errorf("watched: %d reads; want every interval", reads())
	}
}

// TestDiscoveryMergesOntoDeclaredRows: a port the configuration declares and
// a server listens on is one row with both flags (PF §8.2), never two; the
// declaration's label stays; stopping it marks it gone rather than retiring
// it — the declaration holds it — and a row both declared and listening
// survives its declaration going, until it stops.
func TestDiscoveryMergesOntoDeclaredRows(t *testing.T) {
	d := newDiscovery(t)
	ctx := context.Background()
	if err := d.svc.DeclarePorts(ctx, "w1", []preview.Declared{{Port: 5173, Label: "vite"}}); err != nil {
		t.Fatal(err)
	}
	declared := d.rows(t, "w1")[5173]
	d.src.listen("w1", "[::]:5173", "0.0.0.0:5173", "127.0.0.1:5173")
	d.scan()
	d.scan()
	rows := d.rows(t, "w1")
	p := rows[5173]
	if len(rows) != 1 || p.ID != declared.ID || !p.Declared || !p.Observed || p.Label == nil || *p.Label != "vite" {
		t.Fatalf("rows = %+v; want the declared row, now observed too", rows)
	}
	if bind(p) != "0.0.0.0" || p.Loopback {
		t.Errorf("bind %q; want the widest of the three sockets, 0.0.0.0", bind(p))
	}
	// The declaration goes while the server listens: kept, undeclared.
	if err := d.svc.DeclarePorts(ctx, "w1", nil); err != nil {
		t.Fatal(err)
	}
	if p := d.rows(t, "w1")[5173]; p.ID != declared.ID || p.Declared {
		t.Fatalf("after the declaration went, listening: %+v; want kept, undeclared", p)
	}
	// Declared again, then the server stops: gone, kept.
	if err := d.svc.DeclarePorts(ctx, "w1", []preview.Declared{{Port: 5173}}); err != nil {
		t.Fatal(err)
	}
	d.src.listen("w1")
	d.clock.Advance(preview.DefaultGrace)
	d.scan()
	if p := d.rows(t, "w1")[5173]; p.ID != declared.ID || state(p) != "gone" || !p.Declared {
		t.Errorf("declared, stopped: %+v; want the row kept, gone", p)
	}
	// And a declared row that has stopped listening is retired when its
	// declaration goes, as step 4 retires one never seen.
	if err := d.svc.DeclarePorts(ctx, "w1", nil); err != nil {
		t.Fatal(err)
	}
	if _, ok := d.rows(t, "w1")[5173]; ok {
		t.Error("declaration gone and not listening: still listed")
	}
	for _, k := range d.kinds(t) {
		if k == "port.added:5173" && k != d.kinds(t)[0] {
			t.Errorf("a second row was added for a declared port: %v", d.kinds(t))
		}
	}
}

// TestARetiredDiscoveredPortStaysRetired: a discovered port the operator
// removes, still listening, comes back as a new row with a new slug (PF §6),
// and the retired row and its slug stay spent — even when the random draw
// lands on the retired slug. Mutation-checked: merging onto any row of the
// port, retired ones included (and clearing retired_at), revives the old
// slug.
func TestARetiredDiscoveredPortStaysRetired(t *testing.T) {
	d := newDiscovery(t)
	d.svc.Random = script(
		fill(1, 10), fill(0, 4), // the first row: "aaaa"
		fill(2, 10), fill(0, 4), fill(1, 4), // the second draws "aaaa" again, spent, then "bbbb"
	)
	ctx := context.Background()
	d.src.listen("w1", "0.0.0.0:5173")
	d.scan()
	d.scan()
	old := d.rows(t, "w1")[5173]
	if old.Slug != "myapp-5173-aaaa" {
		t.Fatalf("control: %q", old.Slug)
	}
	if err := d.svc.Retire(ctx, "w1", old.ID); err != nil {
		t.Fatal(err)
	}
	d.scan()
	again := d.rows(t, "w1")[5173]
	if again.ID == "" || again.ID == old.ID || again.Slug != "myapp-5173-bbbb" || again.Enabled {
		t.Errorf("listed again as %+v; want a new row, a new slug, off", again)
	}
	var retired int
	d.db.QueryRow(`SELECT count(*) FROM forwarded_port WHERE id = ? AND slug = ? AND retired_at IS NOT NULL`,
		old.ID, old.Slug).Scan(&retired)
	if retired != 1 {
		t.Error("the retired row was revived or deleted: its slug is no longer spent")
	}
}

// TestWhatHoldsADiscoveredRow: a stopped port is kept, marked gone, when the
// operator enabled it, added it by hand or hid it — a hidden row coming back
// as a new visible one would undo the hide — and retired when discovery alone
// held it. MaxObserved bounds the rows discovery alone makes.
func TestWhatHoldsADiscoveredRow(t *testing.T) {
	d := newDiscovery(t)
	ctx := context.Background()
	if _, err := d.svc.Add(ctx, "w1", preview.AddSpec{ContainerPort: 4000}); err != nil {
		t.Fatal(err)
	}
	d.src.listen("w1", "0.0.0.0:4000", "0.0.0.0:5000", "0.0.0.0:6000", "0.0.0.0:7000")
	d.scan()
	d.scan()
	rows := d.rows(t, "w1")
	yes := true
	if _, err := d.svc.Update(ctx, "w1", rows[5000].ID, preview.Change{Enabled: &yes}); err != nil {
		t.Fatal(err)
	}
	if _, err := d.svc.Update(ctx, "w1", rows[6000].ID, preview.Change{Hidden: &yes}); err != nil {
		t.Fatal(err)
	}
	d.src.listen("w1")
	d.clock.Advance(preview.DefaultGrace)
	d.scan()
	d.clock.Advance(preview.RetireAfter)
	d.scan()
	rows = d.rows(t, "w1")
	for _, port := range []int{4000, 5000, 6000} {
		if state(rows[port]) != "gone" {
			t.Errorf("port %d: %+v; want kept, gone", port, rows[port])
		}
	}
	if _, ok := rows[7000]; ok {
		t.Error("port 7000, held by discovery alone, is still listed RetireAfter after it went")
	}
	if !rows[5000].Enabled {
		t.Error("the gone port was switched off by the scan")
	}

	// The cap: forty listeners, at most MaxObserved rows of discovery's own.
	d2 := newDiscovery(t)
	var socks []string
	for p := 9000; p < 9040; p++ {
		socks = append(socks, fmt.Sprintf("0.0.0.0:%d", p))
	}
	d2.src.listen("w1", socks...)
	for i := 0; i < 2+preview.MaxObserved; i++ {
		d2.clock.Advance(preview.MintEvery) // the mint budget is not what this tests
		d2.sc.Round(context.Background())
	}
	if n := len(d2.rows(t, "w1")); n != preview.MaxObserved {
		t.Errorf("%d rows from forty listeners; want %d", n, preview.MaxObserved)
	}
}

// TestDiscoveryScansWhatIsNotRunningAsEmpty: a workspace whose container is
// gone (the source finds none) and one whose row is stopped are scanned as
// empty — their rows go after the grace, never kept listening on a stale
// answer — and a stopped one is never asked of Docker.
func TestDiscoveryScansWhatIsNotRunningAsEmpty(t *testing.T) {
	d := newDiscovery(t)
	d.src.listen("w1", "0.0.0.0:5173")
	d.src.listen("w2", "0.0.0.0:8080")
	d.scan()
	d.scan()
	if len(d.rows(t, "w1")) != 1 || len(d.rows(t, "w2")) != 1 {
		t.Fatal("control: both listed")
	}
	if _, err := d.db.Exec(`UPDATE workspace SET state = 'stopped' WHERE id = 'w2'`); err != nil {
		t.Fatal(err)
	}
	d.src.mu.Lock()
	delete(d.src.ls, "w1") // the container died behind Drydock's back
	before := d.src.calls["w2"]
	d.src.mu.Unlock()
	d.clock.Advance(preview.DefaultGrace)
	d.scan()
	for _, ws := range []string{"w1", "w2"} {
		for _, p := range d.rows(t, ws) {
			if state(p) != "gone" {
				t.Errorf("%s: %+v left listening", ws, p)
			}
		}
	}
	d.clock.Advance(preview.RetireAfter)
	d.scan()
	if len(d.rows(t, "w1")) != 0 || len(d.rows(t, "w2")) != 0 {
		t.Errorf("rows left: %v %v", d.rows(t, "w1"), d.rows(t, "w2"))
	}
	if d.src.calls["w2"] != before {
		t.Error("a stopped workspace was asked of its container")
	}
	// Nothing left listening and not running: no longer scanned at all.
	d.scan()
	if d.src.calls["w2"] != before {
		t.Error("a stopped workspace with nothing listening is still scanned")
	}
}

// TestDiscoveryUnavailableChangesNothing: a table that cannot be read is not
// an empty one (PF §11): the rows keep what they said however long it lasts,
// a rescan is answered "unavailable", and the journal is told once, then
// once more when it recovers. A read that raced a restart teaches nothing
// either. And it fails visibly (§11, step 6): the first unasked round that
// cannot read the table says so with port.discovery, the port list carries
// it, and the round that reads again says that too.
func TestDiscoveryUnavailableChangesNothing(t *testing.T) {
	d := newDiscovery(t)
	d.src.listen("w1", "0.0.0.0:5173")
	d.scan()
	d.scan()
	for _, err := range []error{errors.New("permission denied"), preview.ErrScanRaced} {
		d.src.fail("w1", err)
		d.clock.Advance(preview.DefaultGrace * 4)
		d.scan()
		d.scan("w1")
		if p := d.rows(t, "w1")[5173]; state(p) != "listening" {
			t.Errorf("%v: the row is %+v; want unchanged", err, p)
		}
	}
	if got := d.sc.Discovery("w1"); got != preview.DiscoveryUnavailable {
		t.Errorf("the port list would say discovery is %q; want unavailable", got)
	}
	d.src.listen("w1", "0.0.0.0:5173")
	d.scan()
	kinds := d.kinds(t)
	if got := strings.Join(kinds, " "); got != "port.added:5173 port.discovery:0 port.scanned:0 port.scanned:0 port.discovery:0" {
		t.Errorf("events = %s", got)
	}
	if got := strings.Join(discoveryVerdicts(t, d), " "); got != "port.discovery:unavailable port.scanned:unavailable port.scanned:unavailable port.discovery:ok" {
		t.Errorf("reports = %s; want unavailable said once unasked, both rescans unavailable — a raced read read nothing — and the recovery said", got)
	}
	if got := d.sc.Discovery("w1"); got != preview.DiscoveryOK {
		t.Errorf("after the recovery the port list would say %q", got)
	}
	if len(d.logs) != 2 || !strings.Contains(d.logs[0], "unavailable") || !strings.Contains(d.logs[1], "works again") {
		t.Errorf("journal = %q; want one line as it broke and one as it recovered", d.logs)
	}
}

// discoveryVerdicts is every port.scanned and port.discovery, kind:verdict.
func discoveryVerdicts(t *testing.T, d *discovery) []string {
	t.Helper()
	es, err := d.log.Since(context.Background(), 0)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range es {
		if e.Kind == preview.KindPortScanned || e.Kind == preview.KindPortDiscovery {
			var v struct{ Discovery, Source string }
			json.Unmarshal(e.Data, &v)
			if v.Source != preview.SourceDiscovery || e.Level != events.Info {
				t.Errorf("%s: source %q, level %q", e.Kind, v.Source, e.Level)
			}
			out = append(out, e.Kind+":"+v.Discovery)
		}
	}
	return out
}

// TestRescan: POST …/ports/rescan's half — a scan that begins after the call,
// answered by port.scanned for the workspace asked about and no other;
// refused for a workspace that does not exist or is being deleted, and once
// the scanner's group is stopping.
func TestRescan(t *testing.T) {
	d := newDiscovery(t)
	ctx := context.Background()
	g := life.NewGroup(ctx)
	if err := d.sc.Start(g); err != nil {
		t.Fatal(err)
	}
	if err := d.sc.Rescan(ctx, "w2"); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		es, _ := d.log.Since(ctx, 0)
		if len(es) > 0 {
			if len(es) != 1 || es[0].Kind != preview.KindPortScanned || es[0].WorkspaceID != "w2" {
				t.Fatalf("events = %+v; want one port.scanned for w2", es)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("no port.scanned for the rescan")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := d.sc.Rescan(ctx, "nope"); !errors.Is(err, preview.ErrNoWorkspace) {
		t.Errorf("unknown workspace: %v", err)
	}
	d.db.Exec(`UPDATE workspace SET state = 'deleting' WHERE id = 'w1'`)
	if err := d.sc.Rescan(ctx, "w1"); !errors.Is(err, preview.ErrWorkspaceDeleting) {
		t.Errorf("deleting workspace: %v", err)
	}
	g.Stop()
	wait, cancel := sys.NewTimer(sys.Production().Clock, 10*time.Second)
	defer cancel()
	if late := g.Wait(wait); len(late) > 0 {
		t.Fatalf("still running: %v", late)
	}
	if err := d.sc.Rescan(ctx, "w2"); !errors.Is(err, life.ErrStopping) {
		t.Errorf("after stop: %v; want life.ErrStopping", err)
	}
}

// TestDiscoveryEventsNotifyNoOne: every event discovery writes is a row's
// own kind (port.added, port.updated, port.retired), the rescan's answer, or
// the state report the panel badges (port.discovery — a state, shown where
// the operator looks, never a notice), at info level, carrying source "discovery" — nothing the UI turns into a
// notification, and nothing above info. The web spec
// (web/src/views/PortsDiscovery.spec.ts) plays the same events through the
// app and finds no alert, no announcement and no title change.
func TestDiscoveryEventsNotifyNoOne(t *testing.T) {
	d := newDiscovery(t)
	playGolden(t, d)
	es, _ := d.log.Since(context.Background(), 0)
	allowed := map[string]bool{"port.added": true, "port.updated": true, "port.retired": true, "port.scanned": true,
		"port.discovery": true}
	if len(es) < 2 || es[0].Kind != preview.KindPortAdded {
		t.Fatalf("events = %+v; want the declaration's port.added, then discovery's", es)
	}
	for _, e := range es[1:] { // after the declaration's, which is step 4's
		var v struct{ Source string }
		json.Unmarshal(e.Data, &v)
		if !allowed[e.Kind] || e.Level != events.Info || v.Source != preview.SourceDiscovery {
			t.Errorf("%s at %s, source %q", e.Kind, e.Level, v.Source)
		}
	}
}

var update = flag.Bool("update", false, "rewrite testdata/discovery-events.json")

// playGolden is the scenario the golden file records and the mock backend
// replays (web/src/mocks/discovery.spec.ts), one scan per interval: w1
// declares 5173; a server listens on it at 0.0.0.0 and another on
// 127.0.0.1:8080 (scans 1 and 2); a rescan (3); then both stop (4, 5 and 6,
// the grace ending at the third scan that misses them), and 8080, discovery's
// own, is retired RetireAfter after it was last seen (scan 123). It returns the
// newest event id after each scan, so the golden file says which scan wrote
// each event.
func playGolden(t *testing.T, d *discovery) []int64 {
	t.Helper()
	if err := d.svc.DeclarePorts(context.Background(), "w1", []preview.Declared{{Port: 5173, Label: "vite"}}); err != nil {
		t.Fatal(err)
	}
	var after []int64
	scan := func(asks ...string) {
		d.scan(asks...)
		n, err := d.log.Latest(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		after = append(after, n)
	}
	d.src.listen("w1", "0.0.0.0:5173", "127.0.0.1:8080")
	scan()
	scan()
	scan("w1")
	d.src.listen("w1")
	for i := 0; i < 3+int(preview.RetireAfter/preview.DefaultScanInterval); i++ {
		scan()
	}
	// Step 6: the table cannot be read for a while — said unasked, answered
	// to a rescan — and then can, which is said too.
	d.src.fail("w1", errors.New("permission denied"))
	scan()
	scan("w1")
	d.src.listen("w1")
	scan()
	return after
}

// goldenEvent is one event as the parity check compares it: what the
// reducer reads, without ids, slugs or times.
type goldenEvent struct {
	// Scan is which of playGolden's scans wrote it, from 1.
	Scan      int            `json:"scan"`
	Kind      string         `json:"kind"`
	Level     string         `json:"level"`
	DataKeys  []string       `json:"data_keys"`
	Port      map[string]any `json:"port,omitempty"`
	Source    string         `json:"source"`
	Discovery string         `json:"discovery,omitempty"`
	ContPort  int            `json:"container_port,omitempty"`
}

// TestDiscoveryEventsGolden pins the events of playGolden in
// testdata/discovery-events.json, which the mock backend's spec reads: the
// mock must write the same sequence for the same scenario, so a change here
// fails the web suite until the mock matches (-update rewrites it).
func TestDiscoveryEventsGolden(t *testing.T) {
	d := newDiscovery(t)
	after := playGolden(t, d)
	es, _ := d.log.Since(context.Background(), 0)
	var got []goldenEvent
	for _, e := range es[1:] { // after the declaration's port.added, which is step 4's
		var data map[string]any
		json.Unmarshal(e.Data, &data)
		g := goldenEvent{Kind: e.Kind, Level: string(e.Level)}
		for i, n := range after {
			if e.ID <= n {
				g.Scan = i + 1
				break
			}
		}
		for k := range data {
			g.DataKeys = append(g.DataKeys, k)
		}
		sort.Strings(g.DataKeys)
		g.Source, _ = data["source"].(string)
		g.Discovery, _ = data["discovery"].(string)
		if n, ok := data["container_port"].(float64); ok {
			g.ContPort = int(n)
		}
		if p, ok := data["port"].(map[string]any); ok {
			g.Port = map[string]any{}
			for _, k := range []string{"container_port", "enabled", "declared", "observed", "manual", "hidden",
				"observed_state", "bind_addr", "loopback", "label", "url"} {
				g.Port[k] = p[k]
			}
		}
		got = append(got, g)
	}
	path := filepath.Join("testdata", "discovery-events.json")
	b, _ := json.MarshalIndent(got, "", "  ")
	b = append(b, '\n')
	if *update {
		os.MkdirAll("testdata", 0o755)
		if err := os.WriteFile(path, b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v (run with -update to write it)", err)
	}
	var w []goldenEvent
	json.Unmarshal(want, &w)
	var g2 []goldenEvent
	json.Unmarshal(b, &g2)
	if !reflect.DeepEqual(g2, w) {
		t.Errorf("events differ from %s:\n%s", path, b)
	}
}
