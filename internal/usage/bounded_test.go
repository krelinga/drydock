package usage

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/krelinga/drydock/internal/container"
	"github.com/krelinga/drydock/internal/sys"
	"github.com/krelinga/drydock/internal/workspace"
)

// TestMemoryNeverWaitsOnAWalk: with a walk stuck, a memory round still
// measures and publishes; the walk is cut off by the sampler's stop, and Run
// returns promptly. The control is the same walk let go, which records its
// figure.
func TestMemoryNeverWaitsOnAWalk(t *testing.T) {
	s, _, disk, clock, _, _ := harness()
	s.WalkTimeout = time.Hour // stuck for the whole test, until the stop
	disk.Block = make(chan struct{})
	var mu sync.Mutex
	var frames []Frame
	s.Publish = func(f Frame) { mu.Lock(); frames = append(frames, f); mu.Unlock() }
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { s.Run(ctx); close(done) }()

	deadline := time.Now().Add(5 * time.Second)
	for {
		mu.Lock()
		n := len(frames)
		mu.Unlock()
		if n >= 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("no memory frame while a walk was stuck")
		}
		time.Sleep(time.Millisecond)
	}
	mu.Lock()
	if m := frames[0].Workspaces[wsRun].Memory; m == nil || m.Bytes != 1_200_000_000 {
		t.Errorf("memory frame %+v", frames[0].Workspaces[wsRun])
	}
	mu.Unlock()
	// And the next tick's memory round runs while the walk is still stuck.
	// The memory loop's own interval, by its duration: the stuck walk has
	// its timeout on the same clock, so any timer at all could be that one,
	// and an Advance before the interval is set fires nothing.
	for i := 0; clock.WaitingFor(DefaultMemoryInterval) == 0; i++ {
		if i > 5000 {
			t.Fatal("the memory loop never waited for its next tick")
		}
		time.Sleep(time.Millisecond)
	}
	clock.Advance(DefaultMemoryInterval)
	for {
		mu.Lock()
		n := len(frames)
		mu.Unlock()
		if n >= 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the second memory round waited on the stuck walk")
		}
		time.Sleep(time.Millisecond)
	}

	start := time.Now()
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return while a walk was stuck")
	}
	if time.Since(start) > 2*time.Second {
		t.Errorf("stopping took %s", time.Since(start))
	}
	if d := s.Of(wsRun); d != nil && d.Disk != nil {
		t.Errorf("a walk cut off by the stop was recorded: %+v", d.Disk)
	}

	// Control: the same walk, let go, is recorded.
	s2, _, disk2, _, _, _ := harness()
	disk2.Block = make(chan struct{})
	close(disk2.Block)
	if d := s2.Round(context.Background()).Workspaces[wsRun].Disk; d == nil || d.Bytes != 3_400_000_000 {
		t.Errorf("control: %+v", d)
	}
}

// TestADiskFailureIsRetriedOnceThenBacksOff: a failed layer read keeps the
// last figures stale and is retried at the next tick, not five minutes on;
// a second failure in a row waits the full interval, so a daemon that keeps
// failing is not asked twice a minute. Only due workspaces' containers are
// asked about.
func TestADiskFailureIsRetriedOnceThenBacksOff(t *testing.T) {
	s, c, disk, clock, _, _ := harness()
	ctx := context.Background()
	s.Round(ctx)
	c.took()
	disk.Asked = nil

	clock.Advance(DefaultDiskInterval)
	c.layersErr = errors.New("daemon hung")
	f := s.Round(ctx)
	if d := f.Workspaces[wsRun].Disk; d == nil || !d.Stale {
		t.Fatalf("failed round: %+v", d)
	}
	c.took()
	clock.Advance(DefaultMemoryInterval) // retried at the next tick
	f = s.Round(ctx)
	if got := strings.Join(c.took(), "|"); !strings.Contains(got, "sizes") {
		t.Errorf("no retry at the next tick: %q", got)
	}
	clock.Advance(DefaultMemoryInterval) // failed twice: back off
	s.Round(ctx)
	if got := strings.Join(c.took(), "|"); strings.Contains(got, "sizes") {
		t.Errorf("asked again after two failures: %q", got)
	}

	c.layersErr = nil
	clock.Advance(DefaultDiskInterval)
	f = s.Round(ctx)
	if d := f.Workspaces[wsRun].Disk; d == nil || d.Stale {
		t.Errorf("recovered: %+v", d)
	}
}

// TestOnlyDueWorkspacesAreMeasured: a workspace never measured is measured
// at the next tick — its containers alone asked for their size — while the
// rest wait their interval.
func TestOnlyDueWorkspacesAreMeasured(t *testing.T) {
	s, c, disk, clock, r, _ := harness()
	ctx := context.Background()
	s.Round(ctx)
	c.took()
	*r = append(*r, workspace.Workspace{ID: wsNew, State: workspace.Stopped})
	c.found = append(c.found, foundOf(wsNew))
	disk.SetSize(root+"/"+wsNew, fakeSize(7))
	clock.Advance(DefaultMemoryInterval)
	s.Round(ctx)
	calls := strings.Join(c.took(), "|")
	if !strings.Contains(calls, "sizes "+cNew) || strings.Contains(calls, cRun+","+cStop) || strings.Contains(calls, "sizes "+cRun) {
		t.Errorf("calls %q: want the new workspace's container alone", calls)
	}
}

// TestAListingFailureIsLoggedOnce: a workspace listing that keeps failing is
// said once, and its recovery once.
func TestAListingFailureIsLoggedOnce(t *testing.T) {
	s, _, _, _, _, _ := harness()
	var logs []string
	s.Logf = func(f string, a ...any) { logs = append(logs, f) }
	failing := &failingRows{err: errors.New("database is locked")}
	s.Workspaces = failing
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		s.Round(ctx)
	}
	if n := countPrefix(logs, "drydock: usage: reading workspaces:"); n != 1 {
		t.Errorf("logged %d times: %q", n, logs)
	}
	failing.err = nil
	s.Round(ctx)
	if n := countPrefix(logs, "drydock: usage: reading workspaces works again"); n != 1 {
		t.Errorf("recovery: %q", logs)
	}
}

var cNew = strings.Repeat("c", 64)

func foundOf(ws string) container.Found {
	return container.Found{ContainerID: cNew, WorkspaceID: ws}
}

func fakeSize(n uint64) sys.FakeSize { return sys.FakeSize{Bytes: n} }

type failingRows struct{ err error }

func (f *failingRows) List(context.Context) ([]workspace.Workspace, error) {
	if f.err != nil {
		return nil, f.err
	}
	return []workspace.Workspace{{ID: wsRun, State: workspace.Running}}, nil
}

func countPrefix(logs []string, p string) int {
	n := 0
	for _, l := range logs {
		if strings.HasPrefix(l, p) {
			n++
		}
	}
	return n
}
