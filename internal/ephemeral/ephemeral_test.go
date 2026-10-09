package ephemeral

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/krelinga/drydock/internal/subproc"
	"github.com/krelinga/drydock/internal/sys"
)

const prefix = "drydock.test"

// daemon is a docker stand-in holding containers and their labels in memory:
// ps filters by a label key or key=value as the daemon does, inspect prints
// the labels as JSON, rm removes, and run and create make a container with
// the --label pairs of their argv unless hook says otherwise. A command whose
// context is already done is refused unrun, as os/exec refuses to start one:
// that is how a removal run under an expired context shows up here.
type daemon struct {
	mu    sync.Mutex
	ctrs  map[string]map[string]string
	order []string
	calls [][]string
	n     int
	// beforeInspect, when set, runs as an inspect starts: a container going
	// between a listing and an inspect.
	beforeInspect func()
	// hook, when set, is run and create: it may make containers through add
	// and returns the command's result.
	hook func(ctx context.Context, args []string, out io.Writer) subproc.Result
}

func newDaemon() *daemon { return &daemon{ctrs: map[string]map[string]string{}} }

func (d *daemon) add(labels ...string) string {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.n++
	id := fmt.Sprintf("%064x", d.n)
	m := map[string]string{}
	for _, l := range labels {
		k, v, _ := strings.Cut(l, "=")
		m[k] = v
	}
	d.ctrs[id] = m
	d.order = append(d.order, id)
	return id
}

func (d *daemon) last() string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.order[len(d.order)-1]
}

func (d *daemon) exists(id string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.ctrs[id] != nil
}

func (d *daemon) count(sub string) int {
	d.mu.Lock()
	defer d.mu.Unlock()
	n := 0
	for _, c := range d.calls {
		if c[0] == sub {
			n++
		}
	}
	return n
}

func labelsOf(args []string) []string {
	var out []string
	for i, a := range args {
		if a == "--label" && i+1 < len(args) {
			out = append(out, args[i+1])
		}
	}
	return out
}

func (d *daemon) Run(ctx context.Context, c subproc.Cmd) subproc.Result {
	d.mu.Lock()
	d.calls = append(d.calls, append([]string(nil), c.Args...))
	d.mu.Unlock()
	if ctx.Err() != nil {
		return subproc.Result{ExitCode: -1, Err: ctx.Err()}
	}
	out := c.Stdout
	if out == nil {
		out = io.Discard
	}
	args := c.Args
	switch args[0] {
	case "ps":
		if id, ok := strings.CutPrefix(args[len(args)-1], "id="); ok {
			if d.exists(id) {
				fmt.Fprintln(out, id)
			}
			return subproc.Result{}
		}
		f := strings.TrimPrefix(args[len(args)-1], "label=")
		k, v, exact := strings.Cut(f, "=")
		d.mu.Lock()
		for _, id := range d.order {
			l := d.ctrs[id]
			if l == nil {
				continue
			}
			if got, ok := l[k]; ok && (!exact || got == v) {
				fmt.Fprintln(out, id)
			}
		}
		d.mu.Unlock()
	case "inspect":
		// As docker: the ones it found on stdout, and exit 1 if any was
		// missing.
		if d.beforeInspect != nil {
			d.beforeInspect()
		}
		type c struct {
			ID     string `json:"Id"`
			Config struct{ Labels map[string]string }
		}
		all := []c{}
		missing := false
		d.mu.Lock()
		for _, id := range args[3:] {
			if l := d.ctrs[id]; l != nil {
				x := c{ID: id}
				x.Config.Labels = l
				all = append(all, x)
			} else {
				missing = true
			}
		}
		d.mu.Unlock()
		json.NewEncoder(out).Encode(all)
		if missing {
			return subproc.Result{ExitCode: 1}
		}
	case "rm":
		d.mu.Lock()
		for _, id := range args[4:] {
			delete(d.ctrs, id)
		}
		d.mu.Unlock()
	case "run", "create":
		if d.hook != nil {
			return d.hook(ctx, args, out)
		}
		id := d.add(labelsOf(args)...)
		if args[0] == "create" {
			fmt.Fprintln(out, id)
		}
	}
	return subproc.Result{}
}

func (d *daemon) Start(context.Context, subproc.Cmd) (subproc.Process, error) {
	return nil, errors.New("unused")
}

func helper(d *daemon, k Kind, v string) Helper {
	return Helper{Docker: d, Prefix: prefix, Kind: k, Value: v, Registry: &Registry{},
		Settle: 500 * time.Millisecond, RemoveTimeout: 5 * time.Second}
}

