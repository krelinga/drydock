package container

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func readFixture(t *testing.T, path string) []Listener {
	t.Helper()
	f, err := os.Open(filepath.Join("testdata", "proc", path))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	ls, err := ParseNetTCP(f)
	if err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	return ls
}

func listenersString(ls []Listener) string {
	var b []string
	for _, l := range ls {
		b = append(b, netip.AddrPortFrom(l.Addr, uint16(l.Port)).String())
	}
	return strings.Join(b, " ")
}

// TestParseNetTCPSynthetic: LISTEN only — an established socket and a
// TIME_WAIT one are not offers — each (address, port) once though two
// sockets share it (SO_REUSEPORT), IPv4 words and IPv6 words in host order,
// by port then address.
func TestParseNetTCPSynthetic(t *testing.T) {
	if got, want := listenersString(readFixture(t, "synthetic/net/tcp")),
		"172.18.0.2:5173 0.0.0.0:8080 127.0.0.1:9090"; got != want {
		t.Errorf("tcp\n got %s\nwant %s", got, want)
	}
	if got, want := listenersString(readFixture(t, "synthetic/net/tcp6")),
		"[::1]:3000 [::]:5173 [::ffff:127.0.0.1]:6000 [2001:db8::1]:8081"; got != want {
		t.Errorf("tcp6\n got %s\nwant %s", got, want)
	}
	loop := map[string]bool{}
	for _, l := range append(readFixture(t, "synthetic/net/tcp"), readFixture(t, "synthetic/net/tcp6")...) {
		loop[netip.AddrPortFrom(l.Addr, uint16(l.Port)).String()] = l.Loopback()
	}
	for addr, want := range map[string]bool{
		"127.0.0.1:9090": true, "[::1]:3000": true, "[::ffff:127.0.0.1]:6000": true, // a mapped loopback is loopback
		"0.0.0.0:8080": false, "[::]:5173": false, "172.18.0.2:5173": false, "[2001:db8::1]:8081": false,
	} {
		if loop[addr] != want {
			t.Errorf("%s loopback = %v; want %v", addr, loop[addr], want)
		}
	}
}

// TestParseNetTCPRecorded: a table the kernel wrote, recorded on this
// devcontainer with four `python3 -m http.server`s bound to 127.0.0.1:39091,
// 0.0.0.0:39092, [::]:39093 and [::1]:39094 beside whatever else was
// listening and a dozen established connections.
func TestParseNetTCPRecorded(t *testing.T) {
	all := append(readFixture(t, "recorded/net/tcp"), readFixture(t, "recorded/net/tcp6")...)
	found := map[int]string{}
	for _, l := range all {
		found[l.Port] = l.Addr.String()
	}
	for port, addr := range map[int]string{39091: "127.0.0.1", 39092: "0.0.0.0", 39093: "::", 39094: "::1"} {
		if found[port] != addr {
			t.Errorf("port %d bound to %q; want %s", port, found[port], addr)
		}
	}
	// 5 listening in tcp, 6 in tcp6: the 12 established ones are not here.
	if len(all) != 11 {
		t.Errorf("%d listeners; want 11: %s", len(all), listenersString(all))
	}
}

// TestParseNetTCPRefusesWhatItCannotRead: a changed format is an error, never
// a partial table — the control is the synthetic table, which parses.
func TestParseNetTCPRefusesWhatItCannotRead(t *testing.T) {
	header := "  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode\n"
	good := "   0: 00000000:1F90 00000000:0000 0A 00000000:00000000 00:00000000 00000000  1000        0 1 1\n"
	if ls, err := ParseNetTCP(strings.NewReader(header + good)); err != nil || len(ls) != 1 || ls[0].Port != 8080 {
		t.Fatalf("control: %v, %v", ls, err)
	}
	for name, table := range map[string]string{
		"empty":            "",
		"no header":        good,
		"short line":       header + "   0: 00000000:1F90\n",
		"no colon on sl":   header + "   0 00000000:1F90 00000000:0000 0A x\n",
		"address not hex":  header + "   0: 0000000G:1F90 00000000:0000 0A x\n",
		"port not hex":     header + "   0: 00000000:1FZ0 00000000:0000 0A x\n",
		"address length":   header + "   0: 000000:1F90 00000000:0000 0A x\n",
		"port length":      header + "   0: 00000000:1F900 00000000:0000 0A x\n",
		"state length":     header + "   0: 00000000:1F90 00000000:0000 A x\n",
		"after good lines": header + good + "garbage\n",
	} {
		if ls, err := ParseNetTCP(strings.NewReader(table)); err == nil {
			t.Errorf("%s: parsed as %v", name, ls)
		}
	}
	// More listening sockets than a scan describes is refused, not cut.
	var b strings.Builder
	b.WriteString(header)
	for i := 0; i <= MaxListeners; i++ {
		fmt.Fprintf(&b, "   %d: 00000000:%04X 00000000:0000 0A 0\n", i, i+1)
	}
	if _, err := ParseNetTCP(strings.NewReader(b.String())); !errors.Is(err, ErrTableTooLarge) {
		t.Errorf("%d listeners: %v; want ErrTableTooLarge", MaxListeners+1, err)
	}
}

