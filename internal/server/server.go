// Package server assembles Drydock's front door: the store, the auth service,
// both muxes, and the two Unix sockets they are served on.
//
// There is no TCP listener anywhere in this package or anything it calls, and
// that is the design's first non-negotiable (§13.5): Caddy owns the only
// LAN-facing port, and Drydock is reachable only through a group-owned socket
// whose one other member is Caddy. A loopback port would be reachable by every
// local process and user; the socket is reachable by its group.
package server

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	"github.com/krelinga/drydock/internal/api"
	"github.com/krelinga/drydock/internal/auth"
	"github.com/krelinga/drydock/internal/config"
	"github.com/krelinga/drydock/internal/store"
	"github.com/krelinga/drydock/internal/sys"
	"github.com/krelinga/drydock/internal/web"
)

// Server is a running front door.
type Server struct {
	DB      *store.DB
	Auth    *auth.Service
	api     *http.Server
	preview *http.Server
	apiLn   net.Listener
	prevLn  net.Listener
}

// New opens the store (taking the single-instance lock), builds both muxes,
// and binds both sockets. It does not start serving; Serve does.
//
// The store is opened first on purpose: if another Drydock holds the lock,
// New fails before touching the socket paths, so it can never unlink a socket
// a running instance is serving on.
func New(ctx context.Context, cfg config.Config, env sys.Env) (*Server, error) {
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}
	db, err := store.Open(ctx, cfg.DatabasePath)
	if err != nil {
		return nil, err
	}
	svc := auth.New(db.DB, env)
	gate := api.SessionGate{Sessions: svc.Sessions, UIOrigin: cfg.UIOrigin, UIHost: cfg.UIHost}

	ui, err := web.New(web.Dist())
	if err != nil {
		db.Close()
		return nil, err
	}

	s := &Server{DB: db, Auth: svc}
	s.api = &http.Server{
		Handler:           apiSocketHandler(gate, api.Build(api.MuxAPI, gate, api.SessionRoutes{Auth: svc}.Handlers()), ui),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
		// No WriteTimeout: /api/events is a long-lived SSE stream.
	}
	s.preview = &http.Server{
		Handler:           api.Build(api.MuxPreview, gate, nil),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
	}

	gid, err := lookupGroup(cfg.SocketGroup)
	if err != nil {
		db.Close()
		return nil, err
	}
	if s.apiLn, err = listenUnix(cfg.APISocket, gid); err != nil {
		db.Close()
		return nil, err
	}
	if s.prevLn, err = listenUnix(cfg.PreviewSocket, gid); err != nil {
		s.apiLn.Close()
		db.Close()
		return nil, err
	}
	return s, nil
}

// apiSocketHandler is everything the API socket serves, in one place:
//
//   - /api and /api/... go to the route table's mux — or, for a path the table
//     does not declare, to unrouted, which gates it exactly as a real route
//     would. Never to the app.
//   - /preview/authorize is a server handler on the API mux (port forwarding
//     §6), not a client route; it must never reach the SPA fallback.
//   - Everything else is the app, behind the same exact-Host check as the
//     gate. The app is served without a session — the sign-in page has to
//     load for someone signed out, and the bundle contains no data — but a
//     rebound hostname gets nothing, not even the bundle.
//
// Frontend §8's security headers wrap all of it, so the API's JSON carries
// the same CSP, Referrer-Policy and nosniff as the app's HTML.
func apiSocketHandler(gate api.Gate, apiMux *http.ServeMux, ui http.Handler) http.Handler {
	apiH := unrouted(gate, apiMux)
	root := http.NewServeMux()
	root.Handle("/api", apiH)
	root.Handle("/api/", apiH)
	root.Handle("/preview/authorize", apiH)
	root.Handle("/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !gate.HostAllowed(r) {
			api.WriteError(w, http.StatusForbidden, api.CodeForbiddenHost,
				"This request was not addressed to Drydock.", "")
			return
		}
		ui.ServeHTTP(w, r)
	}))
	return web.SecurityHeaders(root)
}