func runArgs(h Helper, extra ...string) []string {
	l, _ := h.Label()
	return append([]string{"run", "--rm", "--label", l, "--network", "none"}, extra...)
}

// TestRunRemovesOnEveryExit: whatever way the command ends — success, a
// failure, a context cancelled under it, a client killed whose create the
// daemon finishes after it is gone — nothing carrying the label is left, and
// the removal ran although the caller's context had ended (the daemon refuses
// a command under a done context, as exec does). Each case's container is one
// the command left on purpose: --rm is the daemon's and is not relied on.
func TestRunRemovesOnEveryExit(t *testing.T) {
	cases := map[string]struct {
		cancel bool
		hook   func(d *daemon, label string) func(ctx context.Context, args []string, out io.Writer) subproc.Result
	}{
		"success": {hook: func(d *daemon, label string) func(context.Context, []string, io.Writer) subproc.Result {
			return func(context.Context, []string, io.Writer) subproc.Result { d.add(label); return subproc.Result{} }
		}},
		"failure": {hook: func(d *daemon, label string) func(context.Context, []string, io.Writer) subproc.Result {
			return func(context.Context, []string, io.Writer) subproc.Result {
				d.add(label)
				return subproc.Result{ExitCode: 125}
			}
		}},
		"cancel": {cancel: true, hook: func(d *daemon, label string) func(context.Context, []string, io.Writer) subproc.Result {
			return func(ctx context.Context, _ []string, _ io.Writer) subproc.Result {
				d.add(label)
				<-ctx.Done()
				return subproc.Result{ExitCode: -1, Err: ctx.Err()}
			}
		}},
		"killed, the create landing late": {cancel: true, hook: func(d *daemon, label string) func(context.Context, []string, io.Writer) subproc.Result {
			return func(ctx context.Context, _ []string, _ io.Writer) subproc.Result {
				<-ctx.Done()
				go func() { time.Sleep(150 * time.Millisecond); d.add(label) }()
				return subproc.Result{ExitCode: -1, Err: ctx.Err()}
			}
		}},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			d := newDaemon()
			h := helper(d, Cleanup, "01JABCDEFGHJKMNPQRSTVWXYZ0")
			label, _ := h.Label()
			d.hook = c.hook(d, label)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if c.cancel {
				go func() { time.Sleep(50 * time.Millisecond); cancel() }()
			}
			if _, err := h.Run(ctx, subproc.Cmd{Name: "docker", Args: runArgs(h)}); err != nil {
				t.Fatal(err)
			}
			var left []string
			out := &strings.Builder{}
			d.Run(context.Background(), subproc.Cmd{Args: []string{"ps", "--filter", "label=" + label}, Stdout: out})
			left = strings.Fields(out.String())
			if len(left) != 0 {
				t.Errorf("left %q carrying %s", left, label)
			}
			if d.count("rm") == 0 {
				t.Error("control: nothing was ever removed")
			}
			if h.Registry.held(label) {
				t.Error("the label is still held after Run")
			}
		})
	}
}

