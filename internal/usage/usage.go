// Package usage measures what each workspace is using — its container's
// memory and its disk — for the card (design §6, *Resources*; frontend §6.1),
// and how full the disk holding the workspaces is, for §12's *Disk full*.
//
// A measurement is not state. Nothing about a workspace changes when its
// memory moves, so nothing here writes the event log: persisted every
// half-minute per workspace, the samples would be most of the log and would
// push the events that matter out of the replay window. The latest values are
// kept in memory, carried by the workspace views, and published once per
// round as one live, unpersisted `resources` frame (events.Broadcast). The
// browser's reducer is still their one writer; a missed frame is replaced by
// the next.
//
// The cost to the Docker daemon is bounded by rounds, not by workspaces:
//
//   - Every MemoryInterval (30 s): nothing at all while no workspace is
//     running; otherwise one `docker ps` by label, one `docker inspect`, and
//     one `docker stats --no-stream` naming every running container at once.
//     Memory moves on the scale of a build step or a test run, so half a
//     minute shows which workspace is the heavy one without making the
//     daemon compute stats continuously; `docker stats --no-stream` itself
//     takes about a second, since the daemon waits for a second CPU reading.
//   - Every DiskInterval (5 min), and on the next memory tick for a
//     workspace never measured: a walk of each workspace's directory (stat
//     only, no reads) and one `docker inspect --size` for every container,
//     which makes the daemon walk each writable layer. Disk grows slowly,
//     and both walks are real I/O on a large clone.
//
// Stopped workspaces keep their disk figure — they hold disk, and they are
// what an operator deletes to free some — but have no memory figure. A
// failed measurement keeps the last good value and marks it stale; it is never
// shown as zero, and never as current.
package usage

import (
	"context"
	"errors"
	"io/fs"
	"path/filepath"
	"sync"
	"time"

	"github.com/krelinga/drydock/internal/container"
	"github.com/krelinga/drydock/internal/sys"
	"github.com/krelinga/drydock/internal/workspace"
)

const (
	// DefaultMemoryInterval and DefaultDiskInterval are the two cadences;
	// the package comment gives the reasons.
	DefaultMemoryInterval = 30 * time.Second
	DefaultDiskInterval   = 5 * time.Minute
	// DefaultCallTimeout bounds each docker call and each walk round, so a
	// hung daemon makes a stale reading rather than a stuck sampler.
	DefaultCallTimeout = 20 * time.Second
	// FrameName is the stream's name for a round's frame.
	FrameName = "resources"
)

// Containers is what the sampler needs from container.Manager.
type Containers interface {
	List(ctx context.Context) ([]container.Found, error)
	Memory(ctx context.Context, ids []string) (map[string]uint64, error)
	WritableSizes(ctx context.Context, ids []string) (map[string]uint64, error)
}

// Workspaces lists the rows to measure.
type Workspaces interface {
	List(ctx context.Context) ([]workspace.Workspace, error)
}

// Frame is one round, as the stream carries it: every workspace with a row
// but a deleting one, by id, and the host's disk. A workspace absent from
// Workspaces has nothing measured; the reducer leaves it as it is.
type Frame struct {
	At         time.Time                      `json:"at"`
	Workspaces map[string]workspace.Resources `json:"workspaces"`
	Host       *workspace.HostDisk            `json:"host"`
}

// Sampler measures on its two cadences until its context ends.
type Sampler struct {
	Workspaces Workspaces
	Containers Containers
	Disk       sys.DiskUsage
	Clock      sys.Clock
	// Root is the workspace root: a workspace's directory is Root/<id>.
	Root string
	// LimitPercent is config.DiskLimitPercent, reported with the host's
	// figures so the banner and the refusal use one number.
	LimitPercent int
	// Publish sends a round's frame to the stream (events.Log.Broadcast
	// under FrameName). Nil publishes nothing.
	Publish func(Frame)
	// Intervals and timeout; zero means the defaults.
	MemoryInterval, DiskInterval, CallTimeout time.Duration
	Logf                                      func(format string, args ...any)

	mu       sync.Mutex
	latest   map[string]workspace.Resources
	host     *workspace.HostDisk
	lastDisk time.Time
}