// pidDocker is a docker whose container idA is running on a bridge with the
// PID each inspect reads from <dir>/pids, one line per inspect in order (the
// last repeated), so a test can restart the container between calls.
func pidDocker(dir string) string {
	inspect := `[{"Id":"` + idA + `","State":{"Status":"running","Running":true,"Pid":__PID__},` +
		`"Config":{"Labels":{"drydock.test.workspace":"` + wsID + `"}},` +
		`"NetworkSettings":{"Networks":{` + bridge("172.18.0.2") + `}}}]`
	return `case "$1" in
ps) echo ` + idA + ` ;;
inspect)
  n=$(cat ` + dir + `/n 2>/dev/null || echo 0); n=$((n+1)); echo $n > ` + dir + `/n
  pid=$(sed -n "${n}p" ` + dir + `/pids); [ -n "$pid" ] || pid=$(tail -n 1 ` + dir + `/pids)
  printf '%s' '` + inspect + `' | sed "s/__PID__/$pid/" ;;
network) printf '%s' '[{"Id":"` + netBridge + `","Driver":"bridge"}]' ;;
esac`
}

// procTree makes <root>/<pid>/net/tcp listing one 0.0.0.0 listener per port.
func procTree(t *testing.T, root string, pid int, ports ...int) {
	t.Helper()
	dir := filepath.Join(root, strconv.Itoa(pid), "net")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	b := "  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode\n"
	for i, p := range ports {
		b += fmt.Sprintf("   %d: 00000000:%04X 00000000:0000 0A 0\n", i, p)
	}
	if err := os.WriteFile(filepath.Join(dir, "tcp"), []byte(b), 0o644); err != nil {
		t.Fatal(err)
	}
}

