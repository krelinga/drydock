package container

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Network ids the fake docker knows the drivers of.
var (
	netBridge  = strings.Repeat("b1", 32)
	netBridge2 = strings.Repeat("b2", 32)
	netIPvlan  = strings.Repeat("e1", 32)
	netMacvlan = strings.Repeat("e2", 32)
	netHost    = strings.Repeat("e3", 32)
)

// hostAddr is the address the test's "host" holds (LocalAddrs).
const hostAddr = "10.0.0.9"

// inspectJSON is `docker inspect` of one container, as Docker 29 shapes the
// fields Address reads.
func inspectJSON(id, ws string, running bool, networks string) string {
	r := "false"
	if running {
		r = "true"
	}
	return `[{"Id":"` + id + `","State":{"Status":"running","Running":` + r + `},` +
		`"Config":{"Labels":{"drydock.test.workspace":"` + ws + `"}},` +
		`"NetworkSettings":{"Networks":{` + networks + `}}}]`
}

// nw is one network's entry in a container's inspect.
func nw(name, id, ip, ip6, gw string) string {
	return `"` + name + `":{"NetworkID":"` + id + `","IPAddress":"` + ip + `","GlobalIPv6Address":"` + ip6 + `","Gateway":"` + gw + `"}`
}

func bridge(ip string) string { return nw("bridge", netBridge, ip, "", "172.18.0.1") }

// addressDocker is a docker that lists ids for `ps`, prints inspect for
// `inspect`, and knows each test network's driver for `network inspect`.
func addressDocker(ids, inspect string) string {
	return `case "$1" in
ps) printf '%s' '` + ids + `' ;;
inspect) printf '%s' '` + inspect + `' ;;
network) printf '%s' '[{"Id":"` + netBridge + `","Driver":"bridge"},{"Id":"` + netBridge2 + `","Driver":"bridge"},` +
		`{"Id":"` + netIPvlan + `","Driver":"ipvlan"},{"Id":"` + netMacvlan + `","Driver":"macvlan"},{"Id":"` + netHost + `","Driver":"host"}]' ;;
esac`
}

func withHost(m Manager) Manager {
	m.LocalAddrs = func() ([]netip.Addr, error) {
		return []netip.Addr{netip.MustParseAddr("127.0.0.1"), netip.MustParseAddr(hostAddr), netip.MustParseAddr("fd00::9")}, nil
	}
	return m
}

// TestAddressAsksDockerByLabel: the exact argv — running containers by this
// prefix's workspace label with this workspace's id, then inspect of the one
// found and its network's driver, full ids after "--" — and the address read
// from its network.
func TestAddressAsksDockerByLabel(t *testing.T) {
	run, dir := fakes(t, map[string]string{"docker": addressDocker(idA+"\n", inspectJSON(idA, wsID, true, bridge("172.17.0.5")))})
	m := withHost(Manager{Run: run, LabelPrefix: "drydock.test"})
	a, err := m.Address(context.Background(), wsID)
	if err != nil || a.ContainerID != idA || a.IP != netip.MustParseAddr("172.17.0.5") {
		t.Fatalf("Address = %+v, %v", a, err)
	}
	got := strings.Join(argv(t, dir, "docker"), " ")
	want := "ps --quiet --no-trunc --filter status=running --filter label=drydock.test.workspace=" + wsID +
		" -- inspect --type container -- " + idA + " -- network inspect -- " + netBridge
	if strings.TrimSpace(got) != want {
		t.Errorf("argv\n got %s\nwant %s", got, want)
	}
}

