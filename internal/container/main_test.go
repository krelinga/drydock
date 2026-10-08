package container

import (
	"os"
	"testing"

	"github.com/krelinga/drydock/internal/dockerguard"
)

// TestMain lets this test binary be the docker guard, as the drydock binary
// is when run as "docker" (cmd/drydock): a fake CLI that runs docker through
// the --docker-path it is given reaches the real guard.
func TestMain(m *testing.M) {
	if dockerguard.IsGuard(os.Args[0]) {
		os.Exit(dockerguard.Main(os.Args[0], os.Args[1:], os.Stderr))
	}
	os.Exit(m.Run())
}
