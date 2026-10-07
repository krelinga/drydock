package container

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestBuiltImagesAreTheCLIsNames: the names are the ones the CLI gave the
// images it built for a workspace folder: vsc-repo-<sha256 of the folder's
// path>-features, from any working directory. The pair is a recording
// (test/fixtures/devcontainer/image-names.txt, re-measured by the script
// beside it), so a change to the computation, or a CLI that moved its naming,
// is caught; and anything but a clean absolute folder named as Drydock names
// it is refused rather than guessed at.
func TestBuiltImagesAreTheCLIsNames(t *testing.T) {
	names, err := BuiltImages("/srv/drydock/ws/" + wsID + "/repo")
	if err != nil {
		t.Fatal(err)
	}
	// Measured: the CLI's `up --workspace-folder` of a folder, run from / so
	// the process's directory differs, and the image it tagged — recorded by
	// test/fixtures/devcontainer/measure-image-names.sh, which re-measures it.
	b, err := os.ReadFile(filepath.Join("..", "..", "test", "fixtures", "devcontainer", "image-names.txt"))
	if err != nil {
		t.Fatal(err)
	}
	rec := map[string]string{}
	for _, l := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		k, v, _ := strings.Cut(l, " ")
		rec[k] = v
	}
	if rec["folder"] == "" || !strings.HasPrefix(rec["image"], "vsc-") {
		t.Fatalf("the recording is not one: %q", b)
	}
	if m, err := BuiltImages(rec["folder"]); err != nil || m[1] != rec["image"] {
		t.Errorf("BuiltImages(%q) = %q, %v; CLI %s built %s", rec["folder"], m, err, rec["cli"], rec["image"])
	}
	if len(names) != 4 || !strings.HasPrefix(names[0], "vsc-repo-") || len(names[0]) != len("vsc-repo-")+64 ||
		names[1] != names[0]+"-features" || names[2] != names[0]+"-uid" || names[3] != names[0]+"-features-uid" {
		t.Errorf("names %q", names)
	}
	for _, bad := range []string{"", "repo", "srv/ws/x/repo", "/srv/ws/x/repo/", "/srv/ws/x/../repo", "/srv/ws/x/Repo", "/srv/ws/x/my repo"} {
		if _, err := BuiltImages(bad); err == nil {
			t.Errorf("BuiltImages(%q) computed names", bad)
		}
	}
}

// TestRemoveBuiltImagesExactArgv: the listing filters by each exact name, and
// only names it returns at :latest that are this folder's are removed, after
// "--", never with --force. Nothing listed is nothing removed; a failed rm
// is an error.
func TestRemoveBuiltImagesExactArgv(t *testing.T) {
	ctx := context.Background()
	folder := "/srv/drydock/ws/" + wsID + "/repo"
	names, _ := BuiltImages(folder)
	// Beside the two to remove: another tag, a look-alike name (a prefix match
	// would take it) and another workspace's.
	listed := names[1] + ":latest\n" + names[3] + ":latest\n" + names[1] + ":other\n" + names[1] + "-x:latest\n" + "vsc-repo-other-features:latest\n"
	run, dir := fakes(t, map[string]string{"docker": `[ "$2" = ls ] && printf '%s' '` + listed + `'
exit 0
`})
	removed, err := (Manager{Run: run}).RemoveBuiltImages(ctx, folder)
	if err != nil || strings.Join(removed, " ") != names[1]+":latest "+names[3]+":latest" {
		t.Fatalf("RemoveBuiltImages = %q, %v", removed, err)
	}
	got := strings.TrimSpace(strings.Join(argv(t, dir, "docker"), " "))
	want := "image ls --format {{.Repository}}:{{.Tag}}"
	for _, n := range names {
		want += " --filter reference=" + n
	}
	want += " -- image rm -- " + names[1] + ":latest " + names[3] + ":latest"
	if got != want {
		t.Errorf("argv\n got %s\nwant %s", got, want)
	}
	if strings.Contains(got, "force") || strings.Contains(got, "prune") {
		t.Errorf("argv forces or prunes: %s", got)
	}

	none, dir2 := fakes(t, map[string]string{"docker": "exit 0\n"})
	if removed, err := (Manager{Run: none}).RemoveBuiltImages(ctx, folder); err != nil || len(removed) != 0 {
		t.Errorf("nothing listed: %q, %v", removed, err)
	}
	if got := strings.Join(argv(t, dir2, "docker"), " "); strings.Contains(got, " rm ") {
		t.Errorf("removed something when nothing was listed: %s", got)
	}

	inUse, _ := fakes(t, map[string]string{"docker": `[ "$2" = ls ] && { echo '` + names[1] + `:latest'; exit 0; }
echo 'conflict: unable to delete (must be forced) - container 1f75 is using its referenced image' >&2; exit 1
`})
	if _, err := (Manager{Run: inUse}).RemoveBuiltImages(ctx, folder); err == nil {
		t.Error("an image docker refused to remove was not an error")
	}
}
