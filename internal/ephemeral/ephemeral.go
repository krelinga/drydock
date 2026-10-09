// Package ephemeral is how Drydock runs a short-lived helper container — the
// delete's cleanup helper, the credential volume's owner helper, the identity
// watch's reads, the login container, the docker guard's log probe — and how
// it makes sure none outlives its use.
//
// One rule, measured more than once: the subprocess lifecycle is not the Go
// lifecycle. A killed `docker run` or `docker create` client leaves its
// container behind, running or created, and a client killed during its create
// leaves the daemon to finish the create after the client is gone (on an idle
// Docker 29.8.2, 9 kills in 80 left a container that appeared 29–95 ms later).
// So every helper carries a label of its kind, `<prefix>.<kind>=<value>`, and:
//
//   - a label's containers are removed by that exact label whenever its first
//     holder in this process begins (what an earlier attempt left) and
//     whenever its last holder ends — on every exit path: success, failure,
//     a killed client, a cancelled context;
//   - the removal at the end runs under sys.Cleanup on the injected clock, so
//     a context that ended — the very thing that killed the client — never
//     stops it, and it still ends;
//   - when the client was killed and nothing lists yet, the end keeps
//     listing until Settle has passed, since the create may still be
//     landing; once something lists it is removed, being the one container
//     the client's one create could make;
//   - SweepAll, at boot, removes every kind this package knows, by this
//     prefix alone, sparing every label a holder in this process has begun
//     and not ended, and whatever the caller's skip spares.
//
// A helper never carries `<prefix>.workspace` (WorkspaceLabel). Reconciliation
// lists containers by that label and adopts or deletes what it finds, so a
// helper carrying it would be given a row; and the one thing a sweep must
// never remove is a workspace's container. Run and Create refuse an argv that
// names it, and SweepAll keeps anything that carries it, whatever else it
// carries.
//
// The kinds are an enumeration this package owns (Kinds): a helper added
// later is a Kind added here, and SweepAll sweeps it from then on — no sweep
// to remember to extend.
package ephemeral

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/krelinga/drydock/internal/subproc"
	"github.com/krelinga/drydock/internal/sys"
)

// Kind is what a helper is for. Its label key is `<prefix>.<kind>`.
type Kind string

const (
	// Cleanup is the delete's cleanup helper (internal/container), valued
	// with the workspace id.
	Cleanup Kind = "cleanup"
	// Identity is the identity watch's read (internal/identity), valued "1".
	Identity Kind = "identity"
	// Login is the login handshake's container (internal/login), valued
	// with the login id.
	Login Kind = "login"
	// LogProbe is the docker guard's probe of the daemon's log default
	// (internal/dockerguard), valued with the workspace id.
	LogProbe Kind = "log-probe"
	// VolumeOwner is the credential volume's owner helper
	// (internal/container), valued with the volume's name.
	VolumeOwner Kind = "volume-owner"
)

// Kinds is every kind there is. SweepAll lists each.
var Kinds = []Kind{Cleanup, Identity, Login, LogProbe, VolumeOwner}

// WorkspaceLabel is the label key, under the prefix, that reconciliation lists
// workspace containers by (internal/container's LabelWorkspace). No Kind is
// it, no helper carries it, and no sweep removes a container that does.
const WorkspaceLabel = "workspace"

func (k Kind) valid() bool {
	for _, x := range Kinds {
		if k == x {
			return true
		}
	}
	return false
}

// Defaults.
const (
	// DefaultSettle is how long an end keeps looking for a killed client's
	// container that does not list yet.
	DefaultSettle = 3 * time.Second
	// DefaultRemoveTimeout bounds an end's removal: its listings, its
	// settle and its rm.
	DefaultRemoveTimeout = 30 * time.Second
	// pollInterval is the end's interval while it settles.
	pollInterval = 50 * time.Millisecond
	// maxList bounds what a listing reads.
	maxList = 1 << 20
)

