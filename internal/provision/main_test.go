package provision

import (
	"os"
	"testing"

	"github.com/krelinga/drydock/internal/dockerguard"
)

// TestMain lets this test binary be the docker guard: the provisioner's
// guard is os.Executable(), as the server's is the drydock binary, and run
// as "docker" from a workspace's guard directory it checks the fake CLI's
// docker commands exactly as the real guard would (design §6, "The docker
// guard").
func TestMain(m *testing.M) {
	if dockerguard.IsGuard(os.Args[0]) {
		os.Exit(dockerguard.Main(os.Args[0], os.Args[1:], os.Stderr))
	}
	os.Exit(m.Run())
}

// testGuard is the provisioner's docker guard: this test binary.
func testGuard(t *testing.T) *dockerguard.Guard {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return &dockerguard.Guard{Binary: self}
}