// TestAddressRefusesWhatIsNotAContainersOwn: each way a resolution must not
// produce something to dial — above all the host itself, or another machine
// on its LAN — beside the controls that do.
func TestAddressRefusesWhatIsNotAContainersOwn(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name    string
		ids     string
		inspect string
		want    error  // nil with ip set: the address
		ip      string // the address wanted
	}{
		{"control: one running container on a bridge", idA, inspectJSON(idA, wsID, true, bridge("172.18.0.2")), nil, "172.18.0.2"},
		{"IPv6 only", idA, inspectJSON(idA, wsID, true, nw("v6", netBridge, "", "fd00::5", "")), nil, "fd00::5"},
		{"IPv4 before IPv6, first network by name", idA, inspectJSON(idA, wsID, true,
			nw("zeta", netBridge, "10.9.0.2", "", "")+","+nw("alpha", netBridge2, "", "fd00::6", "")+","+nw("beta", netBridge2, "10.8.0.2", "", "")), nil, "10.8.0.2"},
		{"a bridge after a LAN network", idA, inspectJSON(idA, wsID, true,
			nw("a-lan", netIPvlan, "10.1.0.5", "", "")+","+nw("b-bridge", netBridge, "172.18.0.2", "", "")), nil, "172.18.0.2"},
		{"a bridge after the host's own address", idA, inspectJSON(idA, wsID, true,
			nw("a", netBridge2, hostAddr, "", "")+","+nw("b", netBridge, "172.18.0.2", "", "")), nil, "172.18.0.2"},
		{"none running", "", "", ErrNotRunning, ""},
		{"two running", idA + "\n" + idB, "", ErrAmbiguous, ""},
		{"stopped since it was listed", idA, inspectJSON(idA, wsID, false, bridge("172.18.0.2")), ErrNotRunning, ""},
		{"host network", idA, inspectJSON(idA, wsID, true, nw("host", netHost, "", "", "")), ErrNoAddress, ""},
		{"no network", idA, inspectJSON(idA, wsID, true, ``), ErrNoAddress, ""},
		{"loopback", idA, inspectJSON(idA, wsID, true, bridge("127.0.0.1")), ErrNoAddress, ""},
		// Neither is an address the test's host holds, so only UsableIP's
		// loopback rule refuses them.
		{"loopback beyond .1", idA, inspectJSON(idA, wsID, true, bridge("127.0.0.2")), ErrNoAddress, ""},
		{"IPv6 loopback", idA, inspectJSON(idA, wsID, true, nw("v6", netBridge, "", "::1", "")), ErrNoAddress, ""},
		{"unspecified", idA, inspectJSON(idA, wsID, true, bridge("0.0.0.0")), ErrNoAddress, ""},
		{"link-local", idA, inspectJSON(idA, wsID, true, bridge("169.254.1.1")), ErrNoAddress, ""},
		{"multicast", idA, inspectJSON(idA, wsID, true, bridge("224.0.0.1")), ErrNoAddress, ""},
		{"not an address", idA, inspectJSON(idA, wsID, true, bridge("172.17.0.5:80")), ErrNoAddress, ""},
		{"the host's own address", idA, inspectJSON(idA, wsID, true, bridge(hostAddr)), ErrNoAddress, ""},
		{"the host's own IPv6 address", idA, inspectJSON(idA, wsID, true, nw("v6", netBridge, "", "fd00::9", "")), ErrNoAddress, ""},
		{"the network's gateway", idA, inspectJSON(idA, wsID, true, nw("bridge", netBridge, "172.18.0.1", "", "172.18.0.1")), ErrNoAddress, ""},
		{"an ipvlan network", idA, inspectJSON(idA, wsID, true, nw("lan", netIPvlan, "10.1.0.5", "", "10.1.0.1")), ErrNoAddress, ""},
		{"a macvlan network", idA, inspectJSON(idA, wsID, true, nw("lan", netMacvlan, "192.168.1.1", "", "")), ErrNoAddress, ""},
		{"no network id", idA, inspectJSON(idA, wsID, true, nw("bridge", "", "172.18.0.2", "", "")), ErrNoAddress, ""},
	}
	for _, c := range cases {
		run, _ := fakes(t, map[string]string{"docker": addressDocker(c.ids, c.inspect)})
		a, err := withHost(Manager{Run: run, LabelPrefix: "drydock.test"}).Address(ctx, wsID)
		if c.want != nil {
			if !errors.Is(err, c.want) {
				t.Errorf("%s: %+v, %v; want %v", c.name, a, err, c.want)
			}
			continue
		}
		if err != nil || a.IP.String() != c.ip {
			t.Errorf("%s: %+v, %v; want %s", c.name, a, err, c.ip)
		}
	}

	// A listing line that is not an id, another workspace's label, an
	// answer about another container, and Docker failing are errors, not
	// addresses — and not "not running" either.
	for name, body := range map[string]string{
		"not an id":            addressDocker("--all", ""),
		"another workspace":    addressDocker(idA, inspectJSON(idA, "01JZZZZZZZZZZZZZZZZZZZZZZZ", true, bridge("172.18.0.2"))),
		"another container":    addressDocker(idA, inspectJSON(idB, wsID, true, bridge("172.18.0.2"))),
		"docker fails":         "echo 'Cannot connect to the Docker daemon' >&2; exit 1",
		"inspect is not JSON":  addressDocker(idA, "nope"),
		"inspect is two items": addressDocker(idA, "[{},{}]"),
		"network inspect fails": `case "$1" in ps) echo ` + idA + ` ;; inspect) printf '%s' '` + inspectJSON(idA, wsID, true, bridge("172.18.0.2")) +
			`' ;; network) echo 'Error: No such network' >&2; exit 1 ;; esac`,
	} {
		run, _ := fakes(t, map[string]string{"docker": body})
		a, err := withHost(Manager{Run: run, LabelPrefix: "drydock.test"}).Address(ctx, wsID)
		if err == nil || errors.Is(err, ErrNotRunning) {
			t.Errorf("%s: %+v, %v; want an error that is not 'not running'", name, a, err)
		}
	}
	// A workspace id that is not a ULID never becomes a label filter.
	run, dir := fakes(t, map[string]string{"docker": "exit 0"})
	if _, err := (Manager{Run: run, LabelPrefix: "drydock.test"}).Address(ctx, wsID+",label=x"); err == nil {
		t.Error("a malformed workspace id was resolved")
	}
	if err := (Manager{Run: run, LabelPrefix: "drydock.test"}).Confirm(ctx, wsID, Address{ContainerID: "--all"}); err == nil {
		t.Error("Confirm took something that is not an id")
	}
	if _, err := os.Stat(filepath.Join(dir, "docker.argv")); err == nil {
		t.Errorf("docker ran: %q", argv(t, dir, "docker"))
	}
}