var (
	// ErrNotRun: Run or Create never started its command — the helper or
	// its argv was refused, or what an earlier attempt left could not be
	// listed or removed first.
	ErrNotRun = errors.New("ephemeral: the helper was not run")
	// ErrWorkspaceLabel: an argv named the workspace label.
	ErrWorkspaceLabel = errors.New("ephemeral: a helper never carries the workspace label")

	prefixPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)
	valuePattern  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)
	containerID   = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

// Key is a kind's label key under prefix: `<prefix>.<kind>`.
func Key(prefix string, k Kind) string { return prefix + "." + string(k) }

// Label is a helper's whole label, `<prefix>.<kind>=<value>`, as `--label`
// takes it. It refuses a kind this package does not know, and a prefix or
// value that could read as more than one label.
func Label(prefix string, k Kind, value string) (string, error) {
	if !k.valid() {
		return "", fmt.Errorf("ephemeral: %q is not a helper kind", k)
	}
	if !prefixPattern.MatchString(prefix) {
		return "", fmt.Errorf("ephemeral: %q is not a label prefix", prefix)
	}
	if !valuePattern.MatchString(value) {
		return "", fmt.Errorf("ephemeral: %q is not a label value", value)
	}
	return Key(prefix, k) + "=" + value, nil
}

// Helper is one helper container's label and how to remove what carries it.
// The zero values of the optional fields are the defaults.
type Helper struct {
	// Docker runs docker (subproc.Cmd{Name: "docker"}): subproc.Exec in production.
	Docker subproc.Runner
	// Prefix is config.LabelPrefix; Kind and Value make the label.
	Prefix string
	Kind   Kind
	Value  string
	// Clock is what Settle and RemoveTimeout are measured on; nil is the
	// real clock.
	Clock sys.Clock
	// Settle bounds an end's wait for a killed client's container; zero is
	// DefaultSettle. It must be shorter than RemoveTimeout, which bounds it.
	Settle time.Duration
	// RemoveTimeout bounds an end's removal; zero is DefaultRemoveTimeout.
	RemoveTimeout time.Duration
	// Registry is the process's set of holders; nil is Default.
	Registry *Registry
	// Logf is told about a removal that failed after a Run, which Run does
	// not return (the run's result stands, and boot's sweep removes what was
	// left); nil drops it.
	Logf func(string, ...any)
}

// Label is h's label, validated.
func (h Helper) Label() (string, error) { return Label(h.Prefix, h.Kind, h.Value) }

func (h Helper) clock() sys.Clock {
	if h.Clock == nil {
		return sys.RealClock{}
	}
	return h.Clock
}

func (h Helper) settle() time.Duration {
	if h.Settle > 0 {
		return h.Settle
	}
	return DefaultSettle
}

func (h Helper) removeTimeout() time.Duration {
	if h.RemoveTimeout > 0 {
		return h.RemoveTimeout
	}
	return DefaultRemoveTimeout
}

func (h Helper) registry() *Registry {
	if h.Registry == nil {
		return Default
	}
	return h.Registry
}

func (h Helper) check() (string, error) {
	label, err := h.Label()
	if err != nil {
		return "", err
	}
	if h.Docker == nil {
		return "", errors.New("ephemeral: no runner")
	}
	if h.settle() >= h.removeTimeout() {
		return "", fmt.Errorf("ephemeral: a settle of %s leaves no time in a removal bounded by %s", h.settle(), h.removeTimeout())
	}
	return label, nil
}

