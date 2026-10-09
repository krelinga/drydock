package preview_test

import (
	"context"
	"errors"
	"fmt"
	"html"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/krelinga/drydock/internal/preview"
)

// PF §11 and §13 step 6, the diagnosis: each condition the table lists, said
// from what discovery already recorded, and the one that needs no dial never
// dialled.

// countingDial is a real dial that counts its calls.
func countingDial(n *atomic.Int32) func(ctx context.Context, network, addr string) (net.Conn, error) {
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		n.Add(1)
		return (&net.Dialer{}).DialContext(ctx, network, addr)
	}
}

// TestALoopbackPortIsNeverDialled is §13 step 6's done-when: a dev server
// bound to 127.0.0.1 is answered with the sentence that says to use
// --host 0.0.0.0 — on the preview's page and by the probe — and no
// resolution and no dial is ever attempted for it, a websocket upgrade
// included. The app is really listening at the address the resolver names,
// so a proxy that dialled would reach it: the counts, not a refusal, are what
// hold. The control is the same port seen on 0.0.0.0, which is resolved,
// dialled and answers; and a loopback bind discovery has since seen go is
// stale, so it is dialled too. Mutation-checked: removing the check in
// ServePreview, or in Probe, dials.
func TestALoopbackPortIsNeverDialled(t *testing.T) {
	up, got := recordingUpstream(t)
	tg := proxyTarget(t, up.URL)
	for _, bind := range []string{"127.0.0.1", "127.0.0.53", "::1", "::ffff:127.0.0.1"} {
		var dials atomic.Int32
		res := &fakeResolver{ips: []string{"127.0.0.1"}}
		p := &preview.Proxy{Resolver: res, Dial: countingDial(&dials)}
		lt := tg
		lt.Seen = preview.Observation{State: preview.StateListening, Bind: bind}
		srv := front(t, p, lt)
		resp, body := get(t, srv.URL+"/")
		want := preview.LoopbackSentence(bind, tg.ContainerPort)
		if resp.StatusCode != http.StatusBadGateway || !strings.Contains(body, html.EscapeString(want)) ||
			!strings.Contains(want, "--host 0.0.0.0") || !strings.Contains(want, "only reachable from inside the container") {
			t.Errorf("%s: %d %q; want 502 saying %q", bind, resp.StatusCode, body, want)
		}
		if resp.Header.Get("Cache-Control") != "no-store" || resp.Header.Get("Referrer-Policy") != "no-referrer" {
			t.Errorf("%s: the page's headers = %v", bind, resp.Header)
		}
		// An HMR socket asks the same way, and is not dialled either.
		ws, _ := get(t, srv.URL+"/hmr", "Connection", "Upgrade", "Upgrade", "websocket",
			"Sec-WebSocket-Version", "13", "Sec-WebSocket-Key", "dGhlIHNhbXBsZSBub25jZQ==")
		if ws.StatusCode != http.StatusBadGateway {
			t.Errorf("%s: the upgrade = %d", bind, ws.StatusCode)
		}
		pr := p.Probe(context.Background(), tg.WorkspaceID, tg.ContainerPort, lt.Seen)
		if pr.Outcome != preview.ProbeLoopback || pr.Message != want {
			t.Errorf("%s: probe = %+v; want loopback, %q", bind, pr, want)
		}
		if r, c := res.counts(); r != 0 || c != 0 || dials.Load() != 0 || got.n() != 0 || p.Skipped() != 3 {
			t.Errorf("%s: %d resolutions, %d confirmations, %d dials, %d requests reached the app, %d skipped; want nothing but 3 skipped",
				bind, r, c, dials.Load(), got.n(), p.Skipped())
		}
	}

	// Controls: discovery saw it on 0.0.0.0, or saw a loopback bind go.
	for _, seen := range []preview.Observation{
		{State: preview.StateListening, Bind: "0.0.0.0"},
		{State: preview.StateListening, Bind: "::"},
		{State: preview.StateGone, Bind: "127.0.0.1"},
		{},
	} {
		var dials atomic.Int32
		res := &fakeResolver{ips: []string{"127.0.0.1"}}
		p := &preview.Proxy{Resolver: res, Dial: countingDial(&dials)}
		ct := tg
		ct.Seen = seen
		before := got.n()
		resp, body := get(t, front(t, p, ct).URL+"/")
		if resp.StatusCode != http.StatusOK || body != "app" || got.n() != before+1 || dials.Load() == 0 {
			t.Errorf("control %+v: %d %q, %d dials", seen, resp.StatusCode, body, dials.Load())
		}
		if pr := p.Probe(context.Background(), ct.WorkspaceID, ct.ContainerPort, seen); pr.Outcome != preview.ProbeAnswering {
			t.Errorf("control %+v: probe = %+v", seen, pr)
		}
	}
}

