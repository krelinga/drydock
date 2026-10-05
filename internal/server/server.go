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
	"github.com/krelinga/drydock/internal/broker"
	"github.com/krelinga/drydock/internal/catalog"
	"github.com/krelinga/drydock/internal/clone"
	"github.com/krelinga/drydock/internal/config"
	"github.com/krelinga/drydock/internal/container"
	"github.com/krelinga/drydock/internal/events"
	"github.com/krelinga/drydock/internal/github"
	"github.com/krelinga/drydock/internal/provision"
	"github.com/krelinga/drydock/internal/reconcile"
	"github.com/krelinga/drydock/internal/secrets"
	"github.com/krelinga/drydock/internal/store"
	"github.com/krelinga/drydock/internal/subproc"
	"github.com/krelinga/drydock/internal/sys"
	"github.com/krelinga/drydock/internal/web"
	"github.com/krelinga/drydock/internal/workspace"
)

// Server is a running front door.
type Server struct {
	DB     *store.DB
	Auth   *auth.Service
	Events *events.Log
	// Workspaces and Reconciler are Phase 2's skeleton: the state machine
	// and boot reconciliation (§6). No route drives them yet.
	Workspaces *workspace.Store
	Reconciler *reconcile.Reconciler
	// Catalog and Broker are nil when no GitHub App is configured.
	Catalog *catalog.Catalog
	Broker  *broker.Broker
	// Secrets is nil when no master key is configured. With one, the broker
	// (when there is an App) answers GET-SECRETS from it.
	Secrets *secrets.Store
	// Provisioner runs §6's eight steps behind POST /api/workspaces and
	// /start. Always present; without an App it refuses with
	// app_not_configured. Its exported fields are a test's seam — a fake
	// git remote, a remote env, a minimal config with --network=host — set
	// between New and Serve.
	Provisioner *provision.Provisioner
	// reconciled closes when boot reconciliation has finished, so a test
	// can set up workspace rows reconciliation would otherwise move.
	reconciled chan struct{}
	api        *http.Server
	preview    *http.Server
	apiLn      net.Listener
	prevLn     net.Listener
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
	// Before anything can touch a container: whose containers these are is
	// decided by the prefix the database was created with (§6).
	if err := db.ClaimLabelPrefix(ctx, cfg.LabelPrefix, env.Clock.Now()); err != nil {
		db.Close()
		return nil, err
	}
	svc := auth.New(db.DB, env)
	gate := api.SessionGate{Sessions: svc.Sessions, UIOrigin: cfg.UIOrigin, UIHost: cfg.UIHost}

	ui, err := web.New(web.Dist())
	if err != nil {
		db.Close()
		return nil, err
	}

	s := &Server{DB: db, Auth: svc, Events: events.New(db.DB, env.Clock), reconciled: make(chan struct{})}
	s.Workspaces = &workspace.Store{DB: db.DB, Events: s.Events, Env: env, Root: cfg.WorkspaceRoot, Cap: cfg.ContainerCap}
	containers := container.Manager{Run: subproc.Exec{}, LabelPrefix: cfg.LabelPrefix, CleanupImage: cfg.CleanupImage}
	feature := map[string]any{}
	if cfg.BotName != "" {
		feature["botName"] = cfg.BotName
	}
	if cfg.BotEmail != "" {
		feature["botEmail"] = cfg.BotEmail
	}
	s.Provisioner = &provision.Provisioner{Workspaces: s.Workspaces, Events: s.Events, Containers: containers,
		Feature: cfg.Feature, FeatureOptions: feature, Timeout: cfg.ProvisionTimeout,
		Logf: func(f string, a ...any) { fmt.Fprintf(os.Stderr, f+"\n", a...) }}
	// A deleting row found at boot is finished by the same delete the route
	// runs (§6: resume the delete), so a delete is resumable from any
	// sub-step it was interrupted after.
	s.Reconciler = &reconcile.Reconciler{Workspaces: s.Workspaces, Events: s.Events,
		Containers: containers, Busy: s.Provisioner.Owns,
		Delete: func(ctx context.Context, w workspace.Workspace, _ string) error {
			return s.Provisioner.ResumeDelete(ctx, w.ID)
		}}
	// The App key is read here, once, from its file (§13.5). A configured
	// key that cannot be read, or that others can read, stops the server:
	// starting without the repository list it was configured for would be
	// a quieter failure than refusing to start.
	// The secrets master key, likewise read once from its file (§10.2,
	// §13.5) and refused if others can read it. A configured key that cannot
	// be loaded stops the server rather than serving with secrets silently
	// off: every workspace's prelude would then fail closed, which is the
	// loud version of the same outage, but at a distance from its cause.
	var secretRoutes api.SecretStore
	if cfg.SecretsKey != "" {
		key, err := secrets.LoadKey(cfg.SecretsKey)
		if err != nil {
			db.Close()
			return nil, err
		}
		s.Secrets = &secrets.Store{DB: db.DB, Key: key, Events: s.Events, Env: env}
		secretRoutes = s.Secrets
	}
	var repoCatalog api.RepoCatalog
	if cfg.GitHubAppID != 0 {
		key, err := github.LoadKey(cfg.GitHubAppKey)
		if err != nil {
			db.Close()
			return nil, err
		}
		// One client, so the catalog and the broker share its token cache.
		gh := &github.Client{AppID: cfg.GitHubAppID, Key: key, BaseURL: cfg.GitHubAPI, Clock: env.Clock,
			HTTP: &http.Client{Timeout: 30 * time.Second}}
		s.Catalog = &catalog.Catalog{DB: db.DB, Events: s.Events, Clock: env.Clock, GitHub: gh}
		s.Broker = &broker.Broker{Dir: cfg.BrokerDir, GitHub: gh, DB: db.DB, Events: s.Events, Env: env}
		if s.Secrets != nil {
			s.Broker.Secrets = s.Secrets
		}
		repoCatalog = s.Catalog
		s.Provisioner.Broker = s.Broker
		s.Provisioner.Cloner = &clone.Cloner{DB: db.DB, GitHub: gh, Runner: subproc.Exec{}}
	}
	handlers := api.SessionRoutes{Auth: svc}.Handlers()
	for name, h := range (api.RepoRoutes{Catalog: repoCatalog}).Handlers() {
		handlers[name] = h
	}
	for name, h := range (api.EventRoutes{Log: s.Events, Clock: env.Clock, Alive: svc.Sessions.Alive}).Handlers() {
		handlers[name] = h
	}
	for name, h := range (api.SecretRoutes{Store: secretRoutes}).Handlers() {
		handlers[name] = h
	}
	for name, h := range (api.WorkspaceRoutes{Provisioner: s.Provisioner, Workspaces: s.Workspaces, Events: s.Events}).Handlers() {
		handlers[name] = h
	}
	s.api = &http.Server{
		Handler:           apiSocketHandler(gate, api.Build(api.MuxAPI, gate, handlers), ui),
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
//     does not declare, to api.Unrouted, which gates it exactly as a real route
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
	apiH := api.Unrouted(gate, apiMux)
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

// provisionShutdownWait is how long shutdown waits for in-flight runs to
// write down that they were interrupted — well inside systemd's 90-second
// stop timeout, so the service is never SIGKILLed for waiting.
const provisionShutdownWait = 20 * time.Second

// Serve runs both muxes until ctx is cancelled, then shuts down gracefully.
func (s *Server) Serve(ctx context.Context) error {
	// Reconcile once at boot, beside serving rather than before it: a slow
	// daemon must not keep the sign-in page down. A failure changes nothing
	// (reconcile refuses to act on a list it could not read), is written to
	// the journal in full, and reaches the event log as Drydock's sentence
	// only — docker's stderr is not ours to publish.
	reconciled := s.reconciled
	go func() {
		defer close(reconciled)
		if _, err := s.Reconciler.Run(ctx); err != nil && ctx.Err() == nil {
			fmt.Fprintf(os.Stderr, "drydock: reconcile: %v\n", err)
			s.Events.Emit(ctx, "", events.Warn, "system.reconcile",
				"Could not reconcile workspaces with Docker at startup; nothing was changed. See the service log.", nil)
		}
		// Every running workspace gets its broker socket back after a
		// restart — after reconciliation, so the set is the one Docker
		// confirmed: a row it marked stopped gets no socket, as a stop
		// closes it. A container whose socket is missing has no GitHub
		// access, which is safe but not what anyone wants.
		if s.Broker != nil && ctx.Err() == nil {
			if err := s.openBrokerSockets(ctx); err != nil {
				fmt.Fprintf(os.Stderr, "drydock: broker: %v\n", err)
			}
		}
	}()
	// Look at the stored secrets once at boot, so a condition that would fail
	// every workspace's commands — a replaced master key, above all — is on
	// the stream now rather than at the first command, and one the last run
	// reported is announced as fixed if it is (frontend §4.5 #12).
	if s.Secrets != nil {
		if _, err := s.Secrets.Undeliverable(ctx); err != nil {
			fmt.Fprintf(os.Stderr, "drydock: secrets: %v\n", err)
		}
	}
	refreshing := make(chan struct{})
	go func() {
		defer close(refreshing)
		if s.Catalog != nil {
			s.Catalog.Run(ctx, func(f string, a ...any) { fmt.Fprintf(os.Stderr, f+"\n", a...) })
		}
	}()
	errc := make(chan error, 2)
	go func() { errc <- s.api.Serve(s.apiLn) }()
	go func() { errc <- s.preview.Serve(s.prevLn) }()

	var serveErr error
	select {
	case <-ctx.Done():
	case serveErr = <-errc:
	}
	// Runs first, while the broker, the log and the database are all still
	// there: each in-flight run fails the step it was on, saying Drydock shut
	// down, and that has to be written before anything it writes to closes.
	// One that outlasts the wait is left mid-provision for boot
	// reconciliation to mark failed.
	s.Provisioner.Shutdown(provisionShutdownWait)
	if s.Broker != nil {
		s.Broker.CloseAll()
	}
	// Streams never go idle, so Shutdown would wait out its whole timeout on
	// every open browser; ending the subscriptions ends the streams first.
	s.Events.Close()
	shutCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = s.api.Shutdown(shutCtx)
	_ = s.preview.Shutdown(shutCtx)
	<-reconciled // they may still be writing; the database closes after them
	<-refreshing
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

// openBrokerSockets opens a socket for every running workspace. A stopped
// one has no container to mount it into, and stop closed it (§9.1: access
// follows Drydock's state); start opens it again at step 5. A workspace
// mid-provision is this process's own run, whose step 5 opens it; a deleting
// one is having its socket removed.
func (s *Server) openBrokerSockets(ctx context.Context) error {
	all, err := s.Workspaces.List(ctx)
	if err != nil {
		return err
	}
	var errs []error
	for _, w := range all {
		if w.State != workspace.Running {
			continue
		}
		if err := s.Broker.Open(ctx, w.ID); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}