// checkArgs refuses an argv that does not carry label exactly once as a
// `--label` pair, or that names the workspace label anywhere.
func (h Helper) checkArgs(args []string, label string) error {
	ws := Key(h.Prefix, WorkspaceLabel)
	carries := 0
	for i, a := range args {
		v := strings.TrimPrefix(strings.TrimPrefix(a, "--label="), "-l=")
		if v == ws || strings.HasPrefix(v, ws+"=") || strings.Contains(a, ws+"=") {
			return fmt.Errorf("%w: %q", ErrWorkspaceLabel, a)
		}
		if strings.HasPrefix(a, "--label-file") {
			return fmt.Errorf("ephemeral: a helper's labels are its argv's: %q", a)
		}
		if a == "--label" && i+1 < len(args) && args[i+1] == label {
			carries++
		}
	}
	if carries != 1 {
		return fmt.Errorf("ephemeral: the argv carries the label %s %d times; want once", label, carries)
	}
	return nil
}

// Begin registers a holder of h's label: until its End, SweepAll spares the
// label. When it is the label's first holder in this process, whatever
// already carries the label — what an earlier attempt left — is removed first,
// under ctx; if that fails nothing is registered.
//
// Run and Create call it; a caller that starts its container some other way
// (the login's `docker run` on a PTY) calls it before the start, and End on
// every path after.
func (h Helper) Begin(ctx context.Context) error {
	label, err := h.check()
	if err != nil {
		return err
	}
	r := h.registry()
	e := r.acquire(label)
	defer r.release(label, e)
	e.turn.Lock()
	defer e.turn.Unlock()
	r.sweep.RLock()
	defer r.sweep.RUnlock()
	r.mu.Lock()
	first := e.holders == 0
	r.mu.Unlock()
	if first {
		ids, err := list(ctx, h.Docker, label)
		if err == nil {
			err = remove(ctx, h.Docker, ids)
		}
		if err != nil {
			return fmt.Errorf("removing what an earlier %s helper left: %w", h.Kind, err)
		}
	}
	r.mu.Lock()
	if first {
		e.killed, e.ids = false, nil
	}
	e.holders++
	r.mu.Unlock()
	return nil
}

// End is a holder done. When it is the label's last holder in this process,
// every container carrying the label — and any id Create was given for it —
// is removed under sys.Cleanup(ctx, Clock, RemoveTimeout): ctx's values, not
// its end. If this holder or any other since the first was killed — Drydock
// killed the docker client — and nothing lists yet, it keeps listing until
// Settle has passed, and removes what lists. It returns how many it removed.
//
// End without a Begin removes as a last holder would: it is safe to call on
// every path, whether or not the Begin got as far as registering.
func (h Helper) End(ctx context.Context, killed bool) (int, error) {
	label, err := h.check()
	if err != nil {
		return 0, err
	}
	r := h.registry()
	e := r.acquire(label)
	defer r.release(label, e)
	e.turn.Lock()
	defer e.turn.Unlock()
	r.sweep.RLock()
	defer r.sweep.RUnlock()
	r.mu.Lock()
	e.killed = e.killed || killed
	if e.holders > 0 {
		e.holders--
	}
	last := e.holders == 0
	known, settle := e.ids, e.killed
	if last {
		e.ids, e.killed = nil, false
	}
	r.mu.Unlock()
	if !last {
		return 0, nil
	}

	cctx, cancel := sys.Cleanup(ctx, h.clock(), h.removeTimeout())
	defer cancel()
	ids, err := h.await(cctx, label, settle)
	ids = union(ids, known)
	if rerr := remove(cctx, h.Docker, ids); rerr != nil {
		return 0, errors.Join(err, rerr)
	}
	return len(ids), err
}

// await lists the label, and while killed and nothing lists, again every
// pollInterval until Settle has passed.
func (h Helper) await(ctx context.Context, label string, killed bool) ([]string, error) {
	settled, stopSettle := sys.NewTimer(h.clock(), h.settle())
	defer stopSettle()
	over := false
	for {
		ids, err := list(ctx, h.Docker, label)
		if err != nil || len(ids) > 0 || !killed || over {
			return ids, err
		}
		poll, stopPoll := sys.NewTimer(h.clock(), pollInterval)
		select {
		case <-ctx.Done():
			stopPoll()
			return nil, ctx.Err()
		case <-settled:
			// One last look, then give up: what lands later goes at the
			// next Begin of the label, or at boot's sweep.
			over = true
		case <-poll:
		}
		stopPoll()
	}
}

