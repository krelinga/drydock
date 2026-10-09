//go:build linux

package login

import (
	"testing"
	"time"
)

// TestRemoveSettleFitsTheRemoval: the longest settle DockerLauncher.Remove
// allows leaves the Manager's bound on a removal time for what follows the
// settle — one more docker ps and a docker rm — so a configured settle can
// never make every killed-CLI removal give up before its rm.
func TestRemoveSettleFitsTheRemoval(t *testing.T) {
	const afterSettle = 5 * time.Second // the last listing and the rm
	if MaxRemoveSettle+afterSettle > removeTimeout {
		t.Errorf("MaxRemoveSettle %v + %v for the last listing and rm exceeds removeTimeout %v",
			MaxRemoveSettle, afterSettle, removeTimeout)
	}
	if DefaultRemoveSettle > MaxRemoveSettle {
		t.Errorf("DefaultRemoveSettle %v is over MaxRemoveSettle %v", DefaultRemoveSettle, MaxRemoveSettle)
	}
}