func setPids(t *testing.T, dir string, pids ...int) {
	t.Helper()
	var s []string
	for _, p := range pids {
		s = append(s, strconv.Itoa(p))
	}
	os.Remove(filepath.Join(dir, "n"))
	if err := os.WriteFile(filepath.Join(dir, "pids"), []byte(strings.Join(s, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestListenersFollowTheContainersPID: the PID is resolved from Docker on
// every call — a restarted container, at a new PID, is read at the new one,
// and the old PID's table (still there: the kernel may have given that PID
// to anything) is never read again. Mutation-checked: Listeners remembering
// the first PID it resolved for a workspace reads 8080 the second time.
func TestListenersFollowTheContainersPID(t *testing.T) {
	ctx := context.Background()
	state := t.TempDir()
	run, dir := fakes(t, map[string]string{"docker": pidDocker(state)})
	proc := t.TempDir()
	procTree(t, proc, 100, 8080)
	procTree(t, proc, 200, 5173)
	m := withHost(Manager{Run: run, LabelPrefix: "drydock.test", ProcRoot: proc})

	setPids(t, state, 100)
	ls, err := m.Listeners(ctx, wsID)
	if err != nil || listenersString(ls) != "0.0.0.0:8080" {
		t.Fatalf("first scan = %s, %v; want 0.0.0.0:8080", listenersString(ls), err)
	}
	setPids(t, state, 200) // docker restart: same container, a new process
	ls, err = m.Listeners(ctx, wsID)
	if err != nil || listenersString(ls) != "0.0.0.0:5173" {
		t.Fatalf("after the restart = %s, %v; want 0.0.0.0:5173 (the new PID's table)", listenersString(ls), err)
	}
	// Address, then the same container inspected again: four docker calls.
	got := strings.Join(argv(t, dir, "docker"), " ")
	one := "ps --quiet --no-trunc --filter status=running --filter label=drydock.test.workspace=" + wsID +
		" -- inspect --type container -- " + idA + " -- network inspect -- " + netBridge +
		" -- inspect --type container -- " + idA + " -- "
	if want := strings.TrimSuffix(one+one, " -- "); strings.TrimSpace(got) != want {
		t.Errorf("argv\n got %s\nwant %s", got, want)
	}
}

// TestListenersThrowAwayARaceWithARestart: a PID that changed between the
// resolution and the check after the read is ErrMoved, and nothing is
// returned of what was read — it may be another process's namespace. So is
// a process gone from /proc. The control is the same PID both times.
func TestListenersThrowAwayARaceWithARestart(t *testing.T) {
	ctx := context.Background()
	state := t.TempDir()
	run, _ := fakes(t, map[string]string{"docker": pidDocker(state)})
	proc := t.TempDir()
	procTree(t, proc, 100, 8080)
	m := withHost(Manager{Run: run, LabelPrefix: "drydock.test", ProcRoot: proc})

	setPids(t, state, 100, 100)
	if ls, err := m.Listeners(ctx, wsID); err != nil || len(ls) != 1 {
		t.Fatalf("control: %v, %v", ls, err)
	}
	setPids(t, state, 100, 300)
	if ls, err := m.Listeners(ctx, wsID); !errors.Is(err, ErrMoved) || ls != nil {
		t.Errorf("PID changed under the read: %v, %v; want ErrMoved and nothing", ls, err)
	}
	setPids(t, state, 400) // no /proc/400: the process is gone
	if ls, err := m.Listeners(ctx, wsID); !errors.Is(err, ErrMoved) || ls != nil {
		t.Errorf("process gone: %v, %v; want ErrMoved", ls, err)
	}
}

// TestListenersReadNothingThatIsNotTheContainersOwn: a container on the
// host's network has no address of its own, and its PID's namespace is the
// host's — the table is never read (the tree has one to read, the control
// being the same container on a bridge), and no container is ErrNotRunning.
func TestListenersReadNothingThatIsNotTheContainersOwn(t *testing.T) {
	ctx := context.Background()
	proc := t.TempDir()
	procTree(t, proc, 100, 22)
	withPid := func(running bool, networks string) string {
		return strings.Replace(inspectJSON(idA, wsID, running, networks), `"Running":`+strconv.FormatBool(running),
			`"Running":`+strconv.FormatBool(running)+`,"Pid":100`, 1)
	}
	for _, c := range []struct {
		name    string
		docker  string
		want    error
		listens string
	}{
		{"control: a bridge", addressDocker(idA, withPid(true, bridge("172.18.0.2"))), nil, "0.0.0.0:22"},
		{"the host's network", addressDocker(idA, withPid(true, nw("host", netHost, "", "", ""))), ErrNoAddress, ""},
		{"no network", addressDocker(idA, withPid(true, ``)), ErrNoAddress, ""},
		{"no container", addressDocker("", ""), ErrNotRunning, ""},
		{"no process", addressDocker(idA, inspectJSON(idA, wsID, true, bridge("172.18.0.2"))), errors.New("no pid"), ""},
	} {
		run, _ := fakes(t, map[string]string{"docker": c.docker})
		ls, err := withHost(Manager{Run: run, LabelPrefix: "drydock.test", ProcRoot: proc}).Listeners(ctx, wsID)
		switch {
		case c.want == nil:
			if err != nil || listenersString(ls) != c.listens {
				t.Errorf("%s: %s, %v; want %s", c.name, listenersString(ls), err, c.listens)
			}
		case err == nil || ls != nil:
			t.Errorf("%s: %s, %v; want an error and nothing", c.name, listenersString(ls), err)
		case c.want.Error() != "no pid" && !errors.Is(err, c.want):
			t.Errorf("%s: %v; want %v", c.name, err, c.want)
		}
	}
}
