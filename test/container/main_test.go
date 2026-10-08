package container_test

import (
	"os"
	"testing"

	"github.com/krelinga/drydock/internal/dockerguard"
)

// TestMain lets this test binary be the docker guard. The server a test
// builds names os.Executable() as its guard, as the real server names the
// drydock binary, and the devcontainer CLI runs it as "docker" from each
// workspace's guard directory — so the real CLI's docker commands reach the
// real guard here (design §6, "The docker guard").
func TestMain(m *testing.M) {
	if dockerguard.IsGuard(os.Args[0]) {
		os.Exit(dockerguard.Main(os.Args[0], os.Args[1:], os.Stderr))
	}
	os.Exit(m.Run())
}