// TestLoopbackSentence names where the server listens, as an address and a
// port a reader can match against the dev server's own banner.
func TestLoopbackSentence(t *testing.T) {
	for bind, where := range map[string]string{
		"127.0.0.1":        "127.0.0.1:5173",
		"::1":              "[::1]:5173",
		"::ffff:127.0.0.1": "127.0.0.1:5173",
		"":                 "5173",
	} {
		got := preview.LoopbackSentence(bind, 5173)
		want := "Listening on " + where + ", which is only reachable from inside the container. Start it with --host 0.0.0.0."
		if got != want {
			t.Errorf("%q: %q; want %q", bind, got, want)
		}
	}
	for _, o := range []preview.Observation{
		{State: preview.StateListening, Bind: "0.0.0.0"},
		{State: preview.StateGone, Bind: "127.0.0.1"},
		{State: preview.StateListening, Bind: "garbage"},
		{},
	} {
		if o.LoopbackOnly() {
			t.Errorf("%+v is loopback-only", o)
		}
	}
}

// TestNothingListeningIsSaid is §11's *port enabled, nothing listening*: a
// port discovery saw stop listening is still dialled — the dial is the truth,
// and a server started a moment ago answers before discovery has seen it
// twice — and a refusal there says nothing is listening rather than guessing
// at a loopback bind. A port never seen keeps the generic refusal (the
// control), and a timeout stays a timeout.
func TestNothingListeningIsSaid(t *testing.T) {
	closed, _ := net.Listen("tcp", "127.0.0.1:0")
	cport := closed.Addr().(*net.TCPAddr).Port
	closed.Close()
	tg := proxyTarget(t, "http://x:"+strconv.Itoa(cport))
	gone := preview.Observation{State: preview.StateGone, Bind: "0.0.0.0"}

	var dials atomic.Int32
	p := &preview.Proxy{Resolver: &fakeResolver{ips: []string{"127.0.0.1"}}, Dial: countingDial(&dials)}
	gt := tg
	gt.Seen = gone
	resp, body := get(t, front(t, p, gt).URL+"/")
	want := preview.OutcomeSentence(preview.ProbeNotListening, cport)
	if resp.StatusCode != http.StatusBadGateway || !strings.Contains(body, html.EscapeString(want)) ||
		!strings.Contains(want, "Nothing is listening on port "+strconv.Itoa(cport)) || dials.Load() == 0 {
		t.Errorf("gone: %d %q after %d dials; want 502 saying %q", resp.StatusCode, body, dials.Load(), want)
	}
	if pr := p.Probe(context.Background(), tg.WorkspaceID, cport, gone); pr.Outcome != preview.ProbeNotListening || pr.Message != want {
		t.Errorf("gone: probe = %+v", pr)
	}
	// Control: never seen, the generic refusal.
	resp, body = get(t, front(t, &preview.Proxy{Resolver: &fakeResolver{ips: []string{"127.0.0.1"}}}, tg).URL+"/")
	if resp.StatusCode != http.StatusBadGateway || !strings.Contains(body, html.EscapeString(preview.OutcomeSentence(preview.ProbeRefused, cport))) {
		t.Errorf("never seen: %d %q", resp.StatusCode, body)
	}
	// A timeout is not refined: something may be there, answering slowly.
	blocking := func(ctx context.Context, _, _ string) (net.Conn, error) { <-ctx.Done(); return nil, ctx.Err() }
	timed := (&preview.Proxy{Resolver: &fakeResolver{ips: []string{"127.0.0.1"}}, Dial: blocking, DialTimeout: 50 * time.Millisecond}).
		Probe(context.Background(), "ws", cport, gone)
	if timed.Outcome != preview.ProbeTimedOut {
		t.Errorf("gone, timed out: %+v", timed)
	}
}

