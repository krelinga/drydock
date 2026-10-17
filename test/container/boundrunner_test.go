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

// An up or read-configuration that outlives the limit is dumped (with what it
// printed) and then stopped; one that ends in time prints no dump, and exec
// is not bounded here. The last two are the controls.
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
	for _, want := range []string{"BOUND:", "`devcontainer up` still running after 1s", "fetching-feature", "/x"} {
		if !strings.Contains(dump.String(), want) {
			t.Errorf("dump lacks %q:\n%s", want, dump.String())
		}
	}

	// read-configuration is bounded the same way.
	dump.Reset()
	b = boundedRunner{Inner: fakeUp(t, "echo reading >&2; exec sleep 30"), Limit: time.Second, Out: &dump}
	start = time.Now()
	res = b.Run(context.Background(), subproc.Cmd{Name: "devcontainer", Args: []string{"read-configuration", "--workspace-folder", "/y"}})
	if time.Since(start) > 20*time.Second || (res.ExitCode == 0 && res.Err == nil) {
		t.Errorf("the hung read-configuration was not stopped: %+v", res)
	}
	for _, want := range []string{"`devcontainer read-configuration` still running after 1s", "reading", "/y"} {
		if !strings.Contains(dump.String(), want) {
			t.Errorf("read-configuration dump lacks %q:\n%s", want, dump.String())
		}
	}
	// And a subcommand with no bound of its own runs as the inner runner ran it.
	dump.Reset()
	b = boundedRunner{Inner: fakeUp(t, "sleep 2"), Limit: time.Second, Out: &dump}
	if res = b.Run(context.Background(), subproc.Cmd{Name: "devcontainer", Args: []string{"exec"}}); res.ExitCode != 0 || res.Err != nil || dump.Len() != 0 {
		t.Errorf("exec was bounded: %+v, dump %q", res, dump.String())
	}

	dump.Reset()
	b = boundedRunner{Inner: fakeUp(t, "echo ok"), Limit: time.Second, Out: &dump}
	res = b.Run(context.Background(), subproc.Cmd{Name: "devcontainer", Args: []string{"up"}})
	time.Sleep(1500 * time.Millisecond)
	if res.ExitCode != 0 || res.Err != nil || dump.Len() != 0 {
		t.Errorf("a quick up: %+v, dump %q", res, dump.String())
	}
}
