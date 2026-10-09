package provision

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/krelinga/drydock/internal/dockerguard"
	"github.com/krelinga/drydock/internal/life"
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

// runIn runs p's jobs in a group of their own, as Serve runs them in
// work.Child("provision"), and stops it and waits for it as the test ends —
// before the database closes, whose cleanup newEnv registered first.
func runIn(t *testing.T, p *Provisioner) *life.Group {
	t.Helper()
	g := life.NewGroup(context.Background())
	if err := p.RunIn(g); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if late := g.Wait(time.After(time.Minute)); len(late) > 0 {
			t.Errorf("jobs still running a minute after the test: %v", late)
		}
	})
	return g
}

// shutdown is Serve's half of a shutdown, for the provisioner alone: stop
// its group, so no job starts and every one in flight is cancelled, and wait
// up to d for them to end.
func (e *env) shutdown(d time.Duration) []string { return e.g.Wait(time.After(d)) }

// idle waits until p has no job in flight. A job leaves active after its
// end is written, so once none is left every event a job writes is in.
func (p *Provisioner) idle() {
	for {
		p.mu.Lock()
		var dones []chan struct{}
		for _, j := range p.active {
			dones = append(dones, j.done)
		}
		p.mu.Unlock()
		if len(dones) == 0 {
			return
		}
		for _, d := range dones {
			<-d
		}
	}
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
