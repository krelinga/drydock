package dockerguard

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// probeDocker is a fake docker for DaemonLogConfig, run in this process:
// each call is a line in calls, and the "daemon" holds at most the one probe,
// in the stray file, which ps lists. slow-create makes create leave its probe
// on the daemon a second later and hang meanwhile, as a create the daemon
// finishes after its client is killed; slow-inspect makes inspect hang;
// fail-rm makes rm fail.
const probeDocker = `#!/bin/sh
d=$(dirname "$0")
printf '%s\n' "$*" >> "$d/calls"
p=eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee
case "$1" in
ps) if [ -f "$d/stray" ]; then cat "$d/stray"; fi ;;
create)
  if [ -f "$d/slow-create" ]; then (sleep 1; echo "$p" > "$d/stray") >/dev/null 2>&1 & exec sleep 10; fi
  echo "$p" > "$d/stray"; echo "$p" ;;
inspect)
  if [ -f "$d/slow-inspect" ]; then exec sleep 10; fi
  printf '[{"Id":"%s","HostConfig":{"LogConfig":{"Type":"journald","Config":{"tag":"x"}}}}]\n' "$p" ;;
rm)
  if [ -f "$d/fail-rm" ]; then echo "the daemon said no" >&2; exit 1; fi
  : > "$d/stray" ;;
esac
`

// The probe is removed even when the probe's own time is what ran out — an
// inspect that hangs past ProbeTimeout — because its removal runs under a
// context of its own (internal/ephemeral's End, under sys.Cleanup); a create
// cut off, which the daemon may still finish, is waited for, its label
// listed again until the probe lands, and removed; and a removal that fails
// is said on the warning writer, while the default read stands. The control
// is the probe that does not time out: read, then listed and removed once.
func TestTheProbeIsRemovedWhateverRanOut(t *testing.T) {
	defer func(a, b, c time.Duration) { ProbeTimeout, RemoveTimeout, ProbeSettle = a, b, c }(ProbeTimeout, RemoveTimeout, ProbeSettle)
	ProbeTimeout, RemoveTimeout, ProbeSettle = 500*time.Millisecond, 5*time.Second, 2*time.Second
	dir := t.TempDir()
	fake := filepath.Join(dir, "docker")
	if err := os.WriteFile(fake, []byte(probeDocker), 0o755); err != nil {
		t.Fatal(err)
	}
	p := FixturePolicy(t.TempDir(), nil)
	p.ProbeImage = "busybox:1.37.0@sha256:" + strings.Repeat("b", 64)
	probe := strings.Repeat("e", 64)
	label := "label=drydock." + LabelLogProbe + "=" + FixtureLabels["drydock.workspace"]
	ps := "ps --all --quiet --no-trunc --filter " + label
	rm := "rm --force --volumes -- " + probe
	calls := func() []string {
		b, _ := os.ReadFile(filepath.Join(dir, "calls"))
		os.Remove(filepath.Join(dir, "calls"))
		return strings.Split(strings.TrimSpace(string(b)), "\n")
	}
	run := func(flag string) (*LogConfig, error, []string, string) {
		os.Remove(filepath.Join(dir, "slow-create"))
		os.Remove(filepath.Join(dir, "slow-inspect"))
		os.Remove(filepath.Join(dir, "fail-rm"))
		os.Remove(filepath.Join(dir, "stray"))
		if flag != "" {
			os.WriteFile(filepath.Join(dir, flag), nil, 0o600)
		}
		var warn bytes.Buffer
		start := time.Now()
		l, err := DaemonLogConfig(fake, p, &warn)
		if d := time.Since(start); d > 4*time.Second {
			t.Errorf("%s: the probe took %v", flag, d)
		}
		return l, err, calls(), warn.String()
	}

	// Control: read, then removed once.
	l, err, got, warn := run("")
	if err != nil || l.Type != "journald" || len(got) != 5 || got[0] != ps || got[3] != ps || got[4] != rm || warn != "" {
		t.Errorf("control: %+v %v, docker ran %q, warned %q", l, err, got, warn)
	}

	// The inspect hangs past ProbeTimeout: still removed.
	_, err, got, warn = run("slow-inspect")
	if err == nil || got[len(got)-1] != rm || warn != "" {
		t.Errorf("inspect cut off: %v, docker ran %q, warned %q", err, got, warn)
	}

	// The create is cut off and the daemon finishes it: listed again and
	// removed.
	_, err, got, _ = run("slow-create")
	if err == nil || len(got) < 4 || got[2] != ps || got[len(got)-1] != rm {
		t.Errorf("create cut off: %v, docker ran %q", err, got)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "stray")); len(bytes.TrimSpace(b)) != 0 {
		t.Errorf("create cut off: the probe %q is still there", b)
	}

	// A removal that fails is said, and the read stands.
	l, err, got, warn = run("fail-rm")
	if err != nil || l.Type != "journald" || got[len(got)-1] != rm ||
		!strings.Contains(warn, "the log probe "+probe+" could not be removed") || !strings.Contains(warn, "the daemon said no") {
		t.Errorf("rm fails: %+v %v, docker ran %q, warned %q", l, err, got, warn)
	}
}
