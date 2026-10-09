package identity

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/krelinga/drydock/internal/life"
	"github.com/krelinga/drydock/internal/subproc"
)

// hangingSource is a Source whose credential read blocks until its context
// ends — what a FIFO at the credential path, or a daemon that stopped
// answering, does to the real one — and which counts its sweeps. While
// hang is false it answers like fakeSource.
type hangingSource struct {
	fakeSource
	hmu     sync.Mutex
	hang    bool
	entered chan struct{}
	sweeps  int
}

func newHanging() *hangingSource { return &hangingSource{entered: make(chan struct{}, 16)} }

func (h *hangingSource) setHang(v bool) { h.hmu.Lock(); h.hang = v; h.hmu.Unlock() }

func (h *hangingSource) Credentials(ctx context.Context) ([]byte, error) {
	h.hmu.Lock()
	hang := h.hang
	h.hmu.Unlock()
	if hang {
		h.entered <- struct{}{}
		<-ctx.Done()
		return nil, &ReadError{Problem: ProblemDocker, Detail: "docker could not be run: " + ctx.Err().Error()}
	}
	return h.fakeSource.Credentials(ctx)
}

func (h *hangingSource) Sweep(context.Context) (int, error) {
	h.hmu.Lock()
	defer h.hmu.Unlock()
	h.sweeps++
	return 1, nil
}

// srcReads is how many credential reads src answered — not counting one
// that hung.
func srcReads(src *hangingSource) int {
	src.mu.Lock()
	defer src.mu.Unlock()
	return src.credsCalls
}

func (h *hangingSource) sweepCount() int { h.hmu.Lock(); defer h.hmu.Unlock(); return h.sweeps }

