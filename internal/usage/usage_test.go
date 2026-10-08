package usage

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/krelinga/drydock/internal/container"
	"github.com/krelinga/drydock/internal/sys"
	"github.com/krelinga/drydock/internal/workspace"
)

const (
	wsRun  = "01JAAAAAAAAAAAAAAAAAAAAAAA"
	wsStop = "01JBBBBBBBBBBBBBBBBBBBBBBB"
	wsNew  = "01JCCCCCCCCCCCCCCCCCCCCCCC"
	wsDel  = "01JDDDDDDDDDDDDDDDDDDDDDDD"
	root   = "/srv/drydock/ws"
)

var (
	cRun  = strings.Repeat("a", 64)
	cStop = strings.Repeat("b", 64)
)

type fakeContainers struct {
	mu                         sync.Mutex
	found                      []container.Found
	mem, layers                map[string]uint64
	listErr, memErr, layersErr error
	calls                      []string
}

func (f *fakeContainers) List(context.Context) ([]container.Found, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "list")
	return f.found, f.listErr
}

func (f *fakeContainers) Memory(_ context.Context, ids []string) (map[string]uint64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "memory "+strings.Join(ids, ","))
	out := map[string]uint64{}
	for _, id := range ids {
		if v, ok := f.mem[id]; ok {
			out[id] = v
		}
	}
	return out, f.memErr
}

func (f *fakeContainers) WritableSizes(_ context.Context, ids []string) (map[string]uint64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "sizes "+strings.Join(ids, ","))
	return f.layers, f.layersErr
}

func (f *fakeContainers) took() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	c := f.calls
	f.calls = nil
	return c
}

type rows []workspace.Workspace

func (r *rows) List(context.Context) ([]workspace.Workspace, error) { return *r, nil }

func harness() (*Sampler, *fakeContainers, *sys.FakeDisk, *sys.FakeClock, *rows, *[]Frame) {
	clock := sys.NewFakeClock(time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC))
	c := &fakeContainers{
		found: []container.Found{
			{ContainerID: cRun, WorkspaceID: wsRun, Running: true},
			{ContainerID: cStop, WorkspaceID: wsStop, Running: true}, // its row stopped: never asked about
		},
		mem:    map[string]uint64{cRun: 1_200_000_000},
		layers: map[string]uint64{cRun: 300_000_000, cStop: 50_000_000},
	}
	disk := &sys.FakeDisk{}
	disk.Set(500, 1000)
	disk.SetSize(filepath.Join(root, wsRun), sys.FakeSize{Bytes: 3_100_000_000})
	disk.SetSize(filepath.Join(root, wsStop), sys.FakeSize{Bytes: 700_000_000, Partial: true})
	r := &rows{
		{ID: wsRun, State: workspace.Running},
		{ID: wsStop, State: workspace.Stopped},
		{ID: wsDel, State: workspace.Deleting},
	}
	var frames []Frame
	s := &Sampler{Workspaces: r, Containers: c, Disk: disk, Clock: clock, Root: root, LimitPercent: 90,
		Publish: func(f Frame) { frames = append(frames, f) }}
	return s, c, disk, clock, r, &frames
}

// TestRoundMeasuresRunningMemoryAndEveryDisk: the running workspace has its
// memory, the stopped one has none (not zero) but keeps its disk, the
// deleting one is not measured at all, and disk is directory plus layer.
func TestRoundMeasuresRunningMemoryAndEveryDisk(t *testing.T) {
	s, c, _, clock, _, frames := harness()
	f := s.Round(context.Background())
	run, stop := f.Workspaces[wsRun], f.Workspaces[wsStop]
	if run.Memory == nil || run.Memory.Bytes != 1_200_000_000 || run.Memory.Stale || !run.Memory.At.Equal(clock.Now()) {
		t.Errorf("running memory: %+v", run.Memory)
	}
	if stop.Memory != nil {
		t.Errorf("a stopped workspace has memory %+v; it has no reading", stop.Memory)
	}
	if d := run.Disk; d == nil || d.Bytes != 3_400_000_000 || d.DirectoryBytes != 3_100_000_000 ||
		d.ContainerBytes == nil || *d.ContainerBytes != 300_000_000 || d.Partial {
		t.Errorf("running disk: %+v", d)
	}
	if d := stop.Disk; d == nil || d.Bytes != 750_000_000 || !d.Partial {
		t.Errorf("stopped disk: %+v (partial must carry through)", d)
	}
	if _, ok := f.Workspaces[wsDel]; ok {
		t.Error("a deleting workspace was measured")
	}
	if f.Host == nil || f.Host.UsedBytes != 500 || f.Host.Over || f.Host.LimitPercent != 90 {
		t.Errorf("host %+v", f.Host)
	}
	if len(*frames) != 1 {
		t.Errorf("published %d frames", len(*frames))
	}
	// Only the running container is asked for memory, in one call.
	calls := c.took()
	if strings.Join(calls, "|") != "list|memory "+cRun+"|sizes "+cRun+","+cStop {
		t.Errorf("calls %q", calls)
	}
	if got := s.Of(wsRun); got == nil || got.Memory.Bytes != 1_200_000_000 {
		t.Errorf("Of: %+v", got)
	}
	if s.Of(wsDel) != nil {
		t.Error("Of a deleting workspace")
	}
}