// Run runs one docker command that makes a helper — a `docker run`, with
// --rm or not — carrying h's label, between a Begin and an End. The result
// is the command's; an error means it was never started (ErrNotRun). The
// End's own failure goes to Logf.
//
// The client counts as killed when it was still running as ctx ended, which
// is what makes the End wait out a create that may still land.
func (h Helper) Run(ctx context.Context, cmd subproc.Cmd) (subproc.Result, error) {
	label, err := h.check()
	if err == nil && cmd.Name != "docker" {
		err = fmt.Errorf("ephemeral: a helper is run by docker, not %q", cmd.Name)
	}
	if err == nil {
		err = h.checkArgs(cmd.Args, label)
	}
	if err == nil {
		err = h.Begin(ctx)
	}
	if err != nil {
		return subproc.Result{ExitCode: -1}, fmt.Errorf("%w: %w", ErrNotRun, err)
	}
	res := h.Docker.Run(ctx, cmd)
	if _, err := h.End(ctx, killedBy(ctx, res)); err != nil && h.Logf != nil {
		h.Logf("drydock: removing a %s helper (%s): %v", h.Kind, label, err)
	}
	return res, nil
}

// Create runs `docker create` (args are its whole argv, "create" first)
// for a helper carrying h's label, after a Begin, and returns the container's
// id. On success the caller owns the End, and must call it on every path;
// the id is removed then, whatever the listing says. On any failure the End
// has run, and the error says what failed. stderr, when set, gets docker's.
func (h Helper) Create(ctx context.Context, args []string, stderr io.Writer) (string, error) {
	label, err := h.check()
	if err == nil {
		if len(args) == 0 || args[0] != "create" {
			err = errors.New("ephemeral: Create runs docker create")
		} else {
			err = h.checkArgs(args, label)
		}
	}
	if err == nil {
		err = h.Begin(ctx)
	}
	if err != nil {
		return "", fmt.Errorf("%w: %w", ErrNotRun, err)
	}
	var out bytes.Buffer
	if stderr == nil {
		stderr = io.Discard
	}
	res := h.Docker.Run(ctx, subproc.Cmd{Name: "docker", Args: args, Stdout: &capped{buf: &out, max: 4 << 10}, Stderr: stderr})
	id := strings.TrimSpace(out.String())
	switch {
	case res.Err != nil:
		err = fmt.Errorf("docker create: %w", res.Err)
	case res.ExitCode != 0:
		err = fmt.Errorf("docker create exited %d", res.ExitCode)
	case !containerID.MatchString(id):
		err = fmt.Errorf("docker create: %q is not a container id", short(id))
	}
	if err != nil {
		if _, rerr := h.End(ctx, killedBy(ctx, res)); rerr != nil {
			err = errors.Join(err, fmt.Errorf("removing it: %w", rerr))
		}
		return "", err
	}
	h.registry().note(label, id)
	return id, nil
}

// killedBy: the command was still running when ctx ended, and was killed for
// it — subproc reports exactly that as an Err beside a context that is done.
func killedBy(ctx context.Context, res subproc.Result) bool {
	return res.Err != nil && ctx.Err() != nil
}

// Registry is a process's holders, by label. A sweep holds it whole, so no
// holder begins or ends while one runs; each label's begins and ends take
// turns. There is one per process, Default; a test makes its own to stand
// for a process that has just started.
type Registry struct {
	// mu guards labels and every entry's counts, ids and killed. It is
	// held only for those, never across a docker command.
	mu     sync.Mutex
	labels map[string]*entry
	// sweep: Begin and End read-lock it for their whole act (registering,
	// removing), a sweep write-locks it for its listing and removal, so a
	// sweep sees no holder half-way and spares exactly the registered ones.
	// Lock order: an entry's turn, then sweep, then mu; a sweep takes no
	// entry's turn.
	sweep sync.RWMutex
}

