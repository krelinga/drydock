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
//     to write that (kill -9) leaves the workspace mid-provision — or running,
//     inside step 8 — with its step started, and boot reconciliation fails
//     the step and marks a mid-provision row failed (§6) — but reconciliation is also told
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
	"github.com/krelinga/drydock/internal/sys"
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
	// ErrDiskFull: the filesystem holding the workspaces is at or above
	// the configured limit (design §12, *Disk full*). Returned as a
	// *DiskFullError, which carries the figures the refusal names.
	ErrDiskFull = errors.New("provision: the workspace disk is above its limit")
)

// DiskFullError is the pre-flight's refusal, with what it measured.
type DiskFullError struct {
	UsedBytes, TotalBytes uint64
	LimitPercent          int
}

func (e *DiskFullError) Error() string {
	return fmt.Sprintf("provision: the workspace disk is %d%% full; the limit is %d%%", e.Percent(), e.LimitPercent)
}

func (e *DiskFullError) Is(target error) bool { return target == ErrDiskFull }

// Percent is used over total, rounded down — so a disk refused at a limit of
// 90 never reads as "89% full".
func (e *DiskFullError) Percent() int {
	if e.TotalBytes == 0 {
		return 0
	}
	return int(e.UsedBytes * 100 / e.TotalBytes)
}

// preflight is §12's disk check, run before a create, start or rebuild takes
// anything: a clone or an image build that runs out of disk fails minutes in,
// part-way, and leaves less room than it found. A disk that cannot be read
// is not refused — the check exists to save a doomed build, and it is not a
// control anything else relies on — but the reason is logged.
func (p *Provisioner) preflight() error {
	if p.Disk == nil {
		return nil
	}
	used, total, err := p.Disk.Usage(p.Workspaces.Root)
	if err != nil {
		p.logf("drydock: disk pre-flight: %v", err)
		return nil
	}
	if workspace.OverLimit(used, total, p.DiskLimitPercent) {
		return &DiskFullError{UsedBytes: used, TotalBytes: total, LimitPercent: p.DiskLimitPercent}
	}
	return nil
}

// Broker is what provisioning needs from internal/broker: step 5 opens the
// socket, up mounts its directory, stop closes it, and delete removes it
// with its directory.
type Broker interface {
	Open(ctx context.Context, workspaceID string) error
	Close(workspaceID string) error
	Remove(workspaceID string) error
	SocketDir(workspaceID string) string
}

