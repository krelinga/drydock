package container

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"sort"

	"github.com/krelinga/drydock/internal/subproc"
)

// Address is where a workspace's container can be dialled now: which
// container, and its address on a Docker network (port forwarding §8.1). It is
// a resolution, not a fact to keep — the preview proxy asks for one before
// every dial and confirms it after (Confirm), and never dials a remembered one.
type Address struct {
	ContainerID string
	IP          netip.Addr
	// Pid is the container's init process on this host (.State.Pid), from
	// the same inspect as IP: the discovery scan reads the container's
	// socket table through it (PF §8.2). A resolution, never kept — a PID is
	// reused by the kernel like any other once its container dies. Confirm
	// does not compare it: a connection is bound to the container and its
	// address, and the scan makes its own check (Listeners).
	Pid int
}

var (
	// ErrNotRunning: no running container carries the workspace's label
	// right now, or the one that did has stopped since it was listed. The
	// preview proxy treats the workspace as stopped (PF §8.1), never as a
	// cache miss to be filled from a last known address.
	ErrNotRunning = errors.New("container: no running container carries the workspace's label")
	// ErrAmbiguous: more than one running container carries the label — a
	// rebuild that failed half-way can leave two (Find). Which one the
	// device meant is not a question to answer by guessing.
	ErrAmbiguous = errors.New("container: more than one running container carries the workspace's label")
	// ErrNoAddress: the container is running but has no usable address on
	// a Docker network — `--network=host` or `none`, say. There is nothing
	// to dial that is the container's own and not the host's.
	ErrNoAddress = errors.New("container: the workspace's container has no address on a Docker network")
	// ErrMoved: Confirm found the container stopped, gone, or at another
	// address since it was resolved.
	ErrMoved = errors.New("container: the container is no longer at the address it was resolved to")
)

// Address resolves the workspace's running container by label, now: `docker
// ps` for the running ids carrying this prefix's workspace label with this
// workspace's id, then `docker inspect` of the one it finds (a full 64-hex id,
// after "--"). Docker is the truth (§6), and per dial (PF §8.1), because a
// container can die unobserved and Docker is then free to hand its address to
// another workspace's container.
func (m Manager) Address(ctx context.Context, workspaceID string) (Address, error) {
	ids, err := m.findRunning(ctx, workspaceID)
	if err != nil {
		return Address{}, err
	}
	switch len(ids) {
	case 0:
		return Address{}, ErrNotRunning
	case 1:
	default:
		return Address{}, ErrAmbiguous
	}
	return m.addressOf(ctx, workspaceID, ids[0])
}

// Confirm asks again, after a dial, whether a is still true: the same
// container, still running, still carrying the workspace's label, still at
// the same address. A connection made to an address is bound to the container
// resolved before it only if that container still holds the address after it:
// had it died in between and its address gone to another container, it would
// not be running now at that address (PF §8.1, "bound to that freshly-resolved
// container identity").
func (m Manager) Confirm(ctx context.Context, workspaceID string, a Address) error {
	got, err := m.addressOf(ctx, workspaceID, a.ContainerID)
	if errors.Is(err, ErrNotRunning) || errors.Is(err, ErrNoAddress) {
		return ErrMoved
	}
	if err != nil {
		return err
	}
	if got.ContainerID != a.ContainerID || got.IP != a.IP {
		return ErrMoved
	}
	return nil
}

type inspectNetwork struct {
	NetworkID         string
	IPAddress         string
	GlobalIPv6Address string
	Gateway           string
	IPv6Gateway       string
}

type inspectAddress struct {
	ID    string `json:"Id"`
	State struct {
		Running bool
		Pid     int
	}
	Config struct {
		Labels map[string]string
	}
	NetworkSettings struct {
		Networks map[string]inspectNetwork
	}
}

// addressOf inspects one container and reads its address and PID. Structured
// JSON, never a table; a container removed since it was listed is not running.
func (m Manager) addressOf(ctx context.Context, workspaceID, id string) (Address, error) {
	c, err := m.inspectOne(ctx, workspaceID, id)
	if err != nil {
		return Address{}, err
	}
	if !c.State.Running {
		return Address{}, ErrNotRunning
	}
	local, err := m.localAddrs()
	if err != nil {
		return Address{}, fmt.Errorf("container: reading this host's addresses: %w", err)
	}
	cands := candidates(c, local)
	if len(cands) == 0 {
		return Address{}, ErrNoAddress
	}
	drivers, err := m.networkDrivers(ctx, cands)
	if err != nil {
		return Address{}, err
	}
	for _, cd := range cands {
		if drivers[cd.network] == "bridge" {
			return Address{ContainerID: id, IP: cd.ip, Pid: c.State.Pid}, nil
		}
	}
	return Address{}, fmt.Errorf("%w: none of its addresses is on a bridge network", ErrNoAddress)
}

