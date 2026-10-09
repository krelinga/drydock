// Package server assembles Drydock's front door: the store, the auth service,
// both muxes, and the two Unix sockets they are served on.
//
// There is no TCP listener anywhere in this package or anything it calls, and
// that is the design's first non-negotiable (§13.5): Caddy owns the only
// LAN-facing port, and Drydock is reachable only through a group-owned socket
// whose one other member is Caddy. A loopback port would be reachable by every
// local process and user; the socket is reachable by its group.
//
// # Rules and details
//
// No TCP listener is asserted on the running process. Serve cancels its own
// context as serving ends, so a failed listener stops the loops it started
// (and returns) as a cancelled context does.
//
// The preview server is preview.Limit(--preview-max-connections,
// api.PreviewFrontDoor(…)) with no SecurityHeaders; Server.PreviewUpstream
// (default Server.Proxy, resolving through the container manager) is read per
// request, and startWith sets Proxy.Resolver and friends between New and
// Serve. A browser-tier build (-tags browsertier, localaddrs_browsertier.go)
// lists no local addresses and logs a startup line saying so; releases pass no
// tags.
//
// TestPreviewProxyOverTheSockets runs a real dev server behind the real proxy
// on the real socket — HTTP, a websocket, SSE, passthrough, a disable — and
// sweeps what the app received, the proxy's log, the temp root and /proc for
// the cookie and token. TestPreviewHandshakeOverTheSockets runs the handshake
// across both real sockets — not signed in to sign-in and back, the cookie
// stripped and the upstream's Set-Cookie for it dropped, revoke-all closing it
// — then sweeps the temp root, the database's bytes, every /proc/*/cmdline and
// the response bodies for the token and the cookie, with a planted control.
// TestPreviewSessionSendsNoReferrer calls the preview.session handler
// directly, refusal and success, for Referrer-Policy: no-referrer (security
// review F4). In tests, seed preview rows only after <-srv.reconciled, or boot
// reconciliation marks the running workspace stopped.
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
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/krelinga/drydock/internal/api"
	"github.com/krelinga/drydock/internal/auth"
	"github.com/krelinga/drydock/internal/broker"
	"github.com/krelinga/drydock/internal/catalog"
	"github.com/krelinga/drydock/internal/classify"
	"github.com/krelinga/drydock/internal/claudeimage"
	"github.com/krelinga/drydock/internal/clone"
	"github.com/krelinga/drydock/internal/config"
	"github.com/krelinga/drydock/internal/container"
	"github.com/krelinga/drydock/internal/dockerguard"
	"github.com/krelinga/drydock/internal/events"
	"github.com/krelinga/drydock/internal/github"
	"github.com/krelinga/drydock/internal/identity"
	"github.com/krelinga/drydock/internal/life"
	"github.com/krelinga/drydock/internal/login"
	"github.com/krelinga/drydock/internal/preview"
	"github.com/krelinga/drydock/internal/provision"
	"github.com/krelinga/drydock/internal/reconcile"
	"github.com/krelinga/drydock/internal/secrets"
	"github.com/krelinga/drydock/internal/store"
	"github.com/krelinga/drydock/internal/subproc"
	"github.com/krelinga/drydock/internal/supervisor"
	"github.com/krelinga/drydock/internal/sys"
	"github.com/krelinga/drydock/internal/usage"
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
	// Identity is the expiry watch over the shared Claude login (§7.3),
	// always present: with no volume yet it reports absent, which is the
	// first-run state. Its Source is a test's seam, set between New and
	// Serve.
	Identity *identity.Watch
	// Supervisor runs each running workspace's `claude remote-control`
	// server (§8). Like the Provisioner's, its exported fields are a test's
	// seam between New and Serve.
	Supervisor *supervisor.Manager
	// Login is the handshake (§7.2). Its Launcher is a test's seam, set
	// between New and Serve.
	Login *login.Manager
	// Usage measures each workspace's memory and disk and the workspace
	// filesystem (§6 *Resources*, §12 *Disk full*). Nil when env has no
	// Disk, which only a test's env lacks; its fields are a test's seam.
	Usage *usage.Sampler
	// Previews is the preview handshake's state (PF §7): the one-time
	// tokens in memory, the preview sessions in the database.
	Previews *preview.Service
	// Proxy is the preview proxy (PF §8, §13 step 3): it resolves the
	// workspace's container by label before every dial. Its exported fields
	// are a test's seam, set between New and Serve.
	Proxy *preview.Proxy
	// PreviewUpstream is what a request with a valid preview cookie reaches:
	// Proxy. A test's seam, read per request, so it may be set between New
	// and Serve.
	PreviewUpstream preview.Upstream
	// reconciled closes when boot reconciliation has finished, so a test
	// can set up workspace rows reconciliation would otherwise move.
	reconciled chan struct{}
	// identityWork is the identity watch's group in Serve's work, stored as
	// Serve starts the watch: a test stops it to see the check route refuse
	// what nothing would answer.
	identityWork atomic.Pointer[life.Group]
	// loginWork is the login handshake's group in Serve's work, stored
	// likewise: a test stops it to see the begin route refuse.
	loginWork atomic.Pointer[life.Group]
	// supervisorWork is the session supervisors' group in Serve's work,
	// stored for tests that wait for it.
	supervisorWork atomic.Pointer[life.Group]
	// provisionWork is the provisioner's group in Serve's work, stored
	// likewise: a test stops it to see the workspace routes refuse.
	provisionWork atomic.Pointer[life.Group]
	// clock is env's, for the bounds shutdown waits under.
	clock sys.Clock
	// repoOf caches each workspace's repository id for secretValues: it is
	// fixed for the workspace's life, so the log's per-read path asks the
	// database once per workspace, not once per terminal read.
	repoOf  sync.Map // workspace id → int64
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
	// Before anything can touch a container: whose containers these are is
	// decided by the prefix the database was created with (§6).
	if err := db.ClaimLabelPrefix(ctx, cfg.LabelPrefix, env.Clock.Now()); err != nil {
		db.Close()
		return nil, err
	}
	svc := auth.New(db.DB, env)
	// A preview session dies with the auth session's idle window as well as
	// its own (PF §5), and with its revocation by the schema's cascade.
	previews := &preview.Service{DB: db.DB, Clock: env.Clock, Random: env.Random,
		Domain: cfg.PreviewDomain, AuthIdle: auth.IdleLifetime}
	gate := api.SessionGate{Sessions: svc.Sessions, UIOrigin: cfg.UIOrigin, UIHost: cfg.UIHost, Previews: previews}

	ui, err := web.New(web.Dist())
	if err != nil {
		db.Close()
		return nil, err
	}

	s := &Server{DB: db, Auth: svc, Events: events.New(db.DB, env.Clock), reconciled: make(chan struct{}),
		Previews: previews, clock: env.Clock}
	s.Workspaces = &workspace.Store{DB: db.DB, Events: s.Events, Env: env, Root: cfg.WorkspaceRoot, Cap: cfg.ContainerCap}
	// Drydock's own uid owns the shared credential volume (§7.1): the dev
	// container CLI, run as Drydock, gives every workspace's remote user
	// this uid, and the login handshake writes the credential as it.
	// The docker guard is this binary, run as "docker" from each
	// workspace's guard directory (design §6, "The docker guard"). A path
	// that cannot be found leaves Binary empty, and every devcontainer
	// invocation then fails rather than run unguarded.
	self, _ := os.Executable()
	containers := container.Manager{Run: subproc.Exec{}, LabelPrefix: cfg.LabelPrefix, CleanupImage: cfg.CleanupImage,
		ClaudeUID: os.Getuid(), ClaudeGID: os.Getgid(), Guard: &dockerguard.Guard{Binary: self},
		LocalAddrs: previewLocalAddrs, Clock: env.Clock,
		Logf: func(f string, a ...any) { fmt.Fprintf(os.Stderr, f+"\n", a...) }}
	if browserTierBuild {
		fmt.Fprintln(os.Stderr, "drydock: built for the browser tier (-tags browsertier): a preview container at one of this host's own addresses is dialled. Never a release.")
	}
	// The preview proxy dials the container's Docker-network address,
	// resolved from Docker by label before every dial (PF §8.1).
	s.Proxy = &preview.Proxy{Resolver: containerResolver{containers}, Clock: env.Clock,
		IdleTimeout: cfg.PreviewIdleTimeout,
		Logf:        func(f string, a ...any) { fmt.Fprintf(os.Stderr, f+"\n", a...) }}
	s.PreviewUpstream = s.Proxy
	// The port registry writes its events with its rows, and a disable or a
	// retire closes that port's open websockets at once — not at their next
	// recheck (PF §13.4, "What step 4 should know").
	previews.Events = s.Events
	previews.Revoked = func(portID string) {
		s.Proxy.CloseWhere(func(t preview.Target) bool { return t.PortID == portID })
	}
	// The Claude Code version is this binary's, not the Feature's default:
	// the classifiers compiled in here were recorded against it, so a
	// Feature release under the same major tag cannot move it (§11).
	feature := map[string]any{"claudeCodeVersion": classify.ClaudeCodeVersion}
	if cfg.BotName != "" {
		feature["botName"] = cfg.BotName
	}
	if cfg.BotEmail != "" {
		feature["botEmail"] = cfg.BotEmail
	}
	s.Provisioner = &provision.Provisioner{Workspaces: s.Workspaces, Events: s.Events, Containers: containers,
		Feature: cfg.Feature, FeatureOptions: feature, Timeout: cfg.ProvisionTimeout,
		Disk: env.Disk, DiskLimitPercent: cfg.DiskLimitPercent,
		ClaudeVolume: cfg.ClaudeVolume, ClaudeCodeVersion: classify.ClaudeCodeVersion,
		Logf: func(f string, a ...any) { fmt.Fprintf(os.Stderr, f+"\n", a...) }}
	// The session supervisor (§8): started at step 8, at boot for every
	// running workspace, and from POST …/supervisor; stopped, SIGTERM first,
	// before a stop, a rebuild or a delete touches the container.
	s.Supervisor = &supervisor.Manager{DB: db.DB, Events: s.Events, Env: env,
		Runtime: supervisor.ContainerRuntime{Containers: containers, PTY: subproc.Exec{}},
		Spec:    s.Provisioner.SessionSpec,
		Logf:    func(f string, a ...any) { fmt.Fprintf(os.Stderr, f+"\n", a...) }}
	s.Provisioner.StartSupervisor = func(ctx context.Context, w workspace.Workspace) error {
		return s.Supervisor.Start(ctx, w.ID)
	}
	s.Provisioner.StopSupervisor = func(ctx context.Context, w workspace.Workspace) error {
		return s.Supervisor.Stop(ctx, w.ID)
	}
	s.Provisioner.ForgetSupervisor = s.Supervisor.Forget
	s.Provisioner.ParkSupervisor = func(ctx context.Context, w workspace.Workspace, reason, detail string) error {
		return s.Supervisor.Park(ctx, w.ID, supervisor.Reason(reason), detail)
	}
	s.Provisioner.SupervisorRestart = s.Supervisor.Restart
	// A sign-in resumes the supervisors waiting on one, each as a provisioner
	// job (ResumeAwaitingLogin, wired to the identity watch below).
	s.Provisioner.SupervisorsAwaitingLogin = s.Supervisor.AwaitingLogin
	s.Provisioner.SupervisorResume = s.Supervisor.Resume
	// Step 3's declared ports become the registry's declared rows: listed,
	// never enabled by it (PF §13 step 4).
	s.Provisioner.DeclarePorts = func(ctx context.Context, id string, ports []container.DeclaredPort) error {
		ds := make([]preview.Declared, len(ports))
		for i, p := range ports {
			ds[i] = preview.Declared{Port: p.Port, Label: p.Label}
		}
		return previews.DeclarePorts(ctx, id, ds)
	}
	// A deleting row found at boot is finished by the same delete the route
	// runs (§6: resume the delete), so a delete is resumable from any
	// sub-step it was interrupted after.
	s.Reconciler = &reconcile.Reconciler{Workspaces: s.Workspaces, Events: s.Events,
		Containers: containers, Exclusive: s.Provisioner.Unowned,
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
		// A granted secret's value a session server prints is masked in its
		// log as it is written (§13.5: redact by default).
		s.Supervisor.Redact = s.secretValues
		// …and so is one a failed build printed, in the build log it holds —
		// failing closed: values that cannot be read withhold the log.
		s.Provisioner.Redact = s.secretValuesStrict
		// A removed repository's grants are deleted when nothing holds it
		// any more (§4), by a workspace's removal or by a refresh; the
		// broker's snapshot must not outlive them.
		s.Workspaces.GrantsDropped = s.Secrets.Invalidate
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
		s.Catalog = &catalog.Catalog{DB: db.DB, Events: s.Events, Clock: env.Clock, GitHub: gh,
			Logf: func(f string, a ...any) { fmt.Fprintf(os.Stderr, f+"\n", a...) }}
		s.Broker = &broker.Broker{Dir: cfg.BrokerDir, GitHub: gh, DB: db.DB, Events: s.Events, Env: env,
			Logf: func(f string, a ...any) { fmt.Fprintf(os.Stderr, f+"\n", a...) }}
		if s.Secrets != nil {
			s.Broker.Secrets = s.Secrets
			s.Catalog.GrantsDropped = s.Secrets.Invalidate
		}
		repoCatalog = s.Catalog
		s.Provisioner.Broker = s.Broker
		s.Provisioner.Cloner = &clone.Cloner{DB: db.DB, GitHub: gh, Runner: subproc.Exec{}}
	}
	// A sign-out closes the preview websockets its sessions authorized at
	// once; anything else that ends a preview session — `drydock passwd`, a
	// disabled port, an expiry — closes them at the proxy's next recheck.
	handlers := api.SessionRoutes{Auth: svc, Revoked: func(all bool, id string) {
		s.Proxy.CloseWhere(func(t preview.Target) bool { return all || t.AuthSessionID == id })
	}}.Handlers()
	for name, h := range (api.SupervisorRoutes{Provisioner: s.Provisioner, Workspaces: s.Workspaces, Logs: s.supervisorLogs}).Handlers() {
		handlers[name] = h
	}
	for name, h := range (api.RepoRoutes{Catalog: repoCatalog}).Handlers() {
		handlers[name] = h
	}
	for name, h := range (api.EventRoutes{Log: s.Events, Clock: env.Clock, Alive: svc.Sessions.Alive}).Handlers() {
		handlers[name] = h
	}
	for name, h := range (api.SecretRoutes{Store: secretRoutes}).Handlers() {
		handlers[name] = h
	}
	// The Claude image is built on first need — the first check that finds
	// a credential file — and reused; the file itself is read with the
	// cleanup helper's busybox, so blanked and absent never wait on it.
	// One builder for the watch and the login, so a first build is never
	// run twice at once.
	claudeImage := &claudeimage.Builder{Run: subproc.Exec{}, Base: cfg.ClaudeBaseImage, Version: classify.ClaudeCodeVersion}
	s.Identity = &identity.Watch{DB: db.DB, Events: s.Events, Clock: env.Clock,
		Volume: cfg.ClaudeVolume, Window: cfg.IdentityExpiringWindow, Interval: cfg.IdentityInterval,
		Timeout: cfg.IdentityCheckTimeout,
		Source: identity.DockerSource{Run: subproc.Exec{},
			Image:     claudeImage,
			FileImage: cfg.CleanupImage, Volume: cfg.ClaudeVolume, LabelPrefix: cfg.LabelPrefix, Clock: env.Clock,
			Logf: func(f string, a ...any) { fmt.Fprintf(os.Stderr, f+"\n", a...) }},
		Logf: func(f string, a ...any) { fmt.Fprintf(os.Stderr, f+"\n", a...) }}
	// The supervisor defers to the stored identity the watch keeps (§7.3,
	// frontend §6.6): a signed-out fleet starts no session server and spends
	// no restart. Read through the watch, so there is one reader of the row.
	s.Supervisor.Identity = func(ctx context.Context) (string, bool) {
		v, err := s.Identity.Read(ctx)
		if err != nil || v.State == nil {
			return "", false
		}
		return string(*v.State), true
	}
	// …and a live login, announced, resumes every supervisor waiting on one:
	// a direct call from the watch into the provisioner, which gives each a
	// job of its own (§8). No component follows the event log; only the SSE
	// stream does.
	s.Identity.OnChange = func(ctx context.Context, v identity.View) {
		if v.State == nil || !v.State.Live() {
			return
		}
		if err := s.Provisioner.ResumeAwaitingLogin(ctx); err != nil && !errors.Is(err, provision.ErrShuttingDown) {
			fmt.Fprintf(os.Stderr, "drydock: resuming session servers after a sign-in: %v\n", err)
		}
	}
	// The login handshake (§7.2) runs as Drydock's own uid: the dev
	// container CLI gives every workspace's remote user this uid, so the
	// credential the login writes is theirs to read.
	s.Login = &login.Manager{Events: s.Events, Clock: env.Clock, Identity: s.Identity,
		Launcher: login.DockerLauncher{Run: subproc.Exec{}, Volumes: containers, Image: claudeImage,
			Volume: cfg.ClaudeVolume, LabelPrefix: cfg.LabelPrefix,
			UID: os.Getuid(), GID: os.Getgid(), Clock: env.Clock},
		Logf: func(f string, a ...any) { fmt.Fprintf(os.Stderr, f+"\n", a...) }}
	for name, h := range (api.ClaudeRoutes{Watch: s.Identity, Login: s.Login}).Handlers() {
		handlers[name] = h
	}
	routes := api.WorkspaceRoutes{Provisioner: s.Provisioner, Workspaces: s.Workspaces, Events: s.Events,
		BuildLogs: s.Provisioner}
	if env.Disk != nil {
		s.Usage = &usage.Sampler{Workspaces: s.Workspaces, Containers: containers, Disk: env.Disk, Clock: env.Clock, Random: env.Random,
			Root: cfg.WorkspaceRoot, LimitPercent: cfg.DiskLimitPercent,
			Publish: func(f usage.Frame) { _ = s.Events.Broadcast(usage.FrameName, f) },
			Logf:    func(f string, a ...any) { fmt.Fprintf(os.Stderr, f+"\n", a...) }}
		routes.Resources = s.Usage
	}
	for name, h := range previewHandlers(previews) {
		handlers[name] = h
	}
	// The probe dials through the proxy itself — Address, the connect,
	// Confirm — read per request, so it is the proxy a test configured.
	for name, h := range (api.PortRoutes{Registry: previews, Prober: proxyProber{s}}).Handlers() {
		handlers[name] = h
	}
	for name, h := range routes.Handlers() {
		handlers[name] = h
	}
	s.api = &http.Server{
		Handler:           apiSocketHandler(gate, api.Build(api.MuxAPI, gate, handlers), ui),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
		// No WriteTimeout: /api/events is a long-lived SSE stream.
	}
	s.preview = &http.Server{
		// The front door, not Build: Drydock's two reserved paths behind
		// their gates, and everything else redirected to the handshake or
		// handed to the upstream with the preview cookie stripped (PF §7,
		// §13 steps 1–3). Never ServeMux's 404. The cap is outside it, so
		// it counts the handshake's redirects as well as the proxied
		// requests and their websockets (PF §10.7).
		Handler: preview.Limit(cfg.PreviewMaxConnections, api.PreviewFrontDoor(gate, previewHandlers(previews), preview.UpstreamFunc(
			func(w http.ResponseWriter, r *http.Request, t preview.Target) {
				s.PreviewUpstream.ServePreview(w, r, t)
			}))),
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

// previewHandlers are the preview handshake's handlers, by route name: the
// API mux's /preview/authorize and the preview mux's /.drydock/session and
// /.drydock/denied. Each mux mounts only its own (PF §6).
//
// The preview server deliberately carries no web.SecurityHeaders: what it
// mostly serves is a repository's own app, whose headers are its own. That
// leaves one header nothing else will send. /.drydock/session carries the
// single-use token in its query string, so its response must say
// Referrer-Policy: no-referrer, or the token URL leaks onward in the Referer
// of whatever the app loads next (security review F4, PF §7).
// TestPreviewSessionSendsNoReferrer holds that obligation on the handler
// itself, and api.PreviewFrontDoor sets it again on every answer of Drydock's.
func previewHandlers(p *preview.Service) map[string]http.HandlerFunc {
	return api.PreviewHandshake{Previews: p}.Handlers()
}

// proxyProber is the probe route's dial: the server's own preview proxy, so
// the probe and a preview never resolve a container two ways.
type proxyProber struct{ s *Server }

func (p proxyProber) Probe(ctx context.Context, workspaceID string, port int) preview.ProbeResult {
	return p.s.Proxy.Probe(ctx, workspaceID, port)
}

// containerResolver is the preview proxy's view of the container manager.
type containerResolver struct{ m container.Manager }

func (c containerResolver) Resolve(ctx context.Context, workspaceID string) (preview.Endpoint, error) {
	a, err := c.m.Address(ctx, workspaceID)
	if errors.Is(err, container.ErrNotRunning) || errors.Is(err, container.ErrNoAddress) || errors.Is(err, container.ErrAmbiguous) {
		return preview.Endpoint{}, fmt.Errorf("%w: %v", preview.ErrNotRunning, err)
	}
	if err != nil {
		return preview.Endpoint{}, err
	}
	return preview.Endpoint{ContainerID: a.ContainerID, IP: a.IP}, nil
}

func (c containerResolver) Confirm(ctx context.Context, workspaceID string, e preview.Endpoint) error {
	err := c.m.Confirm(ctx, workspaceID, container.Address{ContainerID: e.ContainerID, IP: e.IP})
	if errors.Is(err, container.ErrMoved) {
		return fmt.Errorf("%w: %v", preview.ErrNotRunning, err)
	}
	return err
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

// workShutdownWait bounds how long shutdown waits for the goroutines of
// Serve's life.Group, all of them at once:
//
//   - each workspace job in flight, whose step's subprocess is SIGTERMed as
//     its context ends and wound down within subproc's 5 s WaitDelay, after
//     which it writes the step's failure, its move to failed and its
//     workspace.job end to the local database — about 5 s. A job that had
//     unpaused a paused container also pauses it again, under provision's
//     RepauseTimeout (20 s), whose own last docker command may need one more
//     wind-down: 5 + 20 + 5 = 30 s at worst, which repauseFits below holds
//     inside this wait, so an operator's pause is never left undone by a
//     shutdown. A job still running at the deadline anyway (a database write
//     that hangs) is named in the log, and whatever it opens after that is
//     refused by the broker, which CloseAll has closed for good; a job cut
//     off before writing its step's failure is boot reconciliation's, which
//     closes the dangling step;
//
//   - the catalog's refresh, whose GitHub calls and transaction end with its
//     context;
//
//   - the identity watch's check, which ends with it too but then removes a
//     cut-off read's helper container under its own 30-second bound;
//
//   - a login in progress, which kills its process, removes its container and
//     announces its end in about 30 s at worst: one 5 s kill (or an abandoned
//     launch's docker command winding down within subproc's 5 s WaitDelay),
//     the 15 s removal plus that WaitDelay for a docker command it cut off,
//     and the 5 s announcement — counted beside login's killWait,
//     removeTimeout and emitTimeout.
//
//   - each session supervisor's loop, which closes its terminal at once and
//     signals no server — its own `devcontainer exec` is SIGTERMed and given
//     KillWait (5 s) — after a docker call it was in winds down within
//     subproc's 5 s WaitDelay; a stop under way keeps the terminal until it
//     has decided, and that stop is a job's, cancelled with it: about 10 s;
//     and a stop of a server hung at a gate, cut off the same way.
//
// So this is the longest of those and a little more.
//
// Shutdown's whole budget must stay inside systemd's 90-second stop timeout,
// so the service is never SIGKILLed for waiting. This wait runs beside the
// HTTP servers' 10 s drain, so the most shutdown waits is the longer of the
// two: 35 s.
const workShutdownWait = 35 * time.Second

// repauseFits does not compile when a stop cut off by shutdown — its
// subprocess's wind-down, the re-pause's bound, the re-pause's own last
// wind-down — could outlast workShutdownWait: a negative constant does not
// convert to uint64. Lengthen RepauseTimeout or shorten this wait and the
// build says so, rather than a comment no one re-reads.
const repauseFits = uint64(workShutdownWait - provision.RepauseTimeout - 2*subproc.DefaultWaitDelay)

// secretValues are the values of the secrets a workspace's repository is
// granted, for its session server's log to mask. Asked on every terminal
// read, so it touches the database at most once per workspace: the
// repository id is cached (a workspace never changes repository), and the
// values come from the broker's snapshot, which is no cache to go stale — a
// secret write or a grant change rebuilds it, so a value granted a moment
// ago is masked from the next read on (§10.3: no decryption per call).
func (s *Server) secretValues(ctx context.Context, workspaceID string) []string {
	var repo int64
	if v, ok := s.repoOf.Load(workspaceID); ok {
		repo = v.(int64)
	} else {
		w, err := s.Workspaces.Get(ctx, workspaceID)
		if err != nil {
			return nil
		}
		repo = w.RepositoryID
		s.repoOf.Store(workspaceID, repo)
	}
	entries, err := s.Secrets.Resolve(ctx, repo)
	if err != nil {
		return nil
	}
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		out = append(out, e.Value)
	}
	return out
}

// secretValuesStrict is secretValues for a log served on request: an error
// when the values cannot be read, so the caller withholds rather than serves
// a log it could not mask.
func (s *Server) secretValuesStrict(ctx context.Context, workspaceID string) ([]string, error) {
	w, err := s.Workspaces.Get(ctx, workspaceID)
	if err != nil {
		return nil, err
	}
	entries, err := s.Secrets.Resolve(ctx, w.RepositoryID)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		out = append(out, e.Value)
	}
	return out, nil
}

