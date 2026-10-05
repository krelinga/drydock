// Package provision is design §6 end to end: one request takes a repository
// from the catalog to a running container, through the eight steps, with
// every step writing its events.
//
// workspace.Store.Provision owns the order, the states and the events; this
// package supplies the eight real step functions, runs them off the request
// path (every mutating route answers 202 and the client follows the stream,
// §5), and keeps the two promises a background run has to keep:
//
//   - One run per workspace at a time. A second start, or a start racing a
//     create, is refused as in_progress rather than run twice.
//   - Every run ends somewhere reconciliation understands. A run is bounded by
//     a timeout and by Drydock's own shutdown; either one fails the step it
//     interrupted, with a sentence saying which. A process that dies too fast
//     to write that (kill -9) leaves the workspace mid-provision, and boot
//     reconciliation marks it failed (§6) — but reconciliation is also told
//     which workspaces are being provisioned right now (Busy), so a create in
//     the first seconds after boot is not mistaken for an interrupted one.
package provision

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"sync"
	"time"

	"github.com/krelinga/drydock/internal/clone"
	"github.com/krelinga/drydock/internal/container"
	"github.com/krelinga/drydock/internal/events"
	"github.com/krelinga/drydock/internal/workspace"
)

// DefaultImage is the image of the minimal configuration Drydock writes for a
// repository that has no devcontainer.json (§6 step 3).
//
// mcr.microsoft.com/devcontainers/base:debian, because:
//   - it has a non-root `vscode` user, which is what the rest of the design
//     assumes a workspace runs as — §6 step 6 mounts the Claude credential
//     volume at /home/vscode/.claude, and a session should not run as root;
//   - it carries git, curl and sudo, so the Feature installs onto it without
//     pulling in a toolchain, and git works for the agent from the start;
//   - it is the base the Feature's own test suite and the container tier run
//     on, so the one image a plain repository gets is the one that is tested.
//
// It is deliberately not a language image. A repository with no
// devcontainer.json has said nothing about its toolchain, and guessing one
// would be inventing state; the agent can install what it needs, or the
// operator can add a devcontainer.json to the repository.
const DefaultImage = "mcr.microsoft.com/devcontainers/base:debian"

// DefaultConfig is the minimal devcontainer.json, as written.
func DefaultConfig() []byte {
	return []byte(`{"name":"drydock-default","image":"` + DefaultImage + `"}` + "\n")
}

var (
	// ErrNotConfigured: no GitHub App, so nothing can be cloned and no
	// broker can serve a token.
	ErrNotConfigured = errors.New("provision: no GitHub App is configured")
	// ErrUnknownRepository: no such repository in the catalog, or one the
	// installation no longer covers.
	ErrUnknownRepository = errors.New("provision: no such repository in the installation")
	// ErrBadBranch: a branch name the clone would refuse anyway, refused
	// before a row exists rather than as a failed clone.
	ErrBadBranch = errors.New("provision: not a branch name Drydock can clone")
	// ErrShuttingDown: Drydock is stopping and starts nothing new.
	ErrShuttingDown = errors.New("provision: Drydock is shutting down")
)

// Broker is what provisioning needs from internal/broker.
type Broker interface {
	Open(ctx context.Context, workspaceID string) error
	SocketPath(workspaceID string) string
}

// Provisioner runs workspaces through §6.
type Provisioner struct {
	Workspaces *workspace.Store
	Events     *events.Log
	// Cloner and Broker are nil when no GitHub App is configured; Create
	// and Start then refuse with ErrNotConfigured.
	Cloner     *clone.Cloner
	Broker     Broker
	Containers container.Manager
	// Feature is the --additional-features reference (config.Feature), and
	// FeatureOptions its options: botName and botEmail.
	Feature        string
	FeatureOptions map[string]any
	// RemoteEnv is added to every workspace's --remote-env, beside
	// DRYDOCK_WORKSPACE and DRYDOCK_REPO. Empty in production; a test sets
	// DRYDOCK_GITHUB_HOST to point the Feature's helper at a fake.
	RemoteEnv map[string]string
	// Config is the minimal devcontainer.json for a repository without one.
	// Nil means DefaultConfig. A test adds runArgs to it.
	Config []byte
	// Timeout bounds one run (config.ProvisionTimeout).
	Timeout time.Duration
	// Logf receives each failed run's full error — the half that never
	// reaches the event log (§6). Nil discards it.
	Logf func(format string, args ...any)

	mu     sync.Mutex
	active map[string]bool // runs in flight
	owned  map[string]bool // every workspace a run was started for
	base   context.Context
	stop   context.CancelFunc
	closed bool
	wg     sync.WaitGroup
}

