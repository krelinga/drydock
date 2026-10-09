package preview_test

import (
	"context"
	"errors"
	"fmt"
	"html"
	"net"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/krelinga/drydock/internal/preview"
)

// TestProbeIsTheProxysDial is PF §13.4's note to step 4: the probe is the
// proxy's own dial — Resolve, the connect, Confirm, the one retry — and gives
// its answers. Each case checks the resolver was asked as a request's dial
// asks it, and that the dial went to the address it named; the failure cases
// say what the proxy's own page says for the same port.
func TestProbeIsTheProxysDial(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()
	port := ln.Addr().(*net.TCPAddr).Port
	var mu sync.Mutex
	var dialled []string
	dial := func(ctx context.Context, n, a string) (net.Conn, error) {
		mu.Lock()
		dialled = append(dialled, a)
		mu.Unlock()
		return (&net.Dialer{}).DialContext(ctx, n, a)
	}
	last := func() string {
		mu.Lock()
		defer mu.Unlock()
		if len(dialled) == 0 {
			return ""
		}
		return dialled[len(dialled)-1]
	}

	// Answering: one resolution, one confirmation, the resolved address.
	res := &fakeResolver{ips: []string{"127.0.0.1"}}
	got := (&preview.Proxy{Resolver: res, Dial: dial}).Probe(context.Background(), "ws", port)
	if r, c := res.counts(); got.Outcome != preview.ProbeAnswering || r != 1 || c != 1 || last() != "127.0.0.1:"+strconv.Itoa(port) {
		t.Errorf("answering: %+v, %d resolutions, %d confirmations, dialled %q", got, r, c, last())
	}

	// Moved between the connect and the confirmation: not running, as the
	// proxy's denied page treats it.
	got = (&preview.Proxy{Resolver: &fakeResolver{ips: []string{"127.0.0.1"}, confirmErr: preview.ErrNotRunning}, Dial: dial}).Probe(context.Background(), "ws", port)
	if got.Outcome != preview.ProbeNotRunning {
		t.Errorf("moved after the connect: %+v", got)
	}

	// No running container: not running, and no dial at all.
	mu.Lock()
	n := len(dialled)
	mu.Unlock()
	got = (&preview.Proxy{Resolver: &fakeResolver{err: fmt.Errorf("%w: none", preview.ErrNotRunning)}, Dial: dial}).Probe(context.Background(), "ws", port)
	mu.Lock()
	extra := len(dialled) - n
	mu.Unlock()
	if got.Outcome != preview.ProbeNotRunning || extra != 0 {
		t.Errorf("no container: %+v after %d dials", got, extra)
	}

	// A restarted container: the first address refuses, the next resolution
	// names another, and that one answers — the proxy's one retry.
	res = &fakeResolver{ips: []string{"127.0.0.3", "127.0.0.1"}}
	got = (&preview.Proxy{Resolver: res, Dial: dial}).Probe(context.Background(), "ws", port)
	if r, _ := res.counts(); got.Outcome != preview.ProbeAnswering || r != 2 {
		t.Errorf("restarted: %+v after %d resolutions", got, r)
	}

	// Refused, timed out and Docker unreachable: the proxy's sentences.
	closed, _ := net.Listen("tcp", "127.0.0.1:0")
	cport := closed.Addr().(*net.TCPAddr).Port
	closed.Close()
	refused := (&preview.Proxy{Resolver: &fakeResolver{ips: []string{"127.0.0.1"}}}).Probe(context.Background(), "ws", cport)
	tg := proxyTarget(t, "http://x:"+strconv.Itoa(cport))
	_, page := get(t, front(t, &preview.Proxy{Resolver: &fakeResolver{ips: []string{"127.0.0.1"}}}, tg).URL+"/")
	if refused.Outcome != preview.ProbeRefused || !strings.Contains(page, html.EscapeString(refused.Message)) {
		t.Errorf("refused: %+v; the proxy's page says %q", refused, page)
	}
	blocking := func(ctx context.Context, _, _ string) (net.Conn, error) { <-ctx.Done(); return nil, ctx.Err() }
	timed := (&preview.Proxy{Resolver: &fakeResolver{ips: []string{"127.0.0.1"}}, Dial: blocking, DialTimeout: 50 * time.Millisecond}).Probe(context.Background(), "ws", port)
	if timed.Outcome != preview.ProbeTimedOut || !strings.Contains(timed.Message, "in time") {
		t.Errorf("timed out: %+v", timed)
	}
	var logged atomic.Int32
	failed := (&preview.Proxy{Resolver: &fakeResolver{err: errors.New("docker: daemon down")},
		Logf: func(string, ...any) { logged.Add(1) }}).Probe(context.Background(), "ws", port)
	if failed.Outcome != preview.ProbeLookupFailed || strings.Contains(failed.Message, "daemon") || logged.Load() != 1 {
		t.Errorf("lookup failed: %+v, logged %d", failed, logged.Load())
	}
}
