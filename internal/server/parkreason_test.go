package server

import (
	"testing"

	"github.com/krelinga/drydock/internal/provision"
	"github.com/krelinga/drydock/internal/supervisor"
)

// TestParkReasonsAreTheSupervisorsReasons: the provisioner names why boot
// parked a session server with its own codes, which the server passes to the
// supervisor as reasons as they are; the card keys its sentence and its
// action on them (web's lib/workspaceCard.ts). So each must be exactly the
// supervisor's.
func TestParkReasonsAreTheSupervisorsReasons(t *testing.T) {
	for park, reason := range map[string]supervisor.Reason{
		provision.ParkStaleBrokerMount: supervisor.ReasonStaleBrokerMount,
		provision.ParkContainerPaused:  supervisor.ReasonContainerPaused,
	} {
		if park != string(reason) {
			t.Errorf("provision parks for %q, the supervisor's reason is %q", park, reason)
		}
	}
}
