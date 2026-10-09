package container

import (
	"bufio"
	"context"
	endian "encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/netip"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// Listener is one listening TCP socket in a container's network namespace:
// the port and the address it is bound to (0.0.0.0, ::, 127.0.0.1, …). The
// bind address is the diagnosis (PF §8.2): a loopback listener is reachable
// only from inside the container.
type Listener struct {
	Port int
	Addr netip.Addr
}

// Loopback reports whether only the container itself can reach it.
func (l Listener) Loopback() bool { return l.Addr.Unmap().IsLoopback() }

// MaxListeners bounds one read of a socket table: more listening sockets than
// this is not a dev container the scan can describe, and is refused as a
// whole rather than truncated to whichever came first.
const MaxListeners = 4096

// maxTable bounds the bytes read from one net/tcp file. A line is about 150
// bytes, so this is a table of about 200,000 sockets, established ones
// included, which is far past anything a dev container holds.
const maxTable = 32 << 20

// ErrTableTooLarge is a socket table past maxTable or MaxListeners.
var ErrTableTooLarge = errors.New("container: the socket table is too large to scan")

// tcpListen is the kernel's TCP_LISTEN state, as /proc/net/tcp prints it.
const tcpListen = "0A"

// ParseNetTCP reads a /proc/<pid>/net/tcp or tcp6 table and returns its
// listening sockets (state 0A), each once, by port then address. An address
// is the kernel's: each 32-bit word printed in host byte order, the port in
// network order. A line that does not parse is an error — the format changed,
// and a scan that skipped what it could not read would report a partial table
// as the whole one (classifiers refuse rather than guess).
func ParseNetTCP(r io.Reader) ([]Listener, error) {
	sc := bufio.NewScanner(io.LimitReader(r, maxTable+1))
	sc.Buffer(make([]byte, 0, 4096), 64<<10)
	seen := map[Listener]bool{}
	var out []Listener
	read := 0
	first := true
	for sc.Scan() {
		line := sc.Text()
		read += len(line) + 1
		if read > maxTable {
			return nil, ErrTableTooLarge
		}
		if first {
			first = false
			if f := strings.Fields(line); len(f) < 4 || f[0] != "sl" || f[1] != "local_address" {
				return nil, fmt.Errorf("container: socket table header %q is not the kernel's", line)
			}
			continue
		}
		if strings.TrimSpace(line) == "" {
			continue
		}
		f := strings.Fields(line)
		if len(f) < 4 || !strings.HasSuffix(f[0], ":") {
			return nil, fmt.Errorf("container: socket table line %q does not parse", line)
		}
		if len(f[3]) != 2 {
			return nil, fmt.Errorf("container: socket table line %q has no state", line)
		}
		local, err := parseSocketAddr(f[1])
		if err != nil {
			return nil, fmt.Errorf("container: socket table line %q: %w", line, err)
		}
		if f[3] != tcpListen {
			continue
		}
		if !seen[local] {
			seen[local] = true
			out = append(out, local)
			if len(out) > MaxListeners {
				return nil, ErrTableTooLarge
			}
		}
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("container: reading the socket table: %w", err)
	}
	if first {
		return nil, errors.New("container: the socket table is empty, not even a header")
	}
	sortListeners(out)
	return out, nil
}

func sortListeners(ls []Listener) {
	sort.Slice(ls, func(i, j int) bool {
		if ls[i].Port != ls[j].Port {
			return ls[i].Port < ls[j].Port
		}
		return ls[i].Addr.Less(ls[j].Addr)
	})
}

// parseSocketAddr reads "0100007F:2382" (IPv4) or a 32-digit IPv6 address and
// a port: the address as four host-order 32-bit words, the port big-endian.
func parseSocketAddr(s string) (Listener, error) {
	host, port, ok := strings.Cut(s, ":")
	if !ok || len(port) != 4 || (len(host) != 8 && len(host) != 32) {
		return Listener{}, fmt.Errorf("address %q is not the kernel's", s)
	}
	p, err := strconv.ParseUint(port, 16, 16)
	if err != nil {
		return Listener{}, fmt.Errorf("port in %q: %w", s, err)
	}
	raw, err := hex.DecodeString(host)
	if err != nil {
		return Listener{}, fmt.Errorf("address in %q: %w", s, err)
	}
	b := make([]byte, len(raw))
	for i := 0; i < len(raw); i += 4 {
		// The kernel printed each word with %08X of a value in host order;
		// put it back in memory order, which is network order.
		endian.NativeEndian.PutUint32(b[i:], endian.BigEndian.Uint32(raw[i:]))
	}
	addr, _ := netip.AddrFromSlice(b)
	return Listener{Port: int(p), Addr: addr}, nil
}

// procRoot is ProcRoot or /proc.
func (m Manager) procRoot() string {
	if m.ProcRoot != "" {
		return m.ProcRoot
	}
	return "/proc"
}

// Listeners reads what the workspace's container is listening on, now (PF
// §8.2): Address resolves the container by label — the one running
// container, with an address of its own on a bridge network — and with it
// its PID; the socket table of that PID's network namespace is read from
// <ProcRoot>/<pid>/net/tcp and tcp6; and the same container is inspected
// again, still running under the same PID, or the read is thrown away as
// ErrMoved: a container that died in between has given its PID back to the
// kernel, and what was read may be an unrelated process's namespace.
//
// The PID is never remembered: every call resolves it, so a restarted
// container — a new PID — is followed, and a stale one is never read. Nothing
// runs inside the container; this is a file read from the host, which a
// user in the docker group may make (/proc/<pid>/net sits outside the ptrace
// check).
//
// A container on the host's network, or none, is ErrNoAddress: its namespace
// is the host's (or empty), and listing the host's listeners as a
// workspace's would offer the host for preview. No running container is
// ErrNotRunning, which the scan reads as nothing listening.
func (m Manager) Listeners(ctx context.Context, workspaceID string) ([]Listener, error) {
	a, err := m.Address(ctx, workspaceID)
	if err != nil {
		return nil, err
	}
	if a.Pid <= 0 {
		return nil, fmt.Errorf("docker inspect: running container %s reports no process", a.ContainerID)
	}
	dir := filepath.Join(m.procRoot(), strconv.Itoa(a.Pid))
	var out []Listener
	for _, name := range []string{"tcp", "tcp6"} {
		ls, err := readTable(filepath.Join(dir, "net", name))
		if errors.Is(err, fs.ErrNotExist) {
			if _, serr := os.Stat(dir); serr != nil {
				// The process is gone: the container stopped under the
				// read, and there is nothing of its to read.
				return nil, ErrMoved
			}
			if name == "tcp6" {
				continue // a kernel without IPv6
			}
		}
		if err != nil {
			return nil, err
		}
		out = append(out, ls...)
	}
	c, err := m.inspectOne(ctx, workspaceID, a.ContainerID)
	if errors.Is(err, ErrNotRunning) || err == nil && (!c.State.Running || c.State.Pid != a.Pid) {
		return nil, ErrMoved
	}
	if err != nil {
		return nil, err
	}
	if len(out) > MaxListeners {
		return nil, ErrTableTooLarge
	}
	sortListeners(out)
	return out, nil
}

func readTable(path string) ([]Listener, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return ParseNetTCP(f)
}