func (s *Sampler) logf(f string, a ...any) {
	if s.Logf != nil {
		s.Logf(f, a...)
	}
}

// failed logs a measurement's failure — unless the round was cut off by
// shutdown, which is no failure of the measurement.
func (s *Sampler) failed(ctx context.Context, f string, a ...any) {
	if ctx.Err() == nil {
		s.logf(f, a...)
	}
}

func or(d, def time.Duration) time.Duration {
	if d > 0 {
		return d
	}
	return def
}

// Run samples at once and then every MemoryInterval until ctx ends.
func (s *Sampler) Run(ctx context.Context) {
	for {
		s.Round(ctx)
		select {
		case <-ctx.Done():
			return
		case <-s.Clock.After(or(s.MemoryInterval, DefaultMemoryInterval)):
		}
	}
}

// Round is one sampling round: memory for running workspaces, disk when it
// is due (or for any workspace never measured), and the host's filesystem;
// then the frame is published. Exported for tests, which drive rounds
// directly rather than through the clock.
func (s *Sampler) Round(ctx context.Context) Frame {
	now := s.Clock.Now()
	rows, err := s.Workspaces.List(ctx)
	if err != nil {
		s.failed(ctx, "drydock: usage: reading workspaces: %v", err)
		return s.Snapshot()
	}
	s.mu.Lock()
	prev := s.latest
	diskDue := s.lastDisk.IsZero() || now.Sub(s.lastDisk) >= or(s.DiskInterval, DefaultDiskInterval)
	s.mu.Unlock()

	live := []workspace.Workspace{}
	anyRunning, needDisk := false, diskDue
	for _, w := range rows {
		if w.State == workspace.Deleting {
			continue
		}
		live = append(live, w)
		anyRunning = anyRunning || w.State == workspace.Running
		if p, ok := prev[w.ID]; !ok || p.Disk == nil {
			needDisk = true
		}
	}

	// The containers, found by label: only if something needs them.
	var found []container.Found
	foundErr := error(nil)
	if anyRunning || needDisk {
		cctx, cancel := sys.WithTimeout(ctx, s.Clock, or(s.CallTimeout, DefaultCallTimeout))
		found, foundErr = s.Containers.List(cctx)
		cancel()
		if foundErr != nil {
			s.failed(ctx, "drydock: usage: listing containers: %v", foundErr)
		}
	}
	byWS := map[string][]container.Found{}
	for _, f := range found {
		byWS[f.WorkspaceID] = append(byWS[f.WorkspaceID], f)
	}

	// Memory: the running containers of running workspaces, in one call.
	var mem map[string]uint64
	memErr := foundErr
	if anyRunning && foundErr == nil {
		var ids []string
		for _, w := range live {
			if w.State != workspace.Running {
				continue
			}
			for _, f := range byWS[w.ID] {
				if f.Running {
					ids = append(ids, f.ContainerID)
				}
			}
		}
		cctx, cancel := sys.WithTimeout(ctx, s.Clock, or(s.CallTimeout, DefaultCallTimeout))
		mem, memErr = s.Containers.Memory(cctx, ids)
		cancel()
		if memErr != nil {
			s.failed(ctx, "drydock: usage: reading memory: %v", memErr)
		}
	}

	// The writable layers, at the disk cadence.
	var layers map[string]uint64
	layerErr := foundErr
	if needDisk && foundErr == nil {
		var ids []string
		for _, f := range found {
			ids = append(ids, f.ContainerID)
		}
		cctx, cancel := sys.WithTimeout(ctx, s.Clock, or(s.CallTimeout, DefaultCallTimeout))
		layers, layerErr = s.Containers.WritableSizes(cctx, ids)
		cancel()
		if layerErr != nil {
			s.failed(ctx, "drydock: usage: reading container sizes: %v", layerErr)
		}
	}

	next := map[string]workspace.Resources{}
	for _, w := range live {
		p := prev[w.ID]
		r := workspace.Resources{At: now}
		if w.State == workspace.Running {
			r.Memory = memoryOf(w, byWS[w.ID], mem, memErr, p.Memory, now)
		}
		if needDisk && (diskDue || p.Disk == nil) {
			r.Disk = s.diskOf(w, byWS[w.ID], layers, layerErr, p.Disk, now)
		} else {
			r.Disk = p.Disk
		}
		next[w.ID] = r
	}

	var host *workspace.HostDisk
	if used, total, err := s.Disk.Usage(s.Root); err != nil {
		s.logf("drydock: usage: the workspace filesystem: %v", err)
	} else {
		host = &workspace.HostDisk{UsedBytes: used, TotalBytes: total, LimitPercent: s.LimitPercent,
			Over: workspace.OverLimit(used, total, s.LimitPercent), At: now}
	}

	if ctx.Err() != nil {
		// Shutting down: what this round read may be half a round. Keep
		// the last whole one, and publish nothing.
		return s.Snapshot()
	}
	s.mu.Lock()
	s.latest = next
	if host != nil {
		s.host = host
	}
	if diskDue {
		s.lastDisk = now
	}
	f := s.snapshotLocked(now)
	s.mu.Unlock()
	if s.Publish != nil {
		s.Publish(f)
	}
	return f
}

