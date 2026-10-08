package workspace

import "time"

// Resources is what a workspace is using, as last measured (design §6,
// *Resources*): memory while its container runs, and disk always. It is a
// measurement, not state — nothing about the workspace changes when it does
// — so it is never written to the event log. The sampler (internal/usage)
// keeps the latest in memory, the views carry it, and each sampling round
// reaches the stream as one live, unpersisted `resources` frame.
//
// A field is null when there is no reading, which is never the same as a
// reading of zero: a stopped workspace has no memory figure at all, and one
// whose directory has not been measured yet has no disk figure.
type Resources struct {
	// Round and Boot are the version the client orders copies by: the
	// sampler's round counter, and an id of this process, so a restart's
	// round 1 is not taken for an old one. Never the wall clock, which can
	// step backwards. Each sample keeps its own At, the time it was
	// measured, which a stale one's is older than.
	Round  uint64        `json:"round"`
	Boot   string        `json:"boot"`
	Memory *MemorySample `json:"memory"`
	Disk   *DiskSample   `json:"disk"`
}

// MemorySample is the running container's memory as `docker stats` counts it:
// the cgroup's usage with the page cache taken out.
type MemorySample struct {
	Bytes uint64    `json:"bytes"`
	At    time.Time `json:"at"`
	// ContainerID is the container measured, when there was one: the
	// client shows the figure only while it is the workspace's container,
	// so a rebuild inside one round — which replaces the container, and its
	// id — never shows the old one's. (A plain stop and start keeps the
	// container and its id.)
	ContainerID string `json:"container_id,omitempty"`
	// Stale is set when the latest attempt to measure failed, so Bytes is
	// the last good reading, from At. The UI says so; it never shows a
	// stale figure as current.
	Stale bool `json:"stale"`
}

// DiskSample is what deleting the workspace would free: its host directory
// (<WorkspaceRoot>/<id> — the clone and .drydock/) and its container's
// writable layer. Images and the shared credential volume are not counted;
// other workspaces share them.
type DiskSample struct {
	// Bytes is DirectoryBytes plus ContainerBytes.
	Bytes          uint64 `json:"bytes"`
	DirectoryBytes uint64 `json:"directory_bytes"`
	// ContainerBytes is null when the workspace has no container.
	ContainerBytes *uint64 `json:"container_bytes"`
	// Partial: part of the directory could not be read — a container's root
	// made a directory private — so Bytes is a lower bound.
	Partial bool      `json:"partial"`
	At      time.Time `json:"at"`
	Stale   bool      `json:"stale"`
}

// HostDisk is the filesystem holding the workspace root, against the limit
// at and above which new workspaces are refused (design §12, *Disk full*).
type HostDisk struct {
	UsedBytes    uint64    `json:"used_bytes"`
	TotalBytes   uint64    `json:"total_bytes"`
	LimitPercent int       `json:"limit_percent"`
	Over         bool      `json:"over"`
	At           time.Time `json:"at"`
	// Round and Boot version it, as Resources' do.
	Round uint64 `json:"round"`
	Boot  string `json:"boot"`
}

// OverLimit is the one rule for "too full": used at or above limitPercent of
// total. A limit outside 1–99 never refuses, and an unreadable total (zero)
// is not "full".
func OverLimit(used, total uint64, limitPercent int) bool {
	if total == 0 || limitPercent >= 100 || limitPercent <= 0 {
		return false
	}
	// In integers, so the edge is exact: 90 of 100 at a limit of 90 is over.
	return used*100 >= total*uint64(limitPercent)
}
