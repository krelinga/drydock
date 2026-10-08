package container

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math"
	"regexp"
	"strconv"

	"github.com/krelinga/drydock/internal/subproc"
)

// Memory reads each running container's current memory use, as the daemon
// reports it: `docker stats --no-stream --no-trunc --format '{{json .}}'`, one
// JSON object per line, for the given ids in one call — so a sampling round
// costs the daemon one request whatever the number of workspaces (design §6,
// *Resources*). Nothing parses a table.
//
// The number is the left half of MemUsage, which is what `docker stats` shows
// an operator: the cgroup's usage with the page cache taken out. Docker prints
// it already formatted ("1.21GiB / 15.5GiB", four significant figures), so it
// is parsed strictly — a figure and one of Docker's binary units — and a value
// in any other shape is an error, never a guess. A container that is not
// running reports "0B / 0B"; that is the absence of a reading, not a reading
// of zero, so it is left out of the result rather than reported as 0.
//
// Every id must be a full 64-hex id, and they follow "--", like every id
// handed to docker here.
func (m Manager) Memory(ctx context.Context, ids []string) (map[string]uint64, error) {
	out := map[string]uint64{}
	if len(ids) == 0 {
		return out, nil
	}
	for _, id := range ids {
		if !containerID.MatchString(id) {
			return nil, fmt.Errorf("container: %q is not a container id", id)
		}
	}
	stdout, ids, err := m.dockerSurviving(ctx, "docker stats",
		[]string{"stats", "--no-stream", "--no-trunc", "--format", "{{json .}}", "--"}, ids)
	if err != nil || len(ids) == 0 {
		return out, err
	}
	want := map[string]bool{}
	for _, id := range ids {
		want[id] = true
	}
	dec := json.NewDecoder(bytes.NewReader(stdout))
	for dec.More() {
		var line struct {
			ID       string
			MemUsage string
		}
		if err := dec.Decode(&line); err != nil {
			return nil, fmt.Errorf("docker stats: %w", err)
		}
		if !want[line.ID] {
			// Asked by full id with --no-trunc, so an answer about anything
			// else means the contract moved.
			return nil, fmt.Errorf("docker stats: an answer for %q, which was not asked about", line.ID)
		}
		used, limit, err := ParseMemUsage(line.MemUsage)
		if err != nil {
			return nil, fmt.Errorf("docker stats: %w", err)
		}
		if limit == 0 {
			continue // not running: no reading, which is not a reading of zero
		}
		out[line.ID] = used
	}
	return out, nil
}

var (
	memUsage  = regexp.MustCompile(`^(\S+) / (\S+)$`)
	dockerQty = regexp.MustCompile(`^([0-9]+(?:\.[0-9]+)?)(B|KiB|MiB|GiB|TiB|PiB|EiB)$`)
	binary    = map[string]float64{"B": 1, "KiB": 1 << 10, "MiB": 1 << 20, "GiB": 1 << 30,
		"TiB": 1 << 40, "PiB": 1 << 50, "EiB": 1 << 60}
)

// ParseMemUsage reads `docker stats`' MemUsage, "<used> / <limit>", each half
// as Docker's units.BytesSize writes it. Anything else is an error.
func ParseMemUsage(s string) (used, limit uint64, err error) {
	m := memUsage.FindStringSubmatch(s)
	if m == nil {
		return 0, 0, fmt.Errorf("MemUsage %q is not \"<used> / <limit>\"", s)
	}
	if used, err = parseDockerQty(m[1]); err != nil {
		return 0, 0, err
	}
	if limit, err = parseDockerQty(m[2]); err != nil {
		return 0, 0, err
	}
	return used, limit, nil
}

func parseDockerQty(s string) (uint64, error) {
	m := dockerQty.FindStringSubmatch(s)
	if m == nil {
		return 0, fmt.Errorf("%q is not a size in Docker's binary units", s)
	}
	f, err := strconv.ParseFloat(m[1], 64)
	if err != nil {
		return 0, err
	}
	v := f * binary[m[2]]
	if v >= math.MaxUint64 {
		return 0, fmt.Errorf("%q is out of range", s)
	}
	return uint64(math.Round(v)), nil
}

// WritableSizes reads each container's writable layer — what the container
// has written to its own filesystem, outside the clone's bind mount: packages
// installed after create, caches, files in /tmp — from
// `docker inspect --size`'s SizeRw, structured JSON. A rebuild or a delete
// frees it, which is why the card's disk figure counts it. The daemon computes
// it by walking the layer, so it is asked for at the disk cadence only.
func (m Manager) WritableSizes(ctx context.Context, ids []string) (map[string]uint64, error) {
	out := map[string]uint64{}
	if len(ids) == 0 {
		return out, nil
	}
	for _, id := range ids {
		if !containerID.MatchString(id) {
			return nil, fmt.Errorf("container: %q is not a container id", id)
		}
	}
	stdout, ids, err := m.dockerSurviving(ctx, "docker inspect --size",
		[]string{"inspect", "--type", "container", "--size", "--"}, ids)
	if err != nil || len(ids) == 0 {
		return out, err
	}
	var all []struct {
		ID     string `json:"Id"`
		SizeRw *int64
	}
	if err := json.Unmarshal(stdout, &all); err != nil {
		return nil, fmt.Errorf("docker inspect --size: %w", err)
	}
	for _, c := range all {
		if c.SizeRw == nil || *c.SizeRw < 0 {
			return nil, fmt.Errorf("docker inspect --size: no SizeRw for %q", c.ID)
		}
		out[c.ID] = uint64(*c.SizeRw)
	}
	return out, nil
}

// noSuchContainer is the daemon's refusal for an id that is gone:
// "Error response from daemon: No such container: <id>".
var noSuchContainer = regexp.MustCompile(`(?m)No such container: ([0-9a-f]{64})\s*$`)

// dockerSurviving runs docker with args and ids, and when it fails only
// because some of the ids are gone — a delete or a rebuild between the listing
// and this call, measured on Docker 29.8.2: `stats` then prints nothing for
// the others, and `inspect` exits 1 — runs it once more with the ids that are
// left, rather than failing every workspace's reading for one that vanished.
// It returns stdout and the ids it was answered for.
func (m Manager) dockerSurviving(ctx context.Context, what string, args, ids []string) ([]byte, []string, error) {
	for attempt := 0; ; attempt++ {
		var stdout, stderr bytes.Buffer
		res := m.Run.Run(ctx, subproc.Cmd{Name: "docker", Args: append(append([]string{}, args...), ids...),
			Stdout: limit(&stdout, 16<<20), Stderr: limit(&stderr, 64<<10)})
		err := failed(what, res, &stderr)
		if err == nil {
			return stdout.Bytes(), ids, nil
		}
		gone := map[string]bool{}
		for _, mm := range noSuchContainer.FindAllStringSubmatch(stderr.String(), -1) {
			gone[mm[1]] = true
		}
		var left []string
		for _, id := range ids {
			if !gone[id] {
				left = append(left, id)
			}
		}
		if attempt > 0 || res.Err != nil || len(gone) == 0 || len(left) == len(ids) {
			return nil, nil, err
		}
		if len(left) == 0 {
			return nil, nil, nil
		}
		ids = left
	}
}