// memoryOf is one running workspace's memory figure: the sum over its running
// containers (normally one), or — when the measurement failed — the last good
// figure marked stale. A running workspace whose container docker does not
// report as running has no figure: that is not a reading of zero.
func memoryOf(w workspace.Workspace, found []container.Found, mem map[string]uint64, err error,
	prev *workspace.MemorySample, now time.Time) *workspace.MemorySample {
	if err != nil {
		return stale(prev)
	}
	var total uint64
	any := false
	for _, f := range found {
		if v, ok := mem[f.ContainerID]; ok && f.Running {
			total += v
			any = true
		}
	}
	if !any {
		return nil
	}
	return &workspace.MemorySample{Bytes: total, At: now}
}

func stale(p *workspace.MemorySample) *workspace.MemorySample {
	if p == nil {
		return nil
	}
	c := *p
	c.Stale = true
	return &c
}

// diskOf is one workspace's disk: its directory and its containers' layers.
// A directory not made yet (a create before step 1) is no figure; any other
// failure keeps the last figure, stale.
func (s *Sampler) diskOf(w workspace.Workspace, found []container.Found, layers map[string]uint64, layerErr error,
	prev *workspace.DiskSample, now time.Time) *workspace.DiskSample {
	dir, partial, err := s.Disk.Size(filepath.Join(s.Root, w.ID))
	if errors.Is(err, fs.ErrNotExist) && len(found) == 0 {
		return nil
	}
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		s.logf("drydock: usage: workspace %s's directory: %v", w.ID, err)
		return staleDisk(prev)
	}
	if layerErr != nil && len(found) > 0 {
		return staleDisk(prev)
	}
	d := &workspace.DiskSample{DirectoryBytes: dir, Partial: partial, At: now}
	if len(found) > 0 {
		var c uint64
		for _, f := range found {
			v, ok := layers[f.ContainerID]
			if !ok {
				return staleDisk(prev) // removed between the two calls: measure again next time
			}
			c += v
		}
		d.ContainerBytes = &c
	}
	d.Bytes = d.DirectoryBytes
	if d.ContainerBytes != nil {
		d.Bytes += *d.ContainerBytes
	}
	return d
}

func staleDisk(p *workspace.DiskSample) *workspace.DiskSample {
	if p == nil {
		return nil
	}
	c := *p
	c.Stale = true
	return &c
}

// Snapshot is the latest round: what the views carry.
func (s *Sampler) Snapshot() Frame {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.snapshotLocked(time.Time{})
}

func (s *Sampler) snapshotLocked(at time.Time) Frame {
	f := Frame{At: at, Workspaces: map[string]workspace.Resources{}}
	for id, r := range s.latest {
		f.Workspaces[id] = r
	}
	if s.host != nil {
		h := *s.host
		f.Host = &h
	}
	return f
}

// Of is one workspace's latest resources, or nil when nothing was measured.
func (s *Sampler) Of(id string) *workspace.Resources {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.latest[id]
	if !ok {
		return nil
	}
	return &r
}

// HostDisk is the latest reading of the workspace filesystem, or nil.
func (s *Sampler) HostDisk() *workspace.HostDisk {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.host == nil {
		return nil
	}
	h := *s.host
	return &h
}
