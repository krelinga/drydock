package container

import (
	"context"
	"strings"
	"testing"
)

const testCleanupImage = "busybox:1.37.0@sha256:bdf57e528e45e4433820e045b29b4597825a1c9e38353532d90a01445013f82e"

// TestCleanupArgsMountOnlyTheWorkspace: the helper runs as root, so its argv
// is its whole reach. It mounts the one directory it is given, which must be
// a workspace's own (its last element the id); a parent, a sibling, a path
// that would add --mount options, and an image not pinned by digest are each
// refused before docker runs. The control builds the exact argv.
func TestCleanupArgsMountOnlyTheWorkspace(t *testing.T) {
	m := Manager{LabelPrefix: "drydock.test", CleanupImage: testCleanupImage}
	dir := "/srv/drydock/ws/" + wsID
	got, err := m.CleanupArgs(wsID, dir)
	if err != nil {
		t.Fatal(err)
	}
	want := "run --rm --label drydock.test.cleanup=" + wsID + " --network none --read-only" +
		" --cap-drop ALL --cap-add DAC_OVERRIDE --cap-add FOWNER --security-opt no-new-privileges --user 0:0" +
		" --mount type=bind,source=" + dir + ",target=/w --entrypoint find " + testCleanupImage +
		" /w -mindepth 1 -delete"
	if strings.Join(got, " ") != want {
		t.Errorf("argv\n got %s\nwant %s", strings.Join(got, " "), want)
	}
	for _, a := range got {
		if strings.Contains(a, "drydock.test.workspace") {
			t.Errorf("the helper carries the workspace label reconciliation lists by: %q", a)
		}
	}

	for _, c := range []struct{ name, id, dir, image string }{
		{"the parent", wsID, "/srv/drydock/ws", testCleanupImage},
		{"the filesystem root", wsID, "/", testCleanupImage},
		{"another workspace", wsID, "/srv/drydock/ws/01JABCDEFGHJKMNPQRSTVWXYZ1", testCleanupImage},
		{"the clone, not the directory", wsID, dir + "/repo", testCleanupImage},
		{"an unclean path", wsID, "/srv/drydock/ws/../ws/" + wsID, testCleanupImage},
		{"a relative path", wsID, "ws/" + wsID, testCleanupImage},
		{"mount options smuggled in", wsID, "/srv/x,source=/," + wsID, testCleanupImage},
		{"not a workspace id", "..", "/srv/drydock/ws/..", testCleanupImage},
		{"an image by tag", wsID, dir, "busybox:1.37.0"},
		{"no image", wsID, dir, ""},
		{"an option for an image", wsID, dir, "--privileged@sha256:" + strings.Repeat("a", 64)},
	} {
		t.Run(c.name, func(t *testing.T) {
			m := Manager{LabelPrefix: "drydock.test", CleanupImage: c.image}
			if args, err := m.CleanupArgs(c.id, c.dir); err == nil {
				t.Errorf("built %v", args)
			}
		})
	}
}

// TestRemoveContentsClearsAStrayFirst: a helper an earlier attempt left is
// found by its label and removed before a new one runs, so a retried delete
// never has two. The control is the same call with no stray: no rm.
func TestRemoveContentsClearsAStrayFirst(t *testing.T) {
	ctx := context.Background()
	stray := strings.Repeat("c", 64)
	dir := "/srv/drydock/ws/" + wsID
	for _, withStray := range []bool{false, true} {
		body := "exit 0\n"
		if withStray {
			body = `[ "$1" = ps ] && echo ` + stray + "\nexit 0\n"
		}
		run, fdir := fakes(t, map[string]string{"docker": body})
		m := Manager{Run: run, LabelPrefix: "drydock.test", CleanupImage: testCleanupImage}
		if err := m.RemoveContents(ctx, wsID, dir); err != nil {
			t.Fatal(err)
		}
		got := strings.Join(argv(t, fdir, "docker"), " ")
		ps := "ps --all --quiet --no-trunc --filter label=drydock.test.cleanup=" + wsID + " --"
		rm := " rm --force --volumes -- " + stray + " --"
		if !strings.HasPrefix(got, ps) || strings.Contains(got, rm) != withStray ||
			!strings.Contains(got, " run --rm --label drydock.test.cleanup="+wsID) {
			t.Errorf("stray %v: argv %s", withStray, got)
		}
	}
	// docker failing the run is an error.
	run, _ := fakes(t, map[string]string{"docker": `[ "$1" = run ] && exit 1` + "\nexit 0\n"})
	m := Manager{Run: run, LabelPrefix: "drydock.test", CleanupImage: testCleanupImage}
	if err := m.RemoveContents(ctx, wsID, dir); err == nil {
		t.Error("a failed docker run was not an error")
	}
}