// LegacyMountSentence is what a workspace whose container an earlier Drydock
// created says: its broker socket is mounted as a file, which has named a
// dead socket since the restart that brought this version in, and only a
// rebuild gives the container the directory mount (container.UpSpec.BrokerDir).
const LegacyMountSentence = "This workspace's container was created by an earlier Drydock, " +
	"whose GitHub access mount does not survive a restart, so git, gh and every command's secrets fail in it. " +
	"Rebuild it once; the clone is kept."

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
	// ClaudeVolume is the shared Claude credential volume (config.ClaudeVolume,
	// §7.1): step 4 makes it if absent, and up mounts it at the Feature's
	// CLAUDE_CONFIG_DIR. Empty fails step 4 rather than giving a workspace a
	// login of its own.
	ClaudeVolume string
	// ClaudeCodeVersion is the Claude Code version step 7 requires `claude
	// --version` to report: classify.ClaudeCodeVersion, the version whose
	// output the classifiers were recorded against, which the server also
	// passes as the Feature's claudeCodeVersion option. Empty skips the
	// comparison but not the check that claude runs.
	ClaudeCodeVersion string
	// RemoteEnv is added to every workspace's --remote-env, beside
	// DRYDOCK_WORKSPACE and DRYDOCK_REPO. Empty in production; a test sets
	// DRYDOCK_GITHUB_HOST to point the Feature's helper at a fake.
	RemoteEnv map[string]string
	// Config is the minimal devcontainer.json for a repository without one.
	// Nil means DefaultConfig. A test adds runArgs to it, which then needs
	// a host-access approval like a repository's own (§6).
	Config []byte
	// Timeout bounds one run (config.ProvisionTimeout).
	Timeout time.Duration
	// Disk and DiskLimitPercent are the pre-flight check (preflight): a
	// create, start or rebuild is refused while the filesystem holding the
	// workspace root is at least DiskLimitPercent full. Nil Disk skips it.
	Disk             sys.DiskUsage
	DiskLimitPercent int
	// Logf receives each failed run's full error — the half that never
	// reaches the event log (§6). Nil discards it.
	Logf func(format string, args ...any)
	// StopSupervisor is Phase 5's seam: stop the workspace's
	// `claude remote-control` server with SIGTERM, escalating to SIGKILL
	// only on timeout (CLAUDE.md) — before its container is stopped,
	// rebuilt or removed, because a server killed with its container blocks
	// the next start for minutes (Spike 02). Nil until the supervisor
	// exists; the sub-step then records that it had nothing to do.
	StopSupervisor func(ctx context.Context, w workspace.Workspace) error
	// StartSupervisor is §6 step 8: hand the running workspace to the
	// session supervisor (internal/supervisor, §8), which starts its
	// `claude remote-control` server and carries its own states from there.
	// Nil records the step as a no-op. ForgetSupervisor drops what the
	// supervisor holds for a workspace once its delete has finished.
	StartSupervisor  func(ctx context.Context, w workspace.Workspace) error
	ForgetSupervisor func(id string)
	// ParkSupervisor records, in place of StartSupervisor at boot, that a
	// running workspace's session server cannot work until its container is
	// rebuilt, with Drydock's sentence saying so: the container has the
	// broker socket mounted as a file, as an earlier Drydock made it
	// (container.Manager.LegacyBrokerMount). Nil starts it as usual.
	ParkSupervisor func(ctx context.Context, w workspace.Workspace, detail string) error
	// SupervisorRestart stops (SIGTERM first) and starts a workspace's
	// session server: the job RestartSupervisor runs. Nil refuses the
	// route with ErrNoSupervisor.
	SupervisorRestart func(ctx context.Context, id string) error

	// Redact returns the values a workspace's held build log must not show:
	// the secrets granted to its repository (the server's secretValues,
	// as the session server's log uses). Nil masks only GitHub tokens.
	Redact func(ctx context.Context, id string) []string

	// afterStep, in a test, runs after each stop or delete sub-step
	// finishes; an error it returns ends the job there, as a crash between
	// sub-steps would.
	afterStep func(action, step string) error

	mu     sync.Mutex
	active map[string]*job // jobs in flight: a run, a stop or a delete
	// buildLogs holds each workspace's latest failed `up` (messages.go).
	buildLogs map[string]BuildLog
	owned     map[string]bool // every workspace a job was started for
	base      context.Context
	stop      context.CancelFunc
	closed    bool
	wg        sync.WaitGroup
}

func (p *Provisioner) logf(format string, args ...any) {
	if p.Logf != nil {
		p.Logf(format, args...)
	}
}

// job is one background operation on a workspace. There is at most one per
// workspace: a run, a stop, or a delete. A delete is the only one that may
// replace another — it cancels the job in flight and waits for it to end.
type job struct {
	kind   string // "run", "stop", "delete"
	cancel context.CancelCauseFunc
	done   chan struct{}
	err    error // set before done closes
}

// errDeleting is the cause a delete cancels an in-flight job with.
var errDeleting = errors.New("provision: the workspace is being deleted")

// Owns reports whether this process has started a job for the workspace,
// finished or not. It is reconciliation's Busy: boot reconciliation is about
// state left by an earlier process, and a workspace this one provisioned is
// in whatever state its own run left it — even if it is still pending, or
// finished in the moment between reconciliation's listing and its action.
func (p *Provisioner) Owns(id string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.owned[id]
}

