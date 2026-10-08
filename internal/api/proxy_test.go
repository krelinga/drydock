package api

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/krelinga/drydock/internal/preview"
)

// --- the proxy behind the fallback (§13.1's third trap, closed) -----------

// targetGate is a stub gate whose preview session carries a Target, as
// SessionGate's does.
type targetGate struct {
	stubGate
	target preview.Target
}

func (g targetGate) PreviewSession(r *http.Request) (*http.Request, bool) {
	if !g.previewSession {
		return r, false
	}
	return r.WithContext(preview.WithTarget(r.Context(), g.target)), true
}

// countingResolver is Docker, answering one address and counting the asks.
type countingResolver struct {
	mu sync.Mutex
	n  int
}

func (c *countingResolver) Resolve(context.Context, string) (preview.Endpoint, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.n++
	return preview.Endpoint{ContainerID: "c", IP: netip.MustParseAddr("127.0.0.1")}, nil
}

func (c *countingResolver) Confirm(context.Context, string, preview.Endpoint) error { return nil }

func (c *countingResolver) asked() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.n
}

// TestTheProxyIsReachedOnlyThroughEveryGate is the meta-test the route table
// cannot give the real proxy, which lives in the fallback: with preview.Proxy
// behind the front door — not a stub — and a resolver that counts, the proxy
// asks Docker, and the app sees a request, only when the Host is a preview
// host, the path is not Drydock's, and the preview cookie validates. Every
// route of both muxes, under the method it is declared with, is tried three
// ways:
//
//   - every gate open but the preview session (a signed-in UI session, the
//     right Origin, a valid token): nothing reaches the proxy — no
//     resolution, no dial, no request at the app;
//   - every gate open but the preview host: the same;
//   - every gate open: an API route's path reaches the *app* — it is the
//     app's path on a preview host — and no API handler; a /.drydock/ path
//     reaches Drydock's own handler or its dead end, and still never the
//     proxy.
//
// The control is the app's own path, which reaches it through all three.
func TestTheProxyIsReachedOnlyThroughEveryGate(t *testing.T) {
	var appMu sync.Mutex
	var appSaw []string
	app := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		appMu.Lock()
		appSaw = append(appSaw, r.Method+" "+r.URL.Path)
		appMu.Unlock()
		w.Write([]byte("app"))
	}))
	defer app.Close()
	u, _ := url.Parse(app.URL)
	port, _ := strconv.Atoi(u.Port())
	target := preview.Target{PortID: "p", WorkspaceID: "w", ContainerPort: port, Slug: "myapp-5173-p2mq",
		Host: testPreviewHost, UpstreamScheme: "http", HostHeader: preview.HostLocalhost}
	res := &countingResolver{}
	proxy := &preview.Proxy{Resolver: res}
	defer proxy.Close()

	hits := map[string]bool{}
	handlers := map[string]http.HandlerFunc{}
	for _, rt := range Table {
		name := rt.Name
		handlers[name] = func(w http.ResponseWriter, _ *http.Request) {
			hits[name] = true
			fmt.Fprintf(w, "handler %s", name)
		}
	}
	noSession := targetGate{stubGate{session: true, origin: true, host: true, token: true, previewHost: true}, target}
	noPreviewHost := targetGate{stubGate{session: true, origin: true, host: true, token: true, previewSession: true}, target}
	open := targetGate{allOpen, target}
	appCount := func() int { appMu.Lock(); defer appMu.Unlock(); return len(appSaw) }

	for _, rt := range Table {
		r := requestFor(rt)
		r.Host = testPreviewHost
		for name, g := range map[string]targetGate{"no preview session": noSession, "not a preview host": noPreviewHost} {
			before, apps := res.asked(), appCount()
			rec := httptest.NewRecorder()
			PreviewFrontDoor(g, handlers, proxy).ServeHTTP(rec, r.Clone(r.Context()))
			if res.asked() != before || appCount() != apps {
				t.Errorf("%s %s, %s, reached the proxy (%d %q)", rt.Method, rt.Pattern, name, rec.Code, rec.Body)
			}
		}
		before, apps := res.asked(), appCount()
		rec := httptest.NewRecorder()
		PreviewFrontDoor(open, handlers, proxy).ServeHTTP(rec, r.Clone(r.Context()))
		reserved := strings.HasPrefix(r.URL.Path, preview.ReservedPrefix)
		switch {
		case reserved && (res.asked() != before || appCount() != apps):
			t.Errorf("%s %s, Drydock's own path, reached the proxy", rt.Method, rt.Pattern)
		case !reserved && (rec.Body.String() != "app" || appCount() != apps+1):
			t.Errorf("%s %s on a preview host, signed in to the preview = %d %q; want the app's answer", rt.Method, rt.Pattern, rec.Code, rec.Body)
		}
	}
	for name := range hits {
		if name != "preview.session" && name != "preview.denied" {
			t.Errorf("the API handler %s ran on the preview socket", name)
		}
	}
	// Control: the app's own path, through all three gates.
	before := appCount()
	rec := httptest.NewRecorder()
	PreviewFrontDoor(open, handlers, proxy).ServeHTTP(rec, httptest.NewRequest("GET", "https://"+testPreviewHost+"/src/main.ts", nil))
	if rec.Body.String() != "app" || appCount() != before+1 {
		t.Errorf("control: the app's path = %d %q", rec.Code, rec.Body)
	}
}

// TestReservedPathIsReadAsAnAppWould: /.drydock/ is Drydock's on a preview
// host however a path spells it, as long as an app's router could read it as
// that — doubled slashes, dot segments, a ;parameter, another case — so none
// reaches the proxy even signed in. The controls, a name that only begins
// with .drydock and one deeper in the path, are the app's.
func TestReservedPathIsReadAsAnAppWould(t *testing.T) {
	app := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("app")) }))
	defer app.Close()
	u, _ := url.Parse(app.URL)
	port, _ := strconv.Atoi(u.Port())
	target := preview.Target{PortID: "p", WorkspaceID: "w", ContainerPort: port, Host: testPreviewHost, UpstreamScheme: "http"}
	proxy := &preview.Proxy{Resolver: &countingResolver{}}
	defer proxy.Close()
	front := PreviewFrontDoor(targetGate{allOpen, target}, nil, proxy)
	for p, reserved := range map[string]bool{
		"//.drydock/x":       true,
		"/./.drydock/x":      true,
		"/a/../.drydock/x":   true,
		"/.drydock;/x":       true,
		"/.drydock;v=1":      true,
		"/.DRYDOCK/x":        true,
		"/.drydock":          true,
		"/.drydockfoo/x":     false,
		"/app/.drydock/x":    false,
		"/a/b/../.drydock-x": false,
	} {
		r := httptest.NewRequest("GET", "https://"+testPreviewHost+"/", nil)
		r.URL.Path, r.URL.RawPath = p, ""
		rec := httptest.NewRecorder()
		front.ServeHTTP(rec, r)
		denied := rec.Code == http.StatusFound && rec.Header().Get("Location") == preview.DeniedPath
		if reserved && !denied {
			t.Errorf("%s reached the app (%d %q); it is Drydock's", p, rec.Code, rec.Body)
		}
		if !reserved && rec.Body.String() != "app" {
			t.Errorf("control: %s = %d %q; want the app's", p, rec.Code, rec.Body)
		}
	}
}