// TestAHungReadIsBoundedAndReported is #37's review finding: a read that
// never returns must not freeze the fleet's login state. With Timeout on the
// injected clock:
//
//   - the check ends when the clock passes the timeout, as a failed check
//     with its own problem (timeout) and sentence, and an
//     auth.identity_check_failed event — never silence;
//   - the stored state is kept (a failed read keeps it, #37's rule) — here
//     ok, which a frozen watch would have gone on asserting;
//   - the helper is swept after the cut-off read;
//   - a check asked for during the hung one is not stuck behind it: it runs
//     when the hung one ends and reads normally (the positive control: same
//     watch, same clock, a regular answer, stored).
func TestAHungReadIsBoundedAndReported(t *testing.T) {
	h := newHarness(t)
	src := newHanging()
	h.w.Source = src
	h.w.Timeout = time.Minute
	ctx := waitCtx(t)

	src.set(fixture(t, "credentials", "ok.json"), fixture(t, "authstatus", "valid.json"), nil, nil)
	v, err := h.w.Check(ctx)
	if err != nil {
		t.Fatal(err)
	}
	stateIs(t, v, OK)
	bootSweeps := src.sweepCount()
	if bootSweeps != 1 {
		t.Fatalf("sweeps after the first check = %d; want the boot sweep alone", bootSweeps)
	}

	src.setHang(true)
	hung, err := h.w.c.Trigger()
	if err != nil {
		t.Fatal(err)
	}
	receive(t, src.entered, "the check did not reach its read")
	type result struct {
		v   View
		err error
	}
	// Asked for during the hung check: it gets a check of its own, after.
	src.set(fixture(t, "credentials", "blanked.json"), nil, nil, nil)
	next := make(chan result, 1)
	go func() { v, err := h.w.Check(ctx); next <- result{v, err} }()
	waitFor(t, func() bool { return h.w.c.Asked() == hung+1 })
	src.setHang(false)

	first := make(chan result, 1)
	go func() { v, err := h.w.c.Await(ctx, hung); first <- result{v, err} }()
	h.clock.Advance(59 * time.Second)
	select {
	case r := <-first:
		t.Fatalf("the check ended before its timeout: %v", r.err)
	case <-time.After(50 * time.Millisecond):
	}
	h.clock.Advance(time.Second)
	var r result
	select {
	case r = <-first:
	case <-time.After(5 * time.Second):
		t.Fatal("a hung read held the check past its timeout")
	}
	// Await answers with the newest check's result, which may already be
	// the next one's; the hung check's own verdict is on the stream.
	if !strings.Contains(strings.Join(h.kinds(t), ","), KindCheckFailed) {
		t.Fatalf("events %v; want the hung check's %s", h.kinds(t), KindCheckFailed)
	}
	var data struct {
		CheckError *CheckError `json:"check_error"`
	}
	for _, e := range h.events(t) {
		if e.Kind == KindCheckFailed {
			json.Unmarshal(e.Data, &data)
		}
	}
	if data.CheckError == nil || data.CheckError.Problem != ProblemTimeout || data.CheckError.Message != sentence(ProblemTimeout) {
		t.Errorf("check_error = %+v; want the timeout's", data.CheckError)
	}
	if got := src.sweepCount(); got != bootSweeps+1 {
		t.Errorf("sweeps = %d; want one after the cut-off read", got)
	}

	// Control: the check asked for meanwhile is not stuck behind the dead
	// one, and reads.
	select {
	case r = <-next:
		if r.err != nil {
			t.Fatal(r.err)
		}
		stateIs(t, r.v, Blanked)
		if r.v.CheckError != nil {
			t.Errorf("a recovered check still reports %+v", r.v.CheckError)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the check after a hung one never returned")
	}
}

// TestHungCheckKeepsTheStoredState: the hung check alone, with nothing asked
// after it, so its own result is Await's: a timeout ReadError, the stored ok
// kept, and the timeout's check_error on the view.
func TestHungCheckKeepsTheStoredState(t *testing.T) {
	h := newHarness(t)
	src := newHanging()
	h.w.Source = src
	h.w.Timeout = time.Minute
	src.set(fixture(t, "credentials", "ok.json"), fixture(t, "authstatus", "valid.json"), nil, nil)
	if _, err := h.w.Check(waitCtx(t)); err != nil {
		t.Fatal(err)
	}
	src.setHang(true)
	got := make(chan error, 1)
	var v View
	go func() {
		var err error
		v, err = h.w.Check(waitCtx(t))
		got <- err
	}()
	receive(t, src.entered, "the check did not reach its read")
	h.clock.Advance(time.Minute)
	var err error
	select {
	case err = <-got:
	case <-time.After(5 * time.Second):
		t.Fatal("a hung read held the check past its timeout")
	}
	var re *ReadError
	if !errors.As(err, &re) || re.Problem != ProblemTimeout {
		t.Fatalf("hung check: %v; want a timeout ReadError", err)
	}
	stateIs(t, v, OK)
	if v.CheckError == nil || v.CheckError.Problem != ProblemTimeout {
		t.Errorf("check_error = %+v; want the timeout's", v.CheckError)
	}
}

// TestAShutdownEndsATriggeredCheck: a check runs under the watch's group,
// whose Stop ends it — and whose Wait does not return before it has ended —
// and the cut-off read is still swept, under its own bound. A check asked
// for meanwhile never starts: its ticket is refused. The control is a
// triggered check that reads normally before the shutdown. (#85's finding 5:
// every wait here is bounded, so a regression fails by name.)
func TestAShutdownEndsATriggeredCheck(t *testing.T) {
	h := newHarness(t)
	src := newHanging()
	h.w.Source = src
	h.w.Timeout = time.Hour // never reached: the shutdown must end it

	src.set(nil, nil, nil, nil)
	h.w.Trigger()
	h.settled(t, "the first triggered check did not end")
	if v, _ := h.w.Read(context.Background()); srcReads(src) != 1 || v.State == nil || *v.State != Absent {
		t.Fatalf("control: %d credential reads, state %v; want one read, stored absent", srcReads(src), v.State)
	}

	src.setHang(true)
	h.w.Trigger()
	receive(t, src.entered, "the second triggered check did not reach its read")
	queued, err := h.w.c.Trigger()
	if err != nil {
		t.Fatal(err)
	}
	before := src.sweepCount()
	ended := make(chan []string, 1)
	go func() { ended <- h.group.Wait(nil) }()
	select {
	case late := <-ended:
		if late != nil {
			t.Fatalf("stragglers %v", late)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the group's Wait did not end a triggered check")
	}
	if src.sweepCount() != before+1 {
		t.Errorf("sweeps %d → %d; want the cut-off read swept", before, src.sweepCount())
	}
	if _, err := h.w.c.Await(waitCtx(t), queued); !errors.Is(err, life.ErrStopping) {
		t.Errorf("the check asked for during the cut-off one: %v; want life.ErrStopping", err)
	}
	// After shutdown a Trigger starts nothing: the worker has exited, so no
	// sleep stands in for "long enough".
	if err := h.w.Trigger(); !errors.Is(err, life.ErrStopping) {
		t.Errorf("a Trigger after shutdown = %v; want life.ErrStopping", err)
	}
	select {
	case <-src.entered:
		t.Error("a check started after the shutdown")
	default:
	}
}

// TestATriggerInACheckTailGetsACheckOfItsOwn replaces the join #78 and #81
// pinned. A Trigger while a check is in its tail — verdict stored, check not
// yet ended — used to join it and read nothing, and the test that waited on
// the stored verdict as proof the next Trigger would read hung on CI. Now no
// request is ever answered by a check already running: the press in the
// tail gets a check after it, which reads. The seam holds the first check
// after its announcement until the Trigger has been made. The control is a
// Trigger after the check has ended, which reads too.
func TestATriggerInACheckTailGetsACheckOfItsOwn(t *testing.T) {
	h := newHarness(t)
	src := newHanging()
	h.w.Source = src
	h.w.Timeout = time.Hour
	src.set(nil, nil, nil, nil)

	tail, pressed := make(chan struct{}), make(chan struct{})
	var once sync.Once
	h.w.observe = func(p string) {
		if p == "announced" {
			once.Do(func() {
				close(tail)
				<-pressed
			})
		}
	}

	h.w.Trigger()
	receive(t, tail, "the first check did not announce")
	if v, _ := h.w.Read(context.Background()); v.State == nil || *v.State != Absent {
		t.Fatalf("in the tail the verdict is %v; want absent already stored", v.State)
	}
	h.w.Trigger()
	close(pressed)
	h.settled(t, "the Trigger made in the tail was not answered")
	if n := srcReads(src); n != 2 {
		t.Fatalf("%d credential reads; want 2: a Trigger in a check's tail gets a check of its own", n)
	}
	if got := strings.Join(h.kinds(t), ","); got != KindIdentity+","+KindChecked {
		t.Errorf("events [%s]; want the first check's verdict, then the press's answer", got)
	}

	h.w.Trigger()
	h.settled(t, "control: the Trigger after the check ended did not end")
	if n := srcReads(src); n != 3 {
		t.Fatalf("control: %d credential reads; want 3: a Trigger after the check ended reads", n)
	}
}

// TestABuildHasItsOwnBound: the image's first build is not a read, so the
// reads' short timeout does not cut it off, and its own bound ends a build
// that never finishes as an image problem — which, beside a blanked file, is
// no failure at all: blanked needs no auth status.
func TestABuildHasItsOwnBound(t *testing.T) {
	h := newHarness(t)
	src := &preparing{entered: make(chan struct{}, 1)}
	h.w.Source = src
	h.w.Timeout = time.Minute
	h.w.BuildTimeout = time.Hour

	src.set(fixture(t, "credentials", "ok.json"), fixture(t, "authstatus", "valid.json"), nil, nil)
	got := make(chan error, 1)
	go func() { _, err := h.w.Check(waitCtx(t)); got <- err }()
	<-src.entered
	h.clock.Advance(30 * time.Minute) // past the reads' bound, inside the build's
	select {
	case err := <-got:
		t.Fatalf("the reads' timeout cut off a build: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	h.clock.Advance(30 * time.Minute)
	err := <-got
	var re *ReadError
	if !errors.As(err, &re) || re.Problem != ProblemImage {
		t.Fatalf("a build past its bound: %v; want an image ReadError", err)
	}

	src.set(fixture(t, "credentials", "blanked.json"), nil, nil, nil)
	go func() { _, err := h.w.Check(waitCtx(t)); got <- err }()
	<-src.entered
	h.clock.Advance(time.Hour)
	if err := <-got; err != nil {
		t.Fatalf("blanked waited on the build: %v", err)
	}
}

type preparing struct {
	fakeSource
	entered chan struct{}
}

func (p *preparing) Prepare(ctx context.Context) error {
	p.entered <- struct{}{}
	<-ctx.Done()
	return &ReadError{Problem: ProblemImage, Detail: ctx.Err().Error()}
}

// TestTheReadScriptRefusesWhatNeverEnds runs credentialsScript itself — the
// constant line the busybox helper runs — under this host's sh, pointed at a
// temp directory, against each thing a workspace could leave at the
// credential path. A FIFO and a symlink to /dev/zero would block or stream
// forever under `cat`; each is refused without being read. An oversized
// regular file is read only to the cap's one-past, which the caller refuses.
// The controls: a regular file is printed whole, and no file is exit 3.
func TestTheReadScriptRefusesWhatNeverEnds(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("no sh")
	}
	if maxReadPlusOne != strconv.Itoa(maxRead+1) {
		t.Fatalf("the script reads %s bytes; the cap is %d", maxReadPlusOne, maxRead)
	}
	run := func(t *testing.T, dir string) (string, int) {
		t.Helper()
		script := strings.Replace(credentialsScript, Mount+"/", dir+"/", 1)
		if script == credentialsScript {
			t.Fatal("the script does not name the mount")
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		var out strings.Builder
		lim := &limitedBuilder{b: &out, max: maxRead}
		res := subproc.Exec{}.Run(ctx, subproc.Cmd{Name: "sh", Args: []string{"-c", script}, Stdout: lim})
		if ctx.Err() != nil {
			t.Fatal("the script did not end")
		}
		if lim.over {
			return "OVER", res.ExitCode
		}
		return out.String(), res.ExitCode
	}
	path := func(dir string) string { return filepath.Join(dir, ".credentials.json") }

	t.Run("a regular file", func(t *testing.T) {
		dir := t.TempDir()
		os.WriteFile(path(dir), []byte(`{"claudeAiOauth":{}}`), 0o600)
		if out, code := run(t, dir); code != 0 || out != `{"claudeAiOauth":{}}` {
			t.Fatalf("%q, exit %d", out, code)
		}
	})
	t.Run("no file", func(t *testing.T) {
		if _, code := run(t, t.TempDir()); code != exitAbsent {
			t.Fatalf("exit %d; want %d", code, exitAbsent)
		}
	})
	t.Run("a FIFO", func(t *testing.T) {
		dir := t.TempDir()
		if err := syscall.Mkfifo(path(dir), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, code := run(t, dir); code != exitNotRegular {
			t.Fatalf("exit %d; want %d", code, exitNotRegular)
		}
	})
	t.Run("a symlink to /dev/zero", func(t *testing.T) {
		dir := t.TempDir()
		os.Symlink("/dev/zero", path(dir))
		if _, code := run(t, dir); code != exitNotRegular {
			t.Fatalf("exit %d; want %d", code, exitNotRegular)
		}
	})
	t.Run("a symlink to a regular file", func(t *testing.T) {
		dir := t.TempDir()
		os.WriteFile(filepath.Join(dir, "real"), []byte(`{}`), 0o600)
		os.Symlink(filepath.Join(dir, "real"), path(dir))
		if _, code := run(t, dir); code != exitNotRegular {
			t.Fatalf("exit %d; want %d", code, exitNotRegular)
		}
	})
	t.Run("a directory", func(t *testing.T) {
		dir := t.TempDir()
		os.Mkdir(path(dir), 0o700)
		if _, code := run(t, dir); code != exitNotRegular {
			t.Fatalf("exit %d; want %d", code, exitNotRegular)
		}
	})
	t.Run("an oversized file", func(t *testing.T) {
		dir := t.TempDir()
		os.WriteFile(path(dir), []byte(strings.Repeat("x", 4*maxRead)), 0o600)
		if out, _ := run(t, dir); out != "OVER" {
			t.Fatalf("an oversized file was read within the cap: %d bytes", len(out))
		}
		// And the script itself stops one byte past the cap: counted with
		// a writer that never refuses, so the bound is the script's.
		var n countWriter
		script := strings.Replace(credentialsScript, Mount+"/", dir+"/", 1)
		subproc.Exec{}.Run(context.Background(), subproc.Cmd{Name: "sh", Args: []string{"-c", script}, Stdout: &n})
		if int(n) != maxRead+1 {
			t.Fatalf("the script printed %d bytes of an oversized file; want %d", n, maxRead+1)
		}
	})
}

type limitedBuilder struct {
	b    *strings.Builder
	max  int
	over bool
}

func (l *limitedBuilder) Write(p []byte) (int, error) {
	if l.b.Len()+len(p) > l.max {
		l.over = true
		return 0, errTooLong
	}
	return l.b.Write(p)
}

// TestSweepRemovesHelpersByLabel: the sweep lists by this prefix's identity
// label alone — never the workspace or login labels — and removes by full
// id after --; a listing that is not ids removes nothing. The control is
// an empty listing, which runs no rm.
func TestSweepRemovesHelpersByLabel(t *testing.T) {
	ctx := context.Background()
	id := strings.Repeat("a", 64)
	r := &scripted{answers: map[string]func(subproc.Cmd) subproc.Result{
		"ps": write(id+"\n", 0),
		"rm": write("", 0)}}
	n, err := newSource(r, nil).Sweep(ctx)
	if err != nil || n != 1 {
		t.Fatalf("sweep: %d, %v", n, err)
	}
	if got := strings.Join(r.cmds[0].Args, " "); got != "ps --all --quiet --no-trunc --filter label=drydock.test.identity" {
		t.Errorf("listing argv: %s", got)
	}
	if got := strings.Join(r.cmds[1].Args, " "); got != "rm --force -- "+id {
		t.Errorf("rm argv: %s", got)
	}

	r = &scripted{answers: map[string]func(subproc.Cmd) subproc.Result{"ps": write("", 0)}}
	if n, err := newSource(r, nil).Sweep(ctx); err != nil || n != 0 || len(r.cmds) != 1 {
		t.Errorf("empty listing: %d, %v, %d commands", n, err, len(r.cmds))
	}
	r = &scripted{answers: map[string]func(subproc.Cmd) subproc.Result{"ps": write("--all\n", 0)}}
	if _, err := newSource(r, nil).Sweep(ctx); err == nil || len(r.cmds) != 1 {
		t.Errorf("a listing that is not ids: %v, %d commands", err, len(r.cmds))
	}
}

type countWriter int

func (c *countWriter) Write(p []byte) (int, error) { *c += countWriter(len(p)); return len(p), nil }