// Unowned runs act, holding the lock every job starts under, only if this
// process has started no job for the workspace (and is not shutting down),
// and reports whether it ran. It is boot reconciliation's guard, and it is
// the check and the act in one: reconciliation runs beside serving, so with
// Owns checked first and the act after, a stop, start or delete asked in
// between started a job that reconciliation's plan — made from rows read
// before it — then acted against, marking stopped a workspace the stop was
// moving, or writing a stale container id over the one a start just set.
// Under the lock, no job can start during act, and none started unseen
// before it; a delete asked meanwhile waits for the lock and then cancels
// nothing, since act is not a job.
//
// act must not take p.mu — no Stop, Start, Delete or ResumeDelete inside it.
// Reconciliation calls ResumeDelete outside, which is safe: a deleting row's
// only job is its delete, and startDelete is atomic itself.
func (p *Provisioner) Unowned(id string, act func() error) (bool, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed || p.owned[id] {
		return false, nil
	}
	return true, act()
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
	// The cheaper, truer refusal first: a repository that already has a
	// workspace is in_progress whatever the disk says. Workspaces.Create
	// checks it again inside its transaction; this read only orders the
	// two refusals.
	var held bool
	if err := p.Workspaces.DB.QueryRowContext(ctx,
		`SELECT EXISTS (SELECT 1 FROM workspace WHERE repository_id = ?)`, repositoryID).Scan(&held); err != nil {
		return workspace.Workspace{}, err
	}
	if held {
		return workspace.Workspace{}, workspace.ErrInProgress
	}
	if err := p.preflight(); err != nil {
		return workspace.Workspace{}, err
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
	p.launch(w.ID, "run", func(ctx context.Context) error {
		return p.run(ctx, w.ID, workspace.StepAllocate, false)
	})
	return w, nil
}

// Start runs a stopped or failed workspace again (POST
// /api/workspaces/{id}/start). It starts from resolving the config, because
// the clone survives a stop and a failed build — unless the clone never
// finished, in which case it starts from the clone. Errors:
// ErrNotConfigured, workspace.ErrNotFound, and workspace.ErrInProgress for a
// workspace that is running, mid-provision, busy or being deleted, and
// workspace.ErrAtCap at the concurrent-container cap.
//
// A start from failed passes --remove-existing-container, as a rebuild does:
// a failed workspace's container, if it has one, is one whose `up` or probe
// did not succeed — a failed postCreateCommand leaves it running (§6) — and
// `up` without the flag reattaches to it and reports success. A start from
// stopped reattaches, which is what makes it cheap.
func (p *Provisioner) Start(ctx context.Context, id string) error {
	return p.restart(ctx, id, false)
}

// Rebuild is POST /api/workspaces/{id}/rebuild: the same run as a start,
// from the same step, with --remove-existing-container — without it `up`
// with an existing id-label reattaches and silently is not a rebuild (§6).
// The clone survives. A running, stopped or failed workspace can be rebuilt;
// one mid-provision, busy or being deleted is workspace.ErrInProgress. A
// running workspace already holds a slot, so only a stopped or failed one is
// refused at the cap.
func (p *Provisioner) Rebuild(ctx context.Context, id string) error {
	return p.restart(ctx, id, true)
}

func (p *Provisioner) restart(ctx context.Context, id string, rebuild bool) error {
	return p.restartWith(ctx, id, rebuild, nil)
}

// ApproveConfig is POST /api/workspaces/{id}/config-approval: the operator
// approves the host-access request a stopped workspace is waiting on, and
// the run it stopped continues — from step 3, with the stopped run's
// --remove-existing-container — exactly as a start would, under the same
// lock, refusals and cap. hash is the request's hash as the operator was
// shown it: a different one is workspace.ErrApprovalStale, so a configuration
// that changed between the display and the click is never approved. by is the
// approving session's id. Step 3 then reads the configuration again and runs
// it only if its subset still hashes to what was approved; if a Feature's tag
// moved in between, it asks again. Errors: ErrNotConfigured,
// workspace.ErrNotFound, workspace.ErrNoApproval, workspace.ErrApprovalStale,
// workspace.ErrInProgress, workspace.ErrAtCap.
func (p *Provisioner) ApproveConfig(ctx context.Context, id, hash, by string) error {
	return p.restartWith(ctx, id, false, &approval{hash: hash, by: by})
}

// DeclineConfig is DELETE /api/workspaces/{id}/config-approval: the request
// is dropped and the workspace stays stopped; nothing is recorded for the
// repository, so the next start asks again. Errors: workspace.ErrNotFound,
// workspace.ErrNoApproval, workspace.ErrInProgress.
func (p *Provisioner) DeclineConfig(ctx context.Context, id string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.active[id] != nil {
		return workspace.ErrInProgress
	}
	return p.Workspaces.Decline(ctx, id)
}

type approval struct{ hash, by string }

func (p *Provisioner) restartWith(ctx context.Context, id string, rebuild bool, ap *approval) error {
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
	if p.active[id] != nil {
		return workspace.ErrInProgress
	}
	// Re-read under the lock: the state checked above may have moved.
	if w, err = p.Workspaces.Get(ctx, id); err != nil {
		return err
	}
	var pending workspace.PendingApproval
	if ap != nil {
		// Checked before the cap, and recorded after it: an approval that
		// cannot run now is not recorded, and the operator approves again.
		_, pp, err := p.Workspaces.Pending(ctx, id)
		if err != nil {
			return err
		}
		if ap.hash == "" || ap.hash != pp.Hash {
			return workspace.ErrApprovalStale
		}
		pending = pp
	}
	switch {
	case w.State == workspace.Stopped || w.State == workspace.Failed:
	case rebuild && w.State == workspace.Running:
	default:
		return workspace.ErrInProgress
	}
	// The disk after the cheap refusals, so a start already running is
	// in_progress rather than disk_full.
	if err := p.preflight(); err != nil {
		return err
	}
	// A start takes a container slot, so the cap applies as it does to a
	// create. The check and the move into the first step's state happen
	// here, under the lock Create also holds, so a create right after this
	// returns already counts this workspace — the run would otherwise make
	// the move a moment later, after the create had looked. A running
	// workspace being rebuilt holds its slot already.
	if !workspace.Occupying(w.State) {
		n, err := p.Workspaces.Occupied(ctx)
		if err != nil {
			return err
		}
		if p.Workspaces.Cap > 0 && n >= p.Workspaces.Cap {
			return workspace.ErrAtCap
		}
	}
	removeExisting := rebuild || w.State == workspace.Failed
	if ap != nil {
		if _, err := p.Workspaces.Approve(ctx, id, ap.hash, ap.by); err != nil {
			return err
		}
		removeExisting = pending.RemoveExisting
	}
	wasRunning := w.State == workspace.Running
	to := workspace.Building
	if first == workspace.StepClone {
		to = workspace.Cloning
	}
	if _, err := p.Workspaces.Move(ctx, id, to, ""); err != nil {
		return err
	}
	p.launch(id, "run", func(ctx context.Context) error {
		if wasRunning && p.StopSupervisor != nil {
			// Phase 5: the server goes before its container does.
			if err := p.StopSupervisor(ctx, w); err != nil {
				p.logf("drydock: workspace %s: stopping the session server before a rebuild: %v", id, err)
			}
		}
		return p.run(ctx, id, first, removeExisting)
	})
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

// launch starts a job for the workspace in the background (p.mu held). Its
// context ends at Drydock's shutdown, or when a delete cancels it.
func (p *Provisioner) launch(id, kind string, f func(ctx context.Context) error) *job {
	if p.active == nil {
		p.active, p.owned = map[string]*job{}, map[string]bool{}
	}
	if p.base == nil {
		p.base, p.stop = context.WithCancel(context.Background())
	}
	ctx, cancel := context.WithCancelCause(p.base)
	j := &job{kind: kind, cancel: cancel, done: make(chan struct{})}
	p.active[id], p.owned[id] = j, true
	p.wg.Add(1)
	go func() {
		defer p.wg.Done()
		defer close(j.done)
		defer cancel(nil)
		j.err = f(ctx)
		p.mu.Lock()
		if p.active[id] == j { // a delete may have replaced it
			delete(p.active, id)
		}
		p.mu.Unlock()
	}()
	return j
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

func (p *Provisioner) run(parent context.Context, id string, first workspace.Step, removeExisting bool) error {
	timeout := p.Timeout
	if timeout <= 0 {
		timeout = 30 * time.Minute
	}
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()
	// The steps run under ctx; the bookkeeping does not. When a step is
	// cancelled, the event saying so and the move to failed still have to
	// be written, and a cancelled context would refuse both.
	book := context.WithoutCancel(ctx)
	r := &runState{p: p, removeExisting: removeExisting}
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
	err := p.Workspaces.Provision(book, id, first, steps)
	if errors.Is(err, workspace.ErrNeedsApproval) {
		// Not a failure: the workspace is stopped, waiting for the
		// operator, and a stopped workspace has no socket.
		p.closeIfFailed(book, id)
		return nil
	}
	if err != nil {
		p.logf("drydock: workspace %s: %v", id, err)
		p.closeIfFailed(book, id)
	}
	return err
}

func (p *Provisioner) redactions(ctx context.Context, id string) []string {
	if p.Redact == nil {
		return nil
	}
	return p.Redact(ctx, id)
}

// closeIfFailed closes a failed workspace's broker socket. GitHub access
// follows Drydock's state, not Docker's (§9.1): a failed workspace may still
// have a running container — a failed postCreateCommand leaves one up (§6) —
// but Drydock has not handed it over as working, and after a restart boot
// would not reopen its socket either, since only running workspaces get one.
// Closing it here makes a live failure and a rebooted one the same. Start and
// rebuild run step 5 again, which reopens it. A run cut off by a delete is
// left alone: the workspace is deleting, and the delete closes the socket in
// its own sub-step. A run that stopped for a host-access approval leaves the
// workspace stopped, and a stopped workspace has no socket either.
func (p *Provisioner) closeIfFailed(ctx context.Context, id string) {
	if p.Broker == nil {
		return
	}
	w, err := p.Workspaces.Get(ctx, id)
	if err != nil || (w.State != workspace.Failed && w.State != workspace.Stopped) {
		return
	}
	if err := p.Broker.Close(id); err != nil {
		p.logf("drydock: workspace %s: closing the broker socket after a failed run: %v", id, err)
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
	if errors.Is(context.Cause(ctx), errDeleting) {
		return workspace.Public("The workspace is being deleted, so Drydock stopped this step.", err)
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