// TestDiskKeepsItsOwnCadence: a second round inside the disk interval reads
// memory again but walks nothing — except a workspace never measured, which
// is walked at the next tick; past the interval everything is walked again.
func TestDiskKeepsItsOwnCadence(t *testing.T) {
	s, c, disk, clock, r, _ := harness()
	ctx := context.Background()
	s.Round(ctx)
	c.took()
	disk.Asked = nil

	clock.Advance(DefaultMemoryInterval)
	disk.SetSize(filepath.Join(root, wsRun), sys.FakeSize{Bytes: 9_000_000_000})
	f := s.Round(ctx)
	if len(disk.Asked) != 0 {
		t.Errorf("walked %v inside the disk interval", disk.Asked)
	}
	if f.Workspaces[wsRun].Disk.DirectoryBytes != 3_100_000_000 {
		t.Error("the disk figure changed without a walk")
	}
	if got := c.took(); strings.Join(got, "|") != "list|memory "+cRun {
		t.Errorf("a memory-only round called %q", got)
	}

	// A workspace created since: walked at the next tick, the others not.
	*r = append(*r, workspace.Workspace{ID: wsNew, State: workspace.Cloning})
	disk.SetSize(filepath.Join(root, wsNew), sys.FakeSize{Bytes: 10})
	clock.Advance(DefaultMemoryInterval)
	f = s.Round(ctx)
	if strings.Join(disk.Asked, ",") != filepath.Join(root, wsNew) {
		t.Errorf("walked %v; want only the new workspace", disk.Asked)
	}
	if d := f.Workspaces[wsNew].Disk; d == nil || d.Bytes != 10 || d.ContainerBytes != nil {
		t.Errorf("new workspace disk %+v", d)
	}
	disk.Asked = nil

	clock.Advance(DefaultDiskInterval)
	f = s.Round(ctx)
	if len(disk.Asked) != 3 || f.Workspaces[wsRun].Disk.DirectoryBytes != 9_000_000_000 {
		t.Errorf("past the interval walked %v, run disk %+v", disk.Asked, f.Workspaces[wsRun].Disk)
	}
}

