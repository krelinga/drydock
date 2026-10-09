package container_test

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"os/exec"
	"testing"

	"github.com/krelinga/drydock/internal/container"
)

// TestAddressDialsOnlyABridge is PF §10.6 against real Docker: the preview
// proxy dials a container's address only on a bridge network. A container on
// an ipvlan network — where an approved --ip can name the host's own address
// or another machine on its LAN (found in review, reproduced with an ipvlan on
// eth0 and the host's address) — is refused, whatever its address; the
// control, the same image on the default bridge, resolves to an address that
// is the container's. The ipvlan here has no parent, so Docker gives it a
// dummy link and the test touches no real network.
func TestAddressDialsOnlyABridge(t *testing.T) {
	needDocker(t)
	p := prefix(t)
	b := make([]byte, 1)
	rand.Read(b)
	net := p + ".ipvlan"
	docker(t, "network", "create", "--driver", "ipvlan", "--subnet", fmt.Sprintf("10.253.%d.0/24", b[0]), "--", net)
	lanWS, bridgeWS := "01JTESTADDRESSXPVAN0000000", "01JTESTADDRESSBR1DGE000000"
	lan := docker(t, "run", "-d", "--network", net, "--label", p+".workspace="+lanWS, image, "sleep", "600")
	t.Cleanup(func() {
		exec.Command("docker", "rm", "-f", "--", lan).Run()
		exec.Command("docker", "network", "rm", "--", net).Run()
	})
	docker(t, "run", "-d", "--label", p+".workspace="+bridgeWS, image, "sleep", "600")

	m := manager(p)
	ctx := context.Background()
	if a, err := m.Address(ctx, lanWS); !errors.Is(err, container.ErrNoAddress) {
		t.Errorf("a container on an ipvlan network = %+v, %v; want refused", a, err)
	}
	a, err := m.Address(ctx, bridgeWS)
	if err != nil || !a.IP.IsValid() {
		t.Fatalf("control: a container on the default bridge = %+v, %v", a, err)
	}
	if want := docker(t, "inspect", "--format", "{{.NetworkSettings.Networks.bridge.IPAddress}}", a.ContainerID); a.IP.String() != want {
		t.Errorf("control: resolved %s; docker says %s", a.IP, want)
	}
	if err := m.Confirm(ctx, bridgeWS, a); err != nil {
		t.Errorf("control: Confirm = %v", err)
	}
}