// inspectOne is `docker inspect` of one container, by full id after "--",
// checked to be the container asked about and to carry the workspace's label
// with this id. A container removed since it was listed is ErrNotRunning.
func (m Manager) inspectOne(ctx context.Context, workspaceID, id string) (inspectAddress, error) {
	if !workspaceIDPattern.MatchString(workspaceID) {
		return inspectAddress{}, fmt.Errorf("container: %q is not a workspace id", workspaceID)
	}
	if !containerID.MatchString(id) {
		return inspectAddress{}, fmt.Errorf("container: %q is not a container id", id)
	}
	var out, stderr bytes.Buffer
	res := m.Run.Run(ctx, subproc.Cmd{Name: "docker", Args: []string{"inspect", "--type", "container", "--", id},
		Stdout: limit(&out, 4<<20), Stderr: limit(&stderr, 64<<10)})
	if res.Err == nil && res.ExitCode != 0 && noSuchContainer.MatchString(stderr.String()) {
		return inspectAddress{}, ErrNotRunning
	}
	if err := failed("docker inspect", res, &stderr); err != nil {
		return inspectAddress{}, err
	}
	var all []inspectAddress
	if err := json.Unmarshal(out.Bytes(), &all); err != nil {
		return inspectAddress{}, fmt.Errorf("docker inspect: %w", err)
	}
	if len(all) != 1 || all[0].ID != id {
		return inspectAddress{}, fmt.Errorf("docker inspect: asked about %s, answered about something else", id)
	}
	c := all[0]
	if c.Config.Labels[m.key(LabelWorkspace)] != workspaceID {
		// Listed by this label a moment ago; without it now, the contract
		// moved, and dialling it would be guessing.
		return inspectAddress{}, fmt.Errorf("docker inspect: container %s does not carry %s=%s", id, m.key(LabelWorkspace), workspaceID)
	}
	return c, nil
}

type candidate struct {
	ip      netip.Addr
	network string // NetworkID
}

// candidates are the container's own addresses in the order they are tried —
// IPv4 before IPv6, and among a container's networks by name, so the choice
// is stable — leaving out every address that is not one to dial as the
// container's: one usableIP refuses, the network's own gateway (the host's
// side of a bridge), and any address this host holds itself. A macvlan or
// ipvlan network with an operator-approved --ip can give a container the
// host's own address, or another LAN machine's (measured in review: an
// ipvlan --ip naming the host's eth0 address came back from inspect as the
// container's), and the proxy must never dial the host (PF §10.6).
func candidates(c inspectAddress, local map[netip.Addr]bool) []candidate {
	names := make([]string, 0, len(c.NetworkSettings.Networks))
	for n := range c.NetworkSettings.Networks {
		names = append(names, n)
	}
	sort.Strings(names)
	var out []candidate
	for _, v6 := range []bool{false, true} {
		for _, n := range names {
			nw := c.NetworkSettings.Networks[n]
			raw, gw := nw.IPAddress, nw.Gateway
			if v6 {
				raw, gw = nw.GlobalIPv6Address, nw.IPv6Gateway
			}
			ip, err := netip.ParseAddr(raw)
			if err != nil {
				continue
			}
			ip = ip.Unmap()
			if ip.Is4() == v6 || !UsableIP(ip) || local[ip] || !networkID.MatchString(nw.NetworkID) {
				continue
			}
			if g, err := netip.ParseAddr(gw); err == nil && g.Unmap() == ip {
				continue
			}
			out = append(out, candidate{ip: ip, network: nw.NetworkID})
		}
	}
	return out
}

var networkID = containerID // a network id is 64 hex too

// networkDrivers asks Docker for the driver of each candidate's network, in
// one `docker network inspect` of full ids after "--". Only a bridge network
// is dialled: it is the one driver whose containers sit behind a host-side
// bridge Docker made for them, so the address is the container's and nothing
// else's. macvlan and ipvlan put the container on the LAN beside the host,
// where an approved --ip can name any machine there; overlay and other
// drivers reach other hosts. A Compose devcontainer's network is a bridge too.
func (m Manager) networkDrivers(ctx context.Context, cands []candidate) (map[string]string, error) {
	var ids []string
	seen := map[string]bool{}
	for _, cd := range cands {
		if !seen[cd.network] {
			seen[cd.network] = true
			ids = append(ids, cd.network)
		}
	}
	var out, stderr bytes.Buffer
	res := m.Run.Run(ctx, subproc.Cmd{Name: "docker", Args: append([]string{"network", "inspect", "--"}, ids...),
		Stdout: limit(&out, 4<<20), Stderr: limit(&stderr, 64<<10)})
	if err := failed("docker network inspect", res, &stderr); err != nil {
		return nil, err
	}
	var nets []struct {
		ID     string `json:"Id"`
		Driver string
	}
	if err := json.Unmarshal(out.Bytes(), &nets); err != nil {
		return nil, fmt.Errorf("docker network inspect: %w", err)
	}
	drivers := map[string]string{}
	for _, n := range nets {
		if seen[n.ID] {
			drivers[n.ID] = n.Driver
		}
	}
	return drivers, nil
}

// localAddrs is every address this host holds, asked at each resolution —
// interfaces come and go — through LocalAddrs when a test sets it.
func (m Manager) localAddrs() (map[netip.Addr]bool, error) {
	var list []netip.Addr
	if m.LocalAddrs != nil {
		l, err := m.LocalAddrs()
		if err != nil {
			return nil, err
		}
		list = l
	} else {
		addrs, err := net.InterfaceAddrs()
		if err != nil {
			return nil, err
		}
		for _, a := range addrs {
			if p, err := netip.ParsePrefix(a.String()); err == nil {
				list = append(list, p.Addr())
			}
		}
	}
	out := map[netip.Addr]bool{}
	for _, a := range list {
		out[a.Unmap().WithZone("")] = true
	}
	return out, nil
}

// UsableIP is the rule an address must pass to be dialled as a container's:
// a unicast address a Docker network could have assigned. Loopback and
// unspecified are the host itself — the proxy dials one container port and
// nothing on the host (PF §10.6) — and link-local, multicast and a zone are
// never a container's network address. (The host's other addresses, and a
// network's gateway, are refused beside it, in candidates.)
func UsableIP(ip netip.Addr) bool {
	return ip.IsValid() && ip.Zone() == "" && !ip.IsLoopback() && !ip.IsUnspecified() &&
		!ip.IsMulticast() && !ip.IsLinkLocalUnicast() && !ip.IsInterfaceLocalMulticast()
}