// TestEnablingALoopbackPortIsRefused: the decision (§13.7) — an enable of a
// port listening on loopback only is ErrLoopbackOnly, nothing written and no
// event; the control on 0.0.0.0 enables. A stale loopback bind (gone) does
// not refuse. One already enabled when its server moved to loopback stays
// enabled — discovery has no path to the switch either way — and the target
// the proxy reads for it carries the observation, so its preview is answered
// with the sentence and never dialled: discovery, registry and proxy end to
// end.
func TestEnablingALoopbackPortIsRefused(t *testing.T) {
	d := newDiscovery(t)
	ctx := context.Background()
	d.src.listen("w1", "127.0.0.1:5173", "0.0.0.0:8080")
	d.scan()
	d.scan()
	rows := d.rows(t, "w1")
	loop, open := rows[5173], rows[8080]
	if !loop.Loopback || state(loop) != preview.StateListening || open.Loopback {
		t.Fatalf("rows = %+v", rows)
	}
	before := len(d.kinds(t))
	if _, err := d.svc.SetEnabled(ctx, "w1", loop.ID, true); !errors.Is(err, preview.ErrLoopbackOnly) {
		t.Errorf("enabling a loopback port = %v; want ErrLoopbackOnly", err)
	}
	if p := d.rows(t, "w1")[5173]; p.Enabled || len(d.kinds(t)) != before {
		t.Errorf("a refused enable wrote: %+v, %v", p, d.kinds(t)[before:])
	}
	if _, err := d.svc.SetEnabled(ctx, "w1", open.ID, true); err != nil {
		t.Errorf("control: enabling a port on 0.0.0.0 = %v", err)
	}

	// The 0.0.0.0 server restarts on loopback: still enabled, now said.
	d.src.listen("w1", "127.0.0.1:5173", "127.0.0.1:8080")
	d.scan()
	d.scan()
	p := d.rows(t, "w1")[8080]
	if !p.Enabled || !p.Loopback {
		t.Fatalf("after the move: %+v", p)
	}
	tg, err := d.svc.Resolve(ctx, p.Slug)
	if err != nil || !tg.Seen.LoopbackOnly() {
		t.Fatalf("Resolve = %+v, %v; want the loopback observation", tg, err)
	}
	var dials atomic.Int32
	res := &fakeResolver{ips: []string{"127.0.0.1"}}
	px := &preview.Proxy{Resolver: res, Dial: countingDial(&dials)}
	resp, body := get(t, front(t, px, tg).URL+"/")
	if r, _ := res.counts(); resp.StatusCode != http.StatusBadGateway || !strings.Contains(body, "--host 0.0.0.0") || r != 0 || dials.Load() != 0 {
		t.Errorf("an enabled port gone loopback: %d %q, %d resolutions, %d dials", resp.StatusCode, body, r, dials.Load())
	}

	// Stale: the loopback server stopped. Its bind is history, not a refusal.
	d.src.listen("w1")
	d.clock.Advance(preview.DefaultGrace)
	d.scan()
	if p := d.rows(t, "w1")[5173]; state(p) != preview.StateGone {
		t.Fatalf("not gone: %+v", p)
	} else if _, err := d.svc.SetEnabled(ctx, "w1", p.ID, true); err != nil {
		t.Errorf("enabling a port whose loopback server has gone = %v", err)
	}
}