// TestAddressRefusesThisHostsOwnAddresses: with no LocalAddrs set — as in
// production — the kernel is asked, and an inspect that names one of this
// machine's own addresses is refused. The control is the same inspect naming
// an address this machine does not hold.
func TestAddressRefusesThisHostsOwnAddresses(t *testing.T) {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		t.Fatal(err)
	}
	own := ""
	for _, a := range addrs {
		if p, err := netip.ParsePrefix(a.String()); err == nil && p.Addr().Is4() && !p.Addr().IsLoopback() && UsableIP(p.Addr()) {
			own = p.Addr().String()
			break
		}
	}
	if own == "" {
		t.Fatal("this machine has no non-loopback IPv4 address to test with")
	}
	for ip, want := range map[string]error{own: ErrNoAddress, "198.51.100.77": nil} {
		run, _ := fakes(t, map[string]string{"docker": addressDocker(idA, inspectJSON(idA, wsID, true, bridge(ip)))})
		a, err := (Manager{Run: run, LabelPrefix: "drydock.test"}).Address(context.Background(), wsID)
		if want == nil && (err != nil || a.IP.String() != ip) {
			t.Errorf("control: %s = %+v, %v", ip, a, err)
		}
		if want != nil && !errors.Is(err, want) {
			t.Errorf("this machine's own %s = %+v, %v; want refused", ip, a, err)
		}
	}
}

// TestConfirm: the same container, still running, at the same address — or
// ErrMoved. A container removed since ("No such container") has moved too.
func TestConfirm(t *testing.T) {
	ctx := context.Background()
	at := Address{ContainerID: idA, IP: netip.MustParseAddr("172.18.0.2")}
	for _, c := range []struct {
		name, body string
		want       error
		argv       string
	}{
		{"control: unchanged", addressDocker("", inspectJSON(idA, wsID, true, bridge("172.18.0.2"))), nil,
			"inspect --type container -- " + idA + " -- network inspect -- " + netBridge},
		{"another address", addressDocker("", inspectJSON(idA, wsID, true, bridge("172.18.0.3"))), ErrMoved,
			"inspect --type container -- " + idA + " -- network inspect -- " + netBridge},
		{"stopped", addressDocker("", inspectJSON(idA, wsID, false, bridge("172.18.0.2"))), ErrMoved, "inspect --type container -- " + idA},
		{"removed", `echo "Error response from daemon: No such container: ` + idA + `" >&2; exit 1`, ErrMoved, "inspect --type container -- " + idA},
	} {
		run, dir := fakes(t, map[string]string{"docker": c.body})
		err := withHost(Manager{Run: run, LabelPrefix: "drydock.test"}).Confirm(ctx, wsID, at)
		if !errors.Is(err, c.want) && !(c.want == nil && err == nil) {
			t.Errorf("%s: %v; want %v", c.name, err, c.want)
		}
		if got := strings.TrimSpace(strings.Join(argv(t, dir, "docker"), " ")); got != c.argv {
			t.Errorf("%s: argv %q", c.name, got)
		}
	}
}