// unrouted sends a request the route table declares to apiMux, and gates one
// it does not.
//
// Left to net/http, an unknown /api path gets a plain-text 404 straight from
// the router, before any auth check: an unauthenticated caller could tell a
// declared route (401) from an undeclared one (404) and read the API surface
// off a server it cannot use — the same oracle mux.go's ordering closes for
// declared-but-unbuilt routes. So an undeclared path goes through the gate's
// own order: Host, then session, and only then a JSON 404 (or 405 when the
// path exists under another method). Never HTML, never the SPA.
func unrouted(gate api.Gate, apiMux *http.ServeMux) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h, pattern := apiMux.Handler(r)
		if pattern != "" {
			apiMux.ServeHTTP(w, r)
			return
		}
		if !gate.HostAllowed(r) {
			api.WriteError(w, http.StatusForbidden, api.CodeForbiddenHost,
				"This request was not addressed to Drydock.", "")
			return
		}
		if _, ok := gate.Authenticate(r); !ok {
			api.WriteError(w, http.StatusUnauthorized, api.CodeUnauthenticated, "Sign in to continue.", "")
			return
		}
		// The mux's fallback for an unmatched request is either NotFound or,
		// when the path is declared under other methods, a 405 that sets
		// Allow. Run it into a probe to learn which, then answer in the
		// envelope.
		var probe statusProbe
		h.ServeHTTP(&probe, r)
		if probe.status == http.StatusMethodNotAllowed {
			w.Header().Set("Allow", probe.header.Get("Allow"))
			api.WriteError(w, http.StatusMethodNotAllowed, api.CodeBadRequest,
				"This API route does not accept that method.", "")
			return
		}
		api.WriteError(w, http.StatusNotFound, api.CodeNotFound, "No such API route.", "")
	})
}

// statusProbe is a ResponseWriter that records the status and headers and
// discards the body.
type statusProbe struct {
	header http.Header
	status int
}

func (p *statusProbe) Header() http.Header {
	if p.header == nil {
		p.header = http.Header{}
	}
	return p.header
}
func (p *statusProbe) Write(b []byte) (int, error) {
	if p.status == 0 {
		p.status = http.StatusOK
	}
	return len(b), nil
}
func (p *statusProbe) WriteHeader(code int) {
	if p.status == 0 {
		p.status = code
	}
}

// Serve runs both muxes until ctx is cancelled, then shuts down gracefully.
func (s *Server) Serve(ctx context.Context) error {
	errc := make(chan error, 2)
	go func() { errc <- s.api.Serve(s.apiLn) }()
	go func() { errc <- s.preview.Serve(s.prevLn) }()

	var serveErr error
	select {
	case <-ctx.Done():
	case serveErr = <-errc:
	}
	shutCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = s.api.Shutdown(shutCtx)
	_ = s.preview.Shutdown(shutCtx)
	s.DB.Close()
	if errors.Is(serveErr, http.ErrServerClosed) {
		serveErr = nil
	}
	return serveErr
}

// listenUnix binds a socket readable and writable by its owner and group only.
//
// Two details that each close a real gap:
//   - The umask is narrowed around the bind, because between Listen creating
//     the socket and the chmod below, it would otherwise exist with
//     umask-derived permissions — world-connectable under a permissive umask.
//     With 0177 it is owner-only for that window, then 0660.
//   - A leftover file at the path is removed only if it is a socket. A
//     regular file there is a misconfiguration, not stale state, and deleting
//     it would be destroying something Drydock did not create.
func listenUnix(path string, gid int) (net.Listener, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return nil, fmt.Errorf("socket dir: %w", err)
	}
	if fi, err := os.Lstat(path); err == nil {
		if fi.Mode()&fs.ModeSocket == 0 {
			return nil, fmt.Errorf("%s exists and is not a socket; refusing to remove it", path)
		}
		if err := os.Remove(path); err != nil {
			return nil, fmt.Errorf("remove stale socket %s: %w", path, err)
		}
	}
	old := syscall.Umask(0o177)
	ln, err := net.Listen("unix", path)
	syscall.Umask(old)
	if err != nil {
		return nil, fmt.Errorf("listen %s: %w", path, err)
	}
	if err := os.Chown(path, -1, gid); err != nil {
		ln.Close()
		return nil, fmt.Errorf("chown %s to group %d: %w", path, gid, err)
	}
	if err := os.Chmod(path, 0o660); err != nil {
		ln.Close()
		return nil, fmt.Errorf("chmod %s: %w", path, err)
	}
	return ln, nil
}

// lookupGroup resolves the socket group. It is required: a socket left owned
// by whatever group the process happens to run as is a socket nobody decided
// the access of.
func lookupGroup(name string) (int, error) {
	if name == "" {
		return 0, errors.New("socket group is required: the group's members are exactly who can reach Drydock")
	}
	g, err := user.LookupGroup(name)
	if err != nil {
		return 0, fmt.Errorf("socket group %q: %w", name, err)
	}
	return strconv.Atoi(g.Gid)
}