// TestABudgetHoldIsSaidAndAnswered is #120's round-2 note: while a budget
// holds a change back the panel must say so, and *Look for listening ports
// now* must not answer "ok". Twelve test servers on ephemeral ports spend the
// mints ephemeral ports may — eight, MintReserve kept back — and the rest are
// held: the round reports "limited" unasked (port.discovery), the port list
// would say so, and a rescan is answered "limited". A dev server on 5173
// started after them is listed at once from the reserve. With the reserve
// spent too, a held port is delayed, not lost: it is listed once MintEvery
// refills a token. When the test servers stop, the next round says "ok".
// Mutation-checked: the verdict ignoring what budget held (the rescan says
// ok), and no reserve (5173 waits).
func TestABudgetHoldIsSaidAndAnswered(t *testing.T) {
	d := newDiscovery(t)
	var tests []string
	for i := 0; i < 12; i++ {
		tests = append(tests, fmt.Sprintf("0.0.0.0:%d", 40000+i))
	}
	d.src.listen("w1", tests...)
	d.scan()
	d.scan()
	if n := len(d.rows(t, "w1")); n != preview.MintBurst-preview.MintReserve {
		t.Errorf("%d ephemeral ports listed; want %d", n, preview.MintBurst-preview.MintReserve)
	}
	if got := d.sc.Discovery("w1"); got != preview.DiscoveryLimited {
		t.Errorf("unasked, the port list would say %q; want limited", got)
	}
	d.scan("w1")
	if v := discoveryVerdicts(t, d); strings.Join(v, " ") != "port.discovery:limited port.scanned:limited" {
		t.Errorf("reports = %v", v)
	}

	// The dev server, started after: listed from the reserve, at once.
	d.src.listen("w1", append([]string{"0.0.0.0:5173"}, tests...)...)
	d.scan()
	d.scan()
	if p, ok := d.rows(t, "w1")[5173]; !ok || p.Enabled {
		t.Errorf("the dev server after a test run: %+v, listed %v; want listed, off", p, ok)
	}

	// The reserve spent as well: the fourth of four more low ports waits.
	low := []string{"0.0.0.0:3001", "0.0.0.0:3002", "0.0.0.0:3003", "0.0.0.0:3004"}
	d.src.listen("w1", append(append([]string{"0.0.0.0:5173"}, low...), tests...)...)
	d.scan()
	d.scan("w1")
	if _, ok := d.rows(t, "w1")[3004]; ok {
		t.Fatal("3004 listed past the budget")
	}
	if v := discoveryVerdicts(t, d); v[len(v)-1] != "port.scanned:limited" {
		t.Errorf("a rescan with 3004 held back answered %v; want limited", v[len(v)-1])
	}
	d.clock.Advance(preview.MintEvery)
	d.scan()
	if _, ok := d.rows(t, "w1")[3004]; !ok {
		t.Error("3004 was held back and never listed: a budget delays, it must not forget")
	}

	// The test servers stop: nothing held, and that is said.
	d.src.listen("w1", append([]string{"0.0.0.0:5173"}, low...)...)
	d.scan()
	v := discoveryVerdicts(t, d)
	if v[len(v)-1] != "port.discovery:ok" || d.sc.Discovery("w1") != preview.DiscoveryOK {
		t.Errorf("after the hold: %v, %q; want ok said", v, d.sc.Discovery("w1"))
	}
	d.scan("w1")
	if v := discoveryVerdicts(t, d); v[len(v)-1] != "port.scanned:ok" {
		t.Errorf("control: a rescan with nothing held = %v", v[len(v)-1])
	}
}

// TestDiscoveryStateReportsAreBudgeted: a Docker that fails every other round
// for an hour reports its state at most StatusBurst times plus one per
// StatusEvery, and what the port list says is what the last report said.
// Mutation-checked: a report on every change of state writes one per round.
func TestDiscoveryStateReportsAreBudgeted(t *testing.T) {
	d := newDiscovery(t)
	d.src.listen("w1", "0.0.0.0:5173")
	rounds := int(time.Hour / preview.DefaultScanInterval)
	for i := 0; i < rounds; i++ {
		if i%2 == 0 {
			d.src.fail("w1", errors.New("docker: daemon not answering"))
		} else {
			d.src.listen("w1", "0.0.0.0:5173")
		}
		d.scan()
	}
	v := discoveryVerdicts(t, d)
	max := preview.StatusBurst + int(time.Hour/preview.StatusEvery) + 1
	if len(v) < 2 || len(v) > max {
		t.Errorf("%d reports in an hour of flapping; want some, at most %d", len(v), max)
	}
	last := strings.TrimPrefix(v[len(v)-1], "port.discovery:")
	if got := d.sc.Discovery("w1"); got != last {
		t.Errorf("the port list would say %q; the last report said %q", got, last)
	}
}