// TestEndSettlesForAKilledCreate: an End told the client was killed keeps
// listing until the late container lists, and removes it; the control, told
// the client exited by itself, lists once and leaves what lands later to the
// next Begin, which clears it before anything runs.
func TestEndSettlesForAKilledCreate(t *testing.T) {
	for _, killed := range []bool{true, false} {
		d := newDaemon()
		h := helper(d, Login, "0123456789abcdef01234567")
		label, _ := h.Label()
		lease, err := h.Begin(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		go func() { time.Sleep(150 * time.Millisecond); d.add(label) }()
		n, err := lease.End(context.Background(), killed)
		if err != nil {
			t.Fatal(err)
		}
		if killed && (n != 1 || d.count("ps") < 3) {
			t.Errorf("killed: removed %d after %d listings; want the late create, listed again", n, d.count("ps"))
		}
		if !killed {
			if n != 0 || d.count("ps") != 2 {
				t.Errorf("exited: removed %d after %d listings; want one look at the end", n, d.count("ps"))
			}
			time.Sleep(300 * time.Millisecond)
			id := d.last()
			if !d.exists(id) {
				t.Fatal("control: the late create never landed")
			}
			// The next holder's Begin clears it.
			next, err := h.Begin(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if d.exists(id) {
				t.Error("Begin did not clear what an earlier holder left")
			}
			next.End(context.Background(), false)
		}
	}

	// Killed and nothing ever lands: it gives up once Settle has passed,
	// on the injected clock, having removed nothing.
	clock := sys.NewFakeClock(time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC))
	d := newDaemon()
	h := helper(d, Login, "0123456789abcdef01234567")
	h.Clock = clock
	h.Settle = time.Second
	done := make(chan error, 1)
	go func() { _, err := h.Remove(context.Background(), true); done <- err }()
	for deadline := time.Now().Add(5 * time.Second); clock.Waiting() < 3; time.Sleep(time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("End never set its bound, its settle and a poll")
		}
	}
	clock.Advance(time.Second - time.Millisecond)
	select {
	case err := <-done:
		t.Fatalf("End gave up before Settle: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	clock.Advance(time.Millisecond)
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("End waited past Settle")
	}
}

// TestCreateRemovesItsIDWhateverTheListing: the id Create was given is
// removed at the End even when the listing does not show it, and a create
// that failed has already been ended.
func TestCreateRemovesItsIDWhateverTheListing(t *testing.T) {
	d := newDaemon()
	h := helper(d, LogProbe, "01JABCDEFGHJKMNPQRSTVWXYZ0")
	label, _ := h.Label()
	hidden := ""
	d.hook = func(_ context.Context, args []string, out io.Writer) subproc.Result {
		hidden = d.add("unlisted=1") // a container the label filter will not find
		fmt.Fprintln(out, hidden)
		return subproc.Result{}
	}
	id, lease, err := h.Create(context.Background(), []string{"create", "--label", label, "--network", "none", "img"}, nil)
	if err != nil || id != hidden {
		t.Fatalf("create: %q %v", id, err)
	}
	if _, err := lease.End(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	if d.exists(hidden) {
		t.Error("the created id outlived its End")
	}

	d.hook = func(context.Context, []string, io.Writer) subproc.Result {
		d.add(label)
		return subproc.Result{ExitCode: 1}
	}
	if _, _, err := h.Create(context.Background(), []string{"create", "--label", label, "img"}, nil); err == nil {
		t.Fatal("a failed create was not an error")
	}
	out := &strings.Builder{}
	d.Run(context.Background(), subproc.Cmd{Args: []string{"ps", "--filter", "label=" + label}, Stdout: out})
	if out.Len() != 0 || h.Registry.held(label) {
		t.Errorf("a failed create left %q, held %v", out.String(), h.Registry.held(label))
	}
}

// TestASharedLabelIsRemovedByItsLastHolder: two holders of one label (two
// creates' owner helpers for one volume): the first to end removes nothing,
// so the other's container survives; the last removes it.
func TestASharedLabelIsRemovedByItsLastHolder(t *testing.T) {
	d := newDaemon()
	h := helper(d, VolumeOwner, "drydock-claude-config")
	label, _ := h.Label()
	ctx := context.Background()
	a, err := h.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	b, err := h.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	other := d.add(label)
	if n, err := a.End(ctx, false); err != nil || n != 0 || !d.exists(other) {
		t.Fatalf("first end: %d %v; the other holder's container exists %v", n, err, d.exists(other))
	}
	// An End with no hold of its own — the first lease ended again, or a
	// Remove with no Begin — must not end the other holder's hold.
	if n, err := a.End(ctx, false); err != nil || n != 0 || !d.exists(other) || !h.Registry.held(label) {
		t.Fatalf("a second End of one lease: %d %v; exists %v, held %v", n, err, d.exists(other), h.Registry.held(label))
	}
	if n, err := h.Remove(ctx, false); err != nil || n != 0 || !d.exists(other) || !h.Registry.held(label) {
		t.Fatalf("a Remove while held: %d %v; exists %v, held %v", n, err, d.exists(other), h.Registry.held(label))
	}
	if n, err := b.End(ctx, false); err != nil || n != 1 || d.exists(other) {
		t.Errorf("last end: %d %v; exists %v", n, err, d.exists(other))
	}
}

// TestAHelperNeverCarriesTheWorkspaceLabel: no kind is the workspace label,
// a kind outside the enumeration is refused, and an argv naming the
// workspace label in any form is refused unrun — the control is the same
// argv without it, which runs.
func TestAHelperNeverCarriesTheWorkspaceLabel(t *testing.T) {
	for _, k := range Kinds {
		l, err := Label(prefix, k, "x1")
		if err != nil {
			t.Fatal(err)
		}
		if strings.HasPrefix(l, prefix+"."+WorkspaceLabel) {
			t.Errorf("kind %s makes the label %s", k, l)
		}
	}
	for _, k := range []Kind{WorkspaceLabel, "repo", ""} {
		if _, err := Label(prefix, k, "x1"); err == nil {
			t.Errorf("kind %q was accepted", k)
		}
	}
	for _, v := range []string{"", "a=b", "a,b", "a\nb", "-x"} {
		if _, err := Label(prefix, Cleanup, v); err == nil {
			t.Errorf("value %q was accepted", v)
		}
	}
	ws := prefix + "." + WorkspaceLabel
	for _, bad := range [][]string{
		{"--label", ws + "=01JABCDEFGHJKMNPQRSTVWXYZ0"},
		{"--label=" + ws + "=x"},
		{"-l", ws + "=x"},
		{"--label", ws},
		{"-l" + ws},
		{"-tl" + ws},
		{"-tl" + ws + "=x"},
		{"-l", ws},
		{"--label-file", "/tmp/labels"},
	} {
		d := newDaemon()
		h := helper(d, Cleanup, "01JABCDEFGHJKMNPQRSTVWXYZ0")
		_, err := h.Run(context.Background(), subproc.Cmd{Name: "docker", Args: runArgs(h, bad...)})
		if !errors.Is(err, ErrNotRun) || len(d.calls) != 0 {
			t.Errorf("%q: %v, %d docker calls", bad, err, len(d.calls))
		}
	}
	d := newDaemon()
	h := helper(d, Cleanup, "01JABCDEFGHJKMNPQRSTVWXYZ0")
	// The key's own text inside a longer name is not the key.
	if _, err := h.Run(context.Background(), subproc.Cmd{Name: "docker", Args: runArgs(h, "--network", ws+"-net", "--hostname", ws+"s")}); err != nil || d.count("run") != 1 {
		t.Errorf("control: %v, %d runs", err, d.count("run"))
	}
	// An argv without the helper's own label is refused too: its removal
	// would find nothing.
	if _, err := h.Run(context.Background(), subproc.Cmd{Name: "docker", Args: []string{"run", "--rm", "img"}}); !errors.Is(err, ErrNotRun) {
		t.Errorf("an argv without the label: %v", err)
	}
}

// TestSweepAll: boot's sweep removes a leftover of every kind under this
// prefix, and keeps another prefix's of every kind, a container carrying a
// helper label and the workspace label, a helper whose label a holder in
// this process has begun, and what skip spares. The controls are each kept
// container's own kind's leftover, removed beside it.
func TestSweepAll(t *testing.T) {
	d := newDaemon()
	r := &Registry{}
	other := "drydock.other"
	want := map[string]bool{}
	for _, k := range Kinds {
		want[d.add(Key(prefix, k)+"=gone1")] = true
		d.add(Key(other, k) + "=gone1")
	}
	both := d.add(Key(prefix, Cleanup)+"=01JABCDEFGHJKMNPQRSTVWXYZ0", Key(prefix, WorkspaceLabel)+"=01JABCDEFGHJKMNPQRSTVWXYZ0")
	workspace := d.add(Key(prefix, WorkspaceLabel) + "=01JABCDEFGHJKMNPQRSTVWXYZ1")
	inflight := Helper{Docker: d, Prefix: prefix, Kind: Identity, Value: "1", Registry: r}
	lease, err := inflight.Begin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	held := d.add(Key(prefix, Identity) + "=1")
	spared := d.add(Key(prefix, LogProbe) + "=01JBUSYBUSYBUSYBUSYBUSYBUS")

	gone, err := r.Sweep(context.Background(), d, prefix, Kinds, func(f Found) bool {
		return f.Kind == LogProbe && f.Value == "01JBUSYBUSYBUSYBUSYBUSYBUS"
	})
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	kinds := map[Kind]bool{}
	for _, f := range gone {
		got[f.ID] = true
		kinds[f.Kind] = true
	}
	if len(got) != len(want) {
		t.Errorf("removed %d; want %d, one of each kind", len(got), len(want))
	}
	for id := range want {
		if d.exists(id) || !got[id] {
			t.Errorf("a leftover %s was not swept", id)
		}
	}
	for _, k := range Kinds {
		if !kinds[k] {
			t.Errorf("no %s helper swept", k)
		}
	}
	for name, id := range map[string]string{"the helper with the workspace label": both, "a workspace container": workspace,
		"the helper in flight": held, "the helper skip spared": spared} {
		if !d.exists(id) {
			t.Errorf("the sweep removed %s", name)
		}
	}
	d.mu.Lock()
	for id, l := range d.ctrs {
		for k := range l {
			if strings.HasPrefix(k, other+".") && got[id] {
				t.Errorf("another prefix's %s was swept", k)
			}
		}
	}
	d.mu.Unlock()
	var foreign int
	for _, k := range Kinds {
		out := &strings.Builder{}
		d.Run(context.Background(), subproc.Cmd{Args: []string{"ps", "--filter", "label=" + Key(other, k)}, Stdout: out})
		foreign += len(strings.Fields(out.String()))
	}
	if foreign != len(Kinds) {
		t.Errorf("%d of another prefix's helpers remain; want all %d", foreign, len(Kinds))
	}

	// Once the holder ends, its label is no longer spared — its End removes
	// the container itself, and a sweep after finds nothing.
	if n, err := lease.End(context.Background(), false); err != nil || n != 1 || d.exists(held) {
		t.Errorf("the holder's end: %d %v", n, err)
	}
}

// TestABeginWaitsForASweep: a holder that begins while a sweep runs is
// registered only after it, so the sweep can never remove the container it
// is about to make — and its Begin never waits on itself.
func TestABeginWaitsForASweep(t *testing.T) {
	d := newDaemon()
	r := &Registry{}
	inSweep, release := make(chan struct{}), make(chan struct{})
	slow := runnerFunc(func(ctx context.Context, c subproc.Cmd) subproc.Result {
		if c.Args[0] == "ps" && strings.HasSuffix(c.Args[len(c.Args)-1], "."+string(Cleanup)) {
			close(inSweep)
			<-release
		}
		return d.Run(ctx, c)
	})
	go r.Sweep(context.Background(), slow, prefix, []Kind{Cleanup}, nil)
	<-inSweep
	h := Helper{Docker: d, Prefix: prefix, Kind: Login, Value: "abc", Registry: r}
	began := make(chan error, 1)
	var lease *Lease
	go func() {
		var err error
		lease, err = h.Begin(context.Background())
		began <- err
	}()
	select {
	case <-began:
		t.Fatal("Begin registered while a sweep ran")
	case <-time.After(50 * time.Millisecond):
	}
	close(release)
	if err := <-began; err != nil {
		t.Fatal(err)
	}
	lease.End(context.Background(), false)
}

type runnerFunc func(context.Context, subproc.Cmd) subproc.Result

func (f runnerFunc) Run(ctx context.Context, c subproc.Cmd) subproc.Result { return f(ctx, c) }
func (f runnerFunc) Start(context.Context, subproc.Cmd) (subproc.Process, error) {
	return nil, errors.New("unused")
}

// TestASweepSkipsAContainerGoneBeforeItsInspect: an --rm helper that exits
// between the sweep's listing and its inspect makes docker inspect fail for
// the whole batch. The sweep inspects each alone, skips the one a listing by
// id confirms is gone, and removes the rest. The control is a container that
// is still there but whose inspect fails: an error, nothing removed.
func TestASweepSkipsAContainerGoneBeforeItsInspect(t *testing.T) {
	d := newDaemon()
	stay := d.add(Key(prefix, Cleanup) + "=gone1")
	vanish := d.add(Key(prefix, Identity) + "=1")
	once := sync.Once{}
	d.beforeInspect = func() {
		once.Do(func() {
			d.mu.Lock()
			delete(d.ctrs, vanish)
			d.mu.Unlock()
		})
	}
	gone, err := (&Registry{}).Sweep(context.Background(), d, prefix, Kinds, nil)
	if err != nil {
		t.Fatalf("one vanished container failed the sweep: %v", err)
	}
	if len(gone) != 1 || gone[0].ID != stay || d.exists(stay) {
		t.Errorf("swept %+v; want the one still there", gone)
	}

	// Control: an inspect that fails for a container still listed.
	d = newDaemon()
	stuck := d.add(Key(prefix, Cleanup) + "=gone1")
	failing := runnerFunc(func(ctx context.Context, c subproc.Cmd) subproc.Result {
		if c.Args[0] == "inspect" {
			return subproc.Result{ExitCode: 1}
		}
		return d.Run(ctx, c)
	})
	if _, err := (&Registry{}).Sweep(context.Background(), failing, prefix, Kinds, nil); err == nil || !d.exists(stuck) {
		t.Errorf("an inspect that failed for a listed container: %v, exists %v", err, d.exists(stuck))
	}
}