func (s *Server) supervisorLogs(id string, n int) ([]api.LogLine, bool, bool) {
	lines, truncated, held := s.Supervisor.Logs(id, n)
	out := make([]api.LogLine, len(lines))
	for i, l := range lines {
		out[i] = api.LogLine{N: l.N, At: l.At, Text: l.Text}
	}
	return out, truncated, held
}

// reconcileWarning is the boot reconciliation's failure in Drydock's words:
// "nothing was changed" only when nothing was (reconcile.ErrNothingChanged),
// and otherwise how many workspaces could not be reconciled while the rest
// were. docker's stderr stays in the journal.
func reconcileWarning(err error) string {
	var p *reconcile.Partial
	switch {
	case errors.Is(err, reconcile.ErrNothingChanged):
		return "Could not reconcile workspaces with Docker at startup; nothing was changed. See the service log."
	case errors.As(err, &p) && len(p.Errs) == 1:
		return "Reconciled workspaces with Docker at startup, but one could not be. See the service log."
	case errors.As(err, &p):
		return fmt.Sprintf("Reconciled workspaces with Docker at startup, but %d could not be. See the service log.", len(p.Errs))
	default:
		return "Reconciling workspaces with Docker at startup failed part-way. See the service log."
	}
}

// Serve runs both muxes until ctx is cancelled, then shuts down gracefully.
func (s *Server) Serve(ctx context.Context) error {
	// Everything started below ends on ctx, and shutdown waits for each of
	// them. Serving can also end with a listener's error while the caller's
	// context is live, so Serve cancels its own as serving ends, either way.
	ctx, stop := context.WithCancel(ctx)
	defer stop()
	// work owns the goroutines of the components that have moved onto
	// life.Group: shutdown stops it and waits for it before the database
	// closes. Today that is the provisioner's jobs, the session
	// supervisors, the catalog, the identity watch and the login handshake;
	// the rest still end on ctx.
	work := life.NewGroup(ctx)
	defer work.Stop()
	// Every workspace job — a run, a stop, a delete, a session server
	// restart — in work, before anything can ask for one: the routes, and
	// reconciliation's resumed deletes just below.
	provisionWork := work.Child("provision")
	s.provisionWork.Store(provisionWork)
	if err := s.Provisioner.RunIn(provisionWork); err != nil {
		fmt.Fprintf(os.Stderr, "drydock: provision: %v\n", err)
	}
	// Every session supervisor's loop, in work, before a job's step 8 or
	// boot's adoption can start one. Its group stopping is the detach: every
	// terminal closed, no server signalled (Spike 02: sessions survive a
	// restart and reconnect).
	supervisorWork := work.Child("supervisor")
	s.supervisorWork.Store(supervisorWork)
	if err := s.Supervisor.RunIn(supervisorWork); err != nil {
		fmt.Fprintf(os.Stderr, "drydock: supervisor: %v\n", err)
	}
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
			s.Events.Emit(ctx, "", events.Warn, "system.reconcile", reconcileWarning(err), nil)
		}
		// Then the helper containers an earlier process left (§6), of every
		// kind internal/ephemeral knows — cleanup helpers, log probes,
		// identity reads, login containers, owner helpers: after
		// reconciliation, whose resumed deletes have finished by now, by this
		// instance's prefix only, never a workspace container, and sparing
		// whatever this process is running.
		if ctx.Err() == nil {
			if _, err := s.Provisioner.SweepHelpers(ctx); err != nil && ctx.Err() == nil {
				fmt.Fprintf(os.Stderr, "drydock: sweeping helper containers: %v\n", err)
			}
			// And any docker guard policy an up killed mid-run left.
			if err := s.Provisioner.SweepGuardPolicies(ctx); err != nil && ctx.Err() == nil {
				fmt.Fprintf(os.Stderr, "drydock: sweeping docker guard policies: %v\n", err)
			}
		}
		// Every running workspace gets its broker socket back after a
		// restart — after reconciliation, so the set is the one Docker
		// confirmed: a row it marked stopped gets no socket, as a stop
		// closes it. A container whose socket is missing has no GitHub
		// access, which is safe but not what anyone wants. A container
		// found paused gets neither its socket nor its session server: one
		// listing, shared by both.
		var paused provision.Paused
		if ctx.Err() == nil {
			paused = s.Provisioner.PausedAtBoot(ctx)
		}
		if s.Broker != nil && ctx.Err() == nil {
			if err := s.Provisioner.ReopenSockets(ctx, paused); err != nil {
				fmt.Fprintf(os.Stderr, "drydock: broker: %v\n", err)
			}
		}
		// Then every running workspace's session server (§6: adopt, and
		// restart the supervisor) — after the sockets, since the launch
		// fetches the workspace's secrets through its socket.
		if ctx.Err() == nil {
			if err := s.Provisioner.ResumeSupervisors(ctx, paused); err != nil && ctx.Err() == nil {
				fmt.Fprintf(os.Stderr, "drydock: session servers: %v\n", err)
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
	// The expiry watch (§7.3): a check at once and every six hours, and
	// whatever POST /api/auth/claude/check and the login handshake ask for:
	// one worker, in work.
	identityWork := work.Child("identity")
	s.identityWork.Store(identityWork)
	if err := s.Identity.Start(identityWork); err != nil {
		fmt.Fprintf(os.Stderr, "drydock: identity: %v\n", err)
	}
	// The login handshake (§7.2): each login, and everything it starts, in
	// work. A login container an earlier process left — killed mid-login,
	// or a crash — goes at the next login's start, and in boot's helper
	// sweep above, which spares a login this process has begun.
	loginWork := work.Child("login")
	s.loginWork.Store(loginWork)
	if err := s.Login.Start(loginWork); err != nil {
		fmt.Fprintf(os.Stderr, "drydock: login: %v\n", err)
	}
	// Memory and disk, on their own cadence (§6 *Resources*): measurements
	// for the card, published live and never written to the event log.
	sampling := make(chan struct{})
	go func() {
		defer close(sampling)
		if s.Usage != nil {
			s.Usage.Run(ctx)
		}
	}()
	// The repository list, at once and every 15 minutes, and whatever POST
	// /api/repos/refresh asks for: one worker, in work.
	if s.Catalog != nil {
		if err := s.Catalog.Start(work.Child("catalog")); err != nil {
			fmt.Fprintf(os.Stderr, "drydock: catalog: %v\n", err)
		}
	}
	errc := make(chan error, 2)
	go func() { errc <- s.api.Serve(s.apiLn) }()
	go func() { errc <- s.preview.Serve(s.prevLn) }()

	var serveErr error
	select {
	case <-ctx.Done():
	case serveErr = <-errc:
	}
	stop()
	// work's goroutines end now, beside the waits below rather than after
	// them, and are waited for before the broker's sockets and the database
	// close: every workspace job in flight, each of which fails the step it
	// was on, saying Drydock shut down, and writes its workspace.job end
	// (one that outlasts the wait is left mid-provision for boot
	// reconciliation to mark failed); the catalog's refreshes; the identity
	// watch's checks, periodic or asked for; and a login in progress, which
	// ends failed, saying Drydock shut down, once its process is killed and
	// its container removed. Nothing asked of them from here on starts — a
	// route asking for a job answers 503 — and a login's last act, asking
	// the watch for a check, is refused at once, since the watch is
	// stopping too. The session supervisors detach: every terminal closes
	// and no server is signalled, so the servers keep serving for the next
	// process to adopt.
	work.Stop()
	workStopped := make(chan struct{})
	go func() {
		defer close(workStopped)
		deadline, cancel := sys.NewTimer(s.clock, workShutdownWait)
		defer cancel()
		if late := work.Wait(deadline); len(late) > 0 {
			// Said, because what follows is the database closing under it.
			fmt.Fprintf(os.Stderr, "drydock: shutdown: still running %s after it: %s\n",
				workShutdownWait, strings.Join(late, ", "))
		}
	}()
	// The supervisors' loops end beside the jobs, and that is safe in
	// either order: stop() above cancelled ctx, and with it every job's
	// context and the supervisor group's, in one call, before anything here
	// waits. A job's StopSupervisor after that signals nothing (docker exec
	// is not started under a cancelled context, and a stop cut off records
	// nothing). Its StartSupervisor (step 8, or a restart's second half) is
	// ordered against the detach by the group itself: launchLocked takes the
	// loop's goroutine with TryGo, under the mutex Stop takes, so one that
	// got in first has a context that is already cancelled — its loop leaves
	// before launching, or closes its terminal — and one after is ErrClosed,
	// which the cancelled step reports as Drydock shutting down.
	// Streams never go idle, so Shutdown would wait out its whole timeout on
	// every open browser; ending the subscriptions ends the streams first.
	// A job still ending writes its last events after this: they are rows
	// all the same, which a browser replays from the next process.
	s.Events.Close()
	shutCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = s.api.Shutdown(shutCtx)
	_ = s.preview.Shutdown(shutCtx)
	// Shutdown does not track a hijacked connection: a preview's websocket
	// is closed here, or it would outlive the server that proxied it.
	s.Proxy.Close()
	<-workStopped
	// Boot's follow-ups end with ctx; one may be reopening a broker socket
	// under the provisioner's lock right now, so the sockets close after.
	<-reconciled
	// After every job and boot's follow-ups: a run's step 5, or a stop's
	// re-pause, opens a socket, and a job still ending inside the wait must
	// be able to (TestTheBrokerOutlivesEveryJob). CloseAll is final, so a
	// job the wait gave up on that opens one afterwards is refused
	// (broker.ErrClosed) rather than leaving a socket nobody serves.
	if s.Broker != nil {
		s.Broker.CloseAll()
	}
	<-sampling
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