// Default is this process's Registry.
var Default = &Registry{}

type entry struct {
	// turn makes one label's Begins and Ends take turns: a first Begin's
	// clearing and a last End's removal never overlap another's.
	turn    sync.Mutex
	refs    int // goroutines using the entry
	holders int
	killed  bool
	ids     []string
}

func (r *Registry) acquire(label string) *entry {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.labels == nil {
		r.labels = map[string]*entry{}
	}
	e := r.labels[label]
	if e == nil {
		e = &entry{}
		r.labels[label] = e
	}
	e.refs++
	return e
}

// release drops a use, and the entry with its last use when it has no
// holder.
func (r *Registry) release(label string, e *entry) {
	r.mu.Lock()
	defer r.mu.Unlock()
	e.refs--
	if e.refs == 0 && e.holders == 0 {
		delete(r.labels, label)
	}
}

// note records an id Create was given for label, removed at its End.
func (r *Registry) note(label, id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if e := r.labels[label]; e != nil && e.holders > 0 {
		e.ids = append(e.ids, id)
	}
}

// held reports whether label has a holder.
func (r *Registry) held(label string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	e := r.labels[label]
	return e != nil && e.holders > 0
}

// Found is a helper container a sweep found.
type Found struct {
	ID    string
	Kind  Kind
	Value string
}

// SweepAll is boot's sweep, in Default: every kind, under prefix. See
// Registry.Sweep.
func SweepAll(ctx context.Context, run subproc.Runner, prefix string, skip func(Found) bool) ([]Found, error) {
	return Default.Sweep(ctx, run, prefix, Kinds, skip)
}

// Sweep removes every container, running or not, carrying `<prefix>.<kind>`
// for one of kinds, whatever its value — except:
//
//   - one that also carries `<prefix>.workspace`: no helper does, so it is a
//     workspace's container whatever else it carries, and reconciliation's;
//   - one whose exact label has a holder in r — a helper this process is
//     running now;
//   - one skip spares (nil spares nothing) — what this instance's jobs may
//     be running out of process, such as a log probe of a workspace with a
//     job in flight.
//
// Another prefix's helpers are never listed: the daemon filters by the
// prefixed key. As container.List does, `docker ps` for the ids, filtered on
// the daemon's side, then `docker inspect` for the labels — never a table
// parse; a container listed by a kind's key whose inspect lacks it is an
// error, since the contract moved and acting on it would be guessing. It
// returns what it removed.
func (r *Registry) Sweep(ctx context.Context, run subproc.Runner, prefix string, kinds []Kind, skip func(Found) bool) ([]Found, error) {
	if !prefixPattern.MatchString(prefix) {
		return nil, fmt.Errorf("ephemeral: %q is not a label prefix", prefix)
	}
	for _, k := range kinds {
		if !k.valid() {
			return nil, fmt.Errorf("ephemeral: %q is not a helper kind", k)
		}
	}
	r.sweep.Lock()
	defer r.sweep.Unlock()
	var listed []string
	seen := map[string]bool{}
	for _, k := range kinds {
		ids, err := list(ctx, run, Key(prefix, k))
		if err != nil {
			return nil, err
		}
		for _, id := range ids {
			if !seen[id] {
				seen[id] = true
				listed = append(listed, id)
			}
		}
	}
	if len(listed) == 0 {
		return nil, nil
	}
	var out, stderr bytes.Buffer
	res := run.Run(ctx, subproc.Cmd{Name: "docker", Args: append([]string{"inspect", "--type", "container"}, listed...),
		Stdout: &out, Stderr: &capped{buf: &stderr, max: 64 << 10}})
	if err := failed("docker inspect", res, &stderr); err != nil {
		return nil, err
	}
	var all []struct {
		ID     string `json:"Id"`
		Config struct {
			Labels map[string]string
		}
	}
	if err := json.Unmarshal(out.Bytes(), &all); err != nil {
		return nil, fmt.Errorf("docker inspect: %w", err)
	}
	var gone []Found
	var ids []string
	for _, c := range all {
		if !containerID.MatchString(c.ID) || !seen[c.ID] {
			return nil, fmt.Errorf("docker inspect: %q is not a container it was asked about", short(c.ID))
		}
		f := Found{ID: c.ID}
		for _, k := range kinds {
			if v, ok := c.Config.Labels[Key(prefix, k)]; ok {
				f.Kind, f.Value = k, v
				break
			}
		}
		if f.Kind == "" {
			return nil, fmt.Errorf("docker inspect: container %s carries none of the helper labels it was listed by", c.ID)
		}
		if _, ws := c.Config.Labels[Key(prefix, WorkspaceLabel)]; ws {
			continue
		}
		if r.held(Key(prefix, f.Kind) + "=" + f.Value) {
			continue
		}
		if skip != nil && skip(f) {
			continue
		}
		gone = append(gone, f)
		ids = append(ids, c.ID)
	}
	if err := remove(ctx, run, ids); err != nil {
		return nil, err
	}
	return gone, nil
}