// TestFailuresAreStaleNeverZero: a failed docker call keeps the last good
// figures, marked stale; a workspace never measured stays unknown (nil); and
// a directory not made yet is no figure. Recovery clears the mark.
func TestFailuresAreStaleNeverZero(t *testing.T) {
	s, c, disk, clock, r, _ := harness()
	ctx := context.Background()
	s.Round(ctx)

	c.memErr = errors.New("daemon hung")
	c.layersErr = errors.New("daemon hung")
	*r = append(*r, workspace.Workspace{ID: wsNew, State: workspace.Running})
	c.found = append(c.found, container.Found{ContainerID: strings.Repeat("c", 64), WorkspaceID: wsNew, Running: true})
	disk.SetSize(filepath.Join(root, wsNew), sys.FakeSize{Bytes: 1})
	clock.Advance(DefaultDiskInterval)
	f := s.Round(ctx)
	if m := f.Workspaces[wsRun].Memory; m == nil || !m.Stale || m.Bytes != 1_200_000_000 {
		t.Errorf("failed memory: %+v, want the last reading, stale", m)
	}
	if d := f.Workspaces[wsRun].Disk; d == nil || !d.Stale || d.Bytes != 3_400_000_000 {
		t.Errorf("failed disk: %+v, want the last reading, stale", d)
	}
	if n := f.Workspaces[wsNew]; n.Memory != nil || n.Disk != nil {
		t.Errorf("never measured, then failed: %+v %+v — unknown, never zero", n.Memory, n.Disk)
	}

	c.memErr, c.layersErr = nil, nil
	c.mem[strings.Repeat("c", 64)] = 5
	clock.Advance(DefaultDiskInterval)
	f = s.Round(ctx)
	if m := f.Workspaces[wsRun].Memory; m == nil || m.Stale {
		t.Errorf("recovered memory still stale: %+v", m)
	}
	if m := f.Workspaces[wsNew].Memory; m == nil || m.Bytes != 5 {
		t.Errorf("new workspace memory %+v", m)
	}

	// Running in the database but no running container: no reading.
	c.found[0].Running = false
	clock.Advance(DefaultMemoryInterval)
	if m := s.Round(ctx).Workspaces[wsRun].Memory; m != nil {
		t.Errorf("a running row with no running container read %+v", m)
	}

	// A workspace before step 1: no directory, no container — no figure.
	s2, _, _, _, r2, _ := harness()
	*r2 = rows{{ID: wsNew, State: workspace.Pending}}
	if d := s2.Round(ctx).Workspaces[wsNew].Disk; d != nil {
		t.Errorf("a workspace with no directory has disk %+v", d)
	}
}

// TestNothingRunningAsksDockerNothing: with every workspace stopped and every
// disk measured, a round makes no docker call at all.
func TestNothingRunningAsksDockerNothing(t *testing.T) {
	s, c, _, clock, r, _ := harness()
	ctx := context.Background()
	(*r)[0].State = workspace.Stopped
	s.Round(ctx)
	if got := c.took(); len(got) == 0 {
		t.Fatal("control: the first round measured no disk")
	}
	clock.Advance(DefaultMemoryInterval)
	s.Round(ctx)
	if got := c.took(); len(got) != 0 {
		t.Errorf("an idle round called docker: %q", got)
	}
}

// TestHostOverTheLimit: the frame's host figure says over at the limit, with
// the limit it was judged by; an unreadable filesystem keeps the last one.
func TestHostOverTheLimit(t *testing.T) {
	s, _, disk, _, _, _ := harness()
	ctx := context.Background()
	if s.Round(ctx).Host.Over {
		t.Fatal("control: 50% is over a limit of 90")
	}
	disk.Set(900, 1000)
	h := s.Round(ctx).Host
	if !h.Over || h.UsedBytes != 900 || h.TotalBytes != 1000 || h.LimitPercent != 90 {
		t.Errorf("host %+v", h)
	}
	disk.Err = errors.New("statfs")
	if h := s.Round(ctx).Host; h == nil || h.UsedBytes != 900 {
		t.Errorf("an unreadable filesystem lost the last reading: %+v", h)
	}
	if got := s.HostDisk(); got == nil || !got.Over {
		t.Errorf("HostDisk %+v", got)
	}
}

// TestRunSamplesOnTheMemoryInterval: Run measures at once, then on each
// interval, and ends with its context.
func TestRunSamplesOnTheMemoryInterval(t *testing.T) {
	s, _, _, clock, _, _ := harness()
	var mu sync.Mutex
	n := 0
	s.Publish = func(Frame) { mu.Lock(); n++; mu.Unlock() }
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { s.Run(ctx); close(done) }()
	count := func() int { mu.Lock(); defer mu.Unlock(); return n }
	waitFor := func(want int) {
		for i := 0; i < 2000 && (count() < want || clock.Waiting() == 0); i++ {
			time.Sleep(time.Millisecond)
		}
		if count() != want {
			t.Fatalf("%d rounds, want %d", count(), want)
		}
	}
	waitFor(1)
	clock.Advance(DefaultMemoryInterval - time.Second)
	time.Sleep(5 * time.Millisecond)
	if count() != 1 {
		t.Errorf("a round before the interval")
	}
	clock.Advance(time.Second)
	waitFor(2)
	cancel()
	<-done
}
