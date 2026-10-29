package dockerguard

import (
	"reflect"
	"slices"
	"testing"
)

// An approved runArgs is never removed from the guard's own additions at the
// front of argv, even where it starts an option there. One that runs from
// the guard's last label on into what the CLI wrote next — here a moved
// Feature's published port, approved as no appPort — would, removed there,
// take the CLI's option with it unread; the repository's own copy is not in
// this argv (its configuration no longer writes it) to be read in its place.
// The control is the same approval with the repository's copy where the CLI
// writes runArgs and no port before it, which passes.
func TestApprovedRunArgsNeverStartInTheGuardsOwn(t *testing.T) {
	root := t.TempDir()
	p := labelPolicy(root)
	p.Name = fixtureName
	run := command(t, Recorded(t, "image", root), "run")
	lf := "devcontainer.local_folder=" + p.Labels["devcontainer.local_folder"]
	approve(p, "-l", lf, "-p", "0.0.0.0:22:22")
	base := WithAdded(p, run)
	ownEnd := len(added(p)) + 1
	if base[ownEnd-1] != lf {
		t.Fatalf("the guard's additions do not end in the folder label: %q", base[:ownEnd])
	}
	if d := Check(p, insert(base, ownEnd, "-p", "0.0.0.0:22:22")); !d.Refused || !reflect.DeepEqual(d.Settings, []string{SettingAppPort}) {
		t.Errorf("a port right after the guard's labels: %+v, want refused naming %s", d, SettingAppPort)
	}
	at := slices.Index(base, "drydock.branch=main") + 1
	if d := Check(p, insert(base, at, "-l", lf, "-p", "0.0.0.0:22:22")); d.Refused {
		t.Errorf("control, the repository's copy where the CLI writes runArgs: %+v", d)
	}
}
