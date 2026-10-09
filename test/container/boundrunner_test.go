package container_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/krelinga/drydock/internal/subproc"
)

// fakeUp is a devcontainer whose `up` runs body.
func fakeUp(t *testing.T, body string) subproc.Exec {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "devcontainer")
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return subproc.Exec{Resolver: subproc.FixedResolver{"devcontainer": p}, WaitDelay: time.Second}
}

// An up that outlives the limit is dumped (with what it printed) and then
// stopped; one that ends in time prints no dump. The second is the control.
func TestBoundDumpsAnUpThatOutlivesTheLimit(t *testing.T) {
	var dump strings.Builder
	b := boundedRunner{Inner: fakeUp(t, "echo fetching-feature; exec sleep 30"), Limit: time.Second, Out: &dump}
	start := time.Now()
	res := b.Run(context.Background(), subproc.Cmd{Name: "devcontainer", Args: []string{"up", "--workspace-folder", "/x"}})
	if time.Since(start) > 20*time.Second {
		t.Errorf("the hung up was not stopped")
	}
	if res.ExitCode == 0 && res.Err == nil {
		t.Errorf("a stopped up reported success")
	}
	for _, want := range []string{"BOUND:", "still running after 1s", "fetching-feature", "/x"} {
		if !strings.Contains(dump.String(), want) {
			t.Errorf("dump lacks %q:\n%s", want, dump.String())
		}
	}

	dump.Reset()
	b = boundedRunner{Inner: fakeUp(t, "echo ok"), Limit: time.Second, Out: &dump}
	res = b.Run(context.Background(), subproc.Cmd{Name: "devcontainer", Args: []string{"up"}})
	time.Sleep(1500 * time.Millisecond)
	if res.ExitCode != 0 || res.Err != nil || dump.Len() != 0 {
		t.Errorf("a quick up: %+v, dump %q", res, dump.String())
	}
}
