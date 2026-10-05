package container

import (
	"context"
	"strings"
	"testing"
)

// TestFindStopRemoveBuildExactArgv: Find filters by this prefix's workspace
// label with this workspace's id, on the daemon's side; stop and rm get the
// ids after "--", rm with --force and --volumes (anonymous volumes only:
// named ones, the credential volume among them, are never removed).
func TestFindStopRemoveBuildExactArgv(t *testing.T) {
	a, b := strings.Repeat("a", 64), strings.Repeat("b", 64)
	run, dir := fakes(t, map[string]string{"docker": `[ "$1" = ps ] && printf '%s\n%s\n' ` + a + ` ` + b + "\nexit 0\n"})
	m := Manager{Run: run, LabelPrefix: "drydock.test"}
	ctx := context.Background()
	ids, err := m.Find(ctx, wsID)
	if err != nil || len(ids) != 2 || ids[0] != a || ids[1] != b {
		t.Fatalf("Find = %v, %v", ids, err)
	}
	if err := m.Stop(ctx, ids); err != nil {
		t.Fatal(err)
	}
	if err := m.Remove(ctx, ids); err != nil {
		t.Fatal(err)
	}
	if err := m.Remove(ctx, nil); err != nil {
		t.Errorf("removing nothing = %v", err)
	}
	got := strings.TrimSpace(strings.Join(argv(t, dir, "docker"), " "))
	want := "ps --all --quiet --no-trunc --filter label=drydock.test.workspace=" + wsID +
		" -- stop -- " + a + " " + b + " -- rm --force --volumes -- " + a + " " + b
	if got != want {
		t.Errorf("argv\n got %s\nwant %s", got, want)
	}
}

// TestLifecycleRefusesWhatIsNotAnID: a workspace id that is not a ULID never
// becomes a label filter (an empty one would match every workspace), and
// nothing that is not a full container id reaches stop or rm — not from a
// caller, and not from docker's own listing.
func TestLifecycleRefusesWhatIsNotAnID(t *testing.T) {
	ctx := context.Background()
	run, dir := fakes(t, map[string]string{"docker": "exit 0\n"})
	m := Manager{Run: run, LabelPrefix: "drydock.test"}
	for _, ws := range []string{"", "x", wsID + ",label=other"} {
		if _, err := m.Find(ctx, ws); err == nil {
			t.Errorf("Find(%q) ran", ws)
		}
	}
	for _, id := range []string{"--all", "-f", "abc", strings.Repeat("A", 64), strings.Repeat("a", 63)} {
		if err := m.Remove(ctx, []string{id}); err == nil {
			t.Errorf("Remove(%q) ran", id)
		}
		if err := m.Stop(ctx, []string{id}); err == nil {
			t.Errorf("Stop(%q) ran", id)
		}
	}
	// The control: a real id runs.
	if err := m.Stop(ctx, []string{strings.Repeat("c", 64)}); err != nil {
		t.Errorf("control: %v", err)
	}
	if got := strings.TrimSpace(strings.Join(argv(t, dir, "docker"), " ")); got != "stop -- "+strings.Repeat("c", 64) {
		t.Errorf("only the control ran: %v", got)
	}

	bad, _ := fakes(t, map[string]string{"docker": "echo '--all'\n"})
	if _, err := (Manager{Run: bad, LabelPrefix: "drydock.test"}).Find(ctx, wsID); err == nil {
		t.Error("Find passed on a listing line that is not an id")
	}
	failing, _ := fakes(t, map[string]string{"docker": "echo 'daemon down' >&2; exit 1\n"})
	if err := (Manager{Run: failing, LabelPrefix: "drydock.test"}).Remove(ctx, []string{strings.Repeat("c", 64)}); err == nil {
		t.Error("a failed rm was not an error")
	}
}