func (p *Provisioner) logf(format string, args ...any) {
	if p.Logf != nil {
		p.Logf(format, args...)
	}
}

// Owns reports whether this process has started a run for the workspace,
// finished or not. It is reconciliation's Busy: boot reconciliation is about
// state left by an earlier process, and a workspace this one provisioned is
// in whatever state its own run left it — even if it is still pending, or
// finished in the moment between reconciliation's listing and its action.
func (p *Provisioner) Owns(id string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.owned[id]
}

// Create is the clone button (POST /api/workspaces): validate, insert the
// row in pending, and start the run in the background. The returned
// workspace is the row as created; everything after arrives as events.
// Errors: ErrNotConfigured, ErrUnknownRepository, ErrBadBranch, and the
// store's workspace.ErrInProgress and workspace.ErrAtCap.
func (p *Provisioner) Create(ctx context.Context, repositoryID int64, branch string) (workspace.Workspace, error) {
	if p.Cloner == nil || p.Broker == nil {
		return workspace.Workspace{}, ErrNotConfigured
	}
	var defaultBranch string
	var removed bool
	err := p.Workspaces.DB.QueryRowContext(ctx,
		`SELECT default_branch, removed_at IS NOT NULL FROM repository WHERE id = ?`, repositoryID).
		Scan(&defaultBranch, &removed)
	switch {
	case errors.Is(err, sql.ErrNoRows) || (err == nil && removed):
		return workspace.Workspace{}, ErrUnknownRepository
	case err != nil:
		return workspace.Workspace{}, err
	}
	if branch == "" {
		branch = defaultBranch
	}
	if !clone.ValidBranch(branch) {
		return workspace.Workspace{}, ErrBadBranch
	}

	// The row and the in-flight mark are made under one lock, so Busy can
	// never see the row without the mark.
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return workspace.Workspace{}, ErrShuttingDown
	}
	w, err := p.Workspaces.Create(ctx, repositoryID, branch)
	if err != nil {
		return workspace.Workspace{}, err
	}
	p.launch(w.ID, workspace.StepAllocate)
	return w, nil
}

// Start runs a stopped or failed workspace again (POST
// /api/workspaces/{id}/start). It starts from resolving the config, because
// the clone survives a stop and a failed build — unless the clone never
// finished, in which case it starts from the clone. Errors:
// ErrNotConfigured, workspace.ErrNotFound, and workspace.ErrInProgress for a
// workspace that is running, mid-provision or being deleted, and
// workspace.ErrAtCap at the concurrent-container cap.
func (p *Provisioner) Start(ctx context.Context, id string) error {
	if p.Cloner == nil || p.Broker == nil {
		return ErrNotConfigured
	}
	w, err := p.Workspaces.Get(ctx, id)
	if err != nil {
		return err
	}
	first := workspace.StepResolveConfig
	if w.State == workspace.Failed {
		cloned, err := p.cloned(ctx, w)
		if err != nil {
			return err
		}
		if !cloned {
			first = workspace.StepClone
		}
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return ErrShuttingDown
	}
	if p.active[id] {
		return workspace.ErrInProgress
	}
	// Re-read under the lock: the state checked above may have moved.
	if w, err = p.Workspaces.Get(ctx, id); err != nil {
		return err
	}
	if w.State != workspace.Stopped && w.State != workspace.Failed {
		return workspace.ErrInProgress
	}
	// A start takes a container slot, so the cap applies as it does to a
	// create. The check and the move into the first step's state happen
	// here, under the lock Create also holds, so a create right after this
	// returns already counts this workspace — the run would otherwise make
	// the move a moment later, after the create had looked.
	n, err := p.Workspaces.Occupied(ctx)
	if err != nil {
		return err
	}
	if p.Workspaces.Cap > 0 && n >= p.Workspaces.Cap {
		return workspace.ErrAtCap
	}
	to := workspace.Building
	if first == workspace.StepClone {
		to = workspace.Cloning
	}
	if _, err := p.Workspaces.Move(ctx, id, to, ""); err != nil {
		return err
	}
	p.launch(id, first)
	return nil
}

// cloned reports whether a failed workspace's clone completed: its last
// clone step said done, and the directory is there. A clone interrupted by a
// restart leaves a directory without a done — and the clone step then
// refuses to reuse it rather than building from half a repository, because
// removing a directory is Delete's job alone.
func (p *Provisioner) cloned(ctx context.Context, w workspace.Workspace) (bool, error) {
	v, err := p.Workspaces.View(ctx, w.ID)
	if err != nil {
		return false, err
	}
	if v.Steps[workspace.StepClone].Status != "done" {
		return false, nil
	}
	return exists(w.HostPath)
}