// list is every container, running or not, the filter `label=<filter>`
// matches, by full id.
func list(ctx context.Context, run subproc.Runner, filter string) ([]string, error) {
	var out, stderr bytes.Buffer
	res := run.Run(ctx, subproc.Cmd{Name: "docker",
		Args:   []string{"ps", "--all", "--quiet", "--no-trunc", "--filter", "label=" + filter},
		Stdout: &capped{buf: &out, max: maxList}, Stderr: &capped{buf: &stderr, max: 64 << 10}})
	if err := failed("docker ps", res, &stderr); err != nil {
		return nil, err
	}
	ids := strings.Fields(out.String())
	for _, id := range ids {
		if !containerID.MatchString(id) {
			return nil, fmt.Errorf("docker ps: %q is not a container id", short(id))
		}
	}
	return ids, nil
}

// remove is `docker rm --force --volumes -- <ids>`, by full id only:
// --volumes takes a helper's anonymous volumes and never a named one.
func remove(ctx context.Context, run subproc.Runner, ids []string) error {
	if len(ids) == 0 {
		return nil
	}
	for _, id := range ids {
		if !containerID.MatchString(id) {
			return fmt.Errorf("docker rm: %q is not a container id", short(id))
		}
	}
	var stderr bytes.Buffer
	res := run.Run(ctx, subproc.Cmd{Name: "docker", Args: append([]string{"rm", "--force", "--volumes", "--"}, ids...),
		Stderr: &capped{buf: &stderr, max: 64 << 10}})
	return failed("docker rm", res, &stderr)
}

func union(a, b []string) []string {
	out := append([]string(nil), a...)
	for _, x := range b {
		found := false
		for _, y := range out {
			if x == y {
				found = true
				break
			}
		}
		if !found {
			out = append(out, x)
		}
	}
	return out
}

func failed(what string, res subproc.Result, stderr *bytes.Buffer) error {
	if res.Err != nil {
		return fmt.Errorf("%s: %w", what, res.Err)
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("%s exited %d: %s", what, res.ExitCode, short(strings.TrimSpace(stderr.String())))
	}
	return nil
}

func short(s string) string {
	if len(s) > 200 {
		return s[:200] + "…"
	}
	return s
}

// capped keeps the first max bytes and reports every write whole.
type capped struct {
	buf *bytes.Buffer
	max int
}

func (c *capped) Write(p []byte) (int, error) {
	if room := c.max - c.buf.Len(); room > 0 {
		if len(p) > room {
			c.buf.Write(p[:room])
		} else {
			c.buf.Write(p)
		}
	}
	return len(p), nil
}
