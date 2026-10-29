package container

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestBuiltImagesAreTheCLIsNames: the names are the ones the CLI gave the
// images it built for a workspace folder: vsc-<basename>-<sha256 of the
// folder's path>-features, lowercased and with runs of separators collapsed,
// from any working directory. The pairs are a recording
// (test/fixtures/devcontainer/image-names.txt, re-measured by the script
// beside it) of folders named as Drydock names them — a plain repository
// name, one with capitals, and one whose separators the CLI collapses — so a
// change to the computation, or a CLI that moved its naming, is caught; and
// anything but a clean absolute folder with a repository's name is refused
// rather than guessed at.
func TestBuiltImagesAreTheCLIsNames(t *testing.T) {
	names, err := BuiltImages("/srv/drydock/ws/" + wsID + "/alpha")
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
	var cli string
	rec := map[string]string{}
	for _, l := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		f := strings.Fields(l)
		switch {
		case len(f) == 2 && f[0] == "cli":
			cli = f[1]
		case len(f) == 3 && f[0] == "image":
			rec[f[1]] = f[2]
		}
	}
	if len(rec) != 3 {
		t.Fatalf("the recording is not one: %q", b)
	}
	cased := false
	for folder, image := range rec {
		cased = cased || filepath.Base(folder) != strings.ToLower(filepath.Base(folder))
		if m, err := BuiltImages(folder); err != nil || m[1] != image {
			t.Errorf("BuiltImages(%q) = %q, %v; CLI %s built %s", folder, m, err, cli, image)
		}
	}
	if !cased {
		t.Error("the recording measures no folder with capitals")
	}
	if len(names) != 4 || !strings.HasPrefix(names[0], "vsc-alpha-") || len(names[0]) != len("vsc-alpha-")+64 ||
		names[1] != names[0]+"-features" || names[2] != names[0]+"-uid" || names[3] != names[0]+"-features-uid" {
		t.Errorf("names %q", names)
	}
	// The case is the image name's alone: the hash is of the path as it is.
	upper, err := BuiltImages("/srv/drydock/ws/" + wsID + "/Alpha")
	if err != nil || !strings.HasPrefix(upper[0], "vsc-alpha-") || upper[0] == names[0] {
		t.Errorf("BuiltImages(…/Alpha) = %q, %v; want vsc-alpha- and another hash than %s", upper, err, names[0])
	}
	for _, bad := range []string{"", "alpha", "srv/ws/x/alpha", "/srv/ws/x/alpha/", "/srv/ws/x/../alpha", "/srv/ws/x/my repo", "/srv/ws/x/a+b", "/srv/ws/x/ü"} {
		if _, err := BuiltImages(bad); err == nil {
			t.Errorf("BuiltImages(%q) computed names", bad)
		}
	}
}

// TestCLIImageNameIsTheCLIs: cliImageName is the CLI's HN, for the separator
// runs a repository name can hold. The wants are HN's own output, run under
// node from CLI 0.89.0's dist/spec-node/devContainersSpecCLI.js on
// "vsc-<name>-abc123"; the first rows change nothing, which is the positive
// control for the rows that collapse.
func TestCLIImageNameIsTheCLIs(t *testing.T) {
	for name, want := range map[string]string{
		"alpha": "vsc-alpha-abc123", "x__y": "vsc-x__y-abc123", "a--b": "vsc-a--b-abc123",
		"My.Repo_x": "vsc-my.repo_x-abc123", "Drydock-Testbed-A": "vsc-drydock-testbed-a-abc123",
		"a..b": "vsc-a.b-abc123", "a._-b": "vsc-a.b-abc123", "x___y": "vsc-x__y-abc123",
		"-_a": "vsc--a-abc123", "___": "vsc-abc123", ".github": "vsc-github-abc123",
		"a-": "vsc-a--abc123", "a.": "vsc-a.abc123", "a_": "vsc-a_abc123", "__a": "vsc-a-abc123",
		"-.x": "vsc--x-abc123", "A.-_B": "vsc-a.b-abc123", "a_.b": "vsc-a_b-abc123",
		"a__.b": "vsc-a__b-abc123", "a-_b": "vsc-a-b-abc123",
	} {
		if got := cliImageName("vsc-" + name + "-abc123"); got != want {
			t.Errorf("cliImageName(vsc-%s-abc123) = %q, want %q", name, got, want)
		}
	}
}

// TestRemoveBuiltImagesExactArgv: the listing filters by each exact name, and
// only names it returns at :latest that are this folder's are removed, after
// "--", never with --force. Nothing listed is nothing removed; a failed rm
// is an error.
func TestRemoveBuiltImagesExactArgv(t *testing.T) {
	ctx := context.Background()
	folder := "/srv/drydock/ws/" + wsID + "/alpha"
	names, _ := BuiltImages(folder)
	// Beside the two to remove: another tag, a look-alike name (a prefix match
	// would take it) and another workspace's.
	listed := names[1] + ":latest\n" + names[3] + ":latest\n" + names[1] + ":other\n" + names[1] + "-x:latest\n" + "vsc-alpha-other-features:latest\n"
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