func (p *Provisioner) launch(id string, first workspace.Step) { // p.mu held
	if p.active == nil {
		p.active, p.owned = map[string]bool{}, map[string]bool{}
	}
	if p.base == nil {
		p.base, p.stop = context.WithCancel(context.Background())
	}
	p.active[id], p.owned[id] = true, true
	p.wg.Add(1)
	go func() {
		defer p.wg.Done()
		defer func() {
			p.mu.Lock()
			delete(p.active, id)
			p.mu.Unlock()
		}()
		p.run(id, first)
	}()
}

// Shutdown stops starting runs, cancels the ones in flight — each fails the
// step it was on, saying Drydock shut down — and waits for them to write that
// down, for at most wait. A run still going after that is left to boot
// reconciliation, which marks it failed.
func (p *Provisioner) Shutdown(wait time.Duration) {
	p.mu.Lock()
	p.closed = true
	if p.stop != nil {
		p.stop()
	}
	p.mu.Unlock()
	done := make(chan struct{})
	go func() { p.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(wait):
	}
}

func (p *Provisioner) run(id string, first workspace.Step) {
	timeout := p.Timeout
	if timeout <= 0 {
		timeout = 30 * time.Minute
	}
	ctx, cancel := context.WithTimeout(p.base, timeout)
	defer cancel()
	// The steps run under ctx; the bookkeeping does not. When a step is
	// cancelled, the event saying so and the move to failed still have to
	// be written, and a cancelled context would refuse both.
	book := context.WithoutCancel(ctx)
	r := &runState{p: p}
	steps := map[workspace.Step]workspace.StepFunc{
		workspace.StepAllocate:         r.allocate,
		workspace.StepClone:            p.Cloner.Step,
		workspace.StepResolveConfig:    r.resolveConfig,
		workspace.StepCredentialVolume: r.credentialVolume,
		workspace.StepBrokerSocket:     r.brokerSocket,
		workspace.StepUp:               r.up,
		workspace.StepVerify:           r.verify,
		workspace.StepSessionServer:    r.sessionServer,
	}
	for st, f := range steps {
		steps[st] = guard(ctx, timeout, f)
	}
	if err := p.Workspaces.Provision(book, id, first, steps); err != nil {
		p.logf("drydock: workspace %s: %v", id, err)
	}
}

// guard runs a step under the run's context, and names an interruption as
// one: a step cut off by the timeout or by shutdown fails with a sentence
// saying which, rather than with whatever the cancelled subprocess printed.
// A step is not started at all once the run is cancelled.
func guard(ctx context.Context, timeout time.Duration, f workspace.StepFunc) workspace.StepFunc {
	return func(_ context.Context, w workspace.Workspace) error {
		if ctx.Err() != nil {
			return interrupted(ctx, timeout, ctx.Err())
		}
		err := f(ctx, w)
		if err != nil && ctx.Err() != nil {
			return interrupted(ctx, timeout, err)
		}
		return err
	}
}

func interrupted(ctx context.Context, timeout time.Duration, err error) error {
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return workspace.Public(fmt.Sprintf("Provisioning did not finish within %s, so Drydock stopped it.", timeout), err)
	}
	return workspace.Public("Drydock shut down while this step was running.", err)
}

// ghToken matches an installation token's shape, so a log line quoting one —
// a repository's postCreateCommand printing `gh auth token`, say — is
// redacted before it is written down (§13.5).
var ghToken = regexp.MustCompile(`gh[soupr]_[A-Za-z0-9]{20,}`)

// logTail writes the last lines of a subprocess's log to the service log,
// redacted. Never to the event log: it can quote anything a repository's own
// commands printed (§6).
func (p *Provisioner) logTail(id, what string, b []byte) {
	const max = 50
	lines := splitLines(b)
	if len(lines) > max {
		lines = lines[len(lines)-max:]
	}
	for _, l := range lines {
		p.logf("drydock: workspace %s: %s: %s", id, what, ghToken.ReplaceAllString(l, "[redacted]"))
	}
}

func splitLines(b []byte) []string {
	var out []string
	start := 0
	for i, c := range b {
		if c == '\n' {
			if i > start {
				out = append(out, string(b[start:i]))
			}
			start = i + 1
		}
	}
	if start < len(b) {
		out = append(out, string(b[start:]))
	}
	return out
}

// limitWriter keeps at most n bytes, never failing the writer.
type limitWriter struct {
	b []byte
	n int
}

func (l *limitWriter) Write(p []byte) (int, error) {
	if room := l.n - len(l.b); room > 0 {
		if len(p) > room {
			l.b = append(l.b, p[:room]...)
		} else {
			l.b = append(l.b, p...)
		}
	}
	return len(p), nil
}
