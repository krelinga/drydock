package workspace

import (
	"context"
	"errors"
	"math/rand/v2"
	"regexp"
	"strings"
	"testing"
)

const testID = "01JABCDEFGHJKMNPQRSTVWXYZ0"

// CloneFolder is the repository's name without its owner, in its own case,
// less any leading characters that are not a letter or digit, and "repo"
// when nothing is left. A name with no owner or outside GitHub's charset is
// refused rather than cleaned.
func TestCloneFolder(t *testing.T) {
	for full, want := range map[string]string{
		"krelinga/alpha": "alpha", "krelinga/Drydock.Testbed_A": "Drydock.Testbed_A",
		"o/a-_b..c": "a-_b..c", "o/x.": "x.", "o/9-": "9-",
		"o/.github": "github", "o/-x": "x", "o/_.-Y": "Y",
		"o/...": "repo", "o/.": "repo", "o/..": "repo", "o/-_": "repo",
		"o/.drydock": "drydock", "o/.DryDock": "DryDock",
	} {
		if got, err := CloneFolder(full); err != nil || got != want {
			t.Errorf("CloneFolder(%q) = %q, %v; want %q", full, got, err, want)
		}
	}
	for _, bad := range []string{
		"", "alpha", "/alpha", "o/", "o/a/b", "o/../x", "o/a b", "o/a+b", "o/ü", "o/a\x00",
	} {
		if got, err := CloneFolder(bad); err == nil {
			t.Errorf("CloneFolder(%q) = %q; want a refusal", bad, got)
		}
	}
}

// The CLI's Compose project name for a configuration with no name: of its
// own, read out of CLI 0.89.0 (dist/spec-node/devContainersSpecCLI.js, cg):
// "<basename of the workspace folder>_devcontainer", lowercased, with
// everything outside [-_a-z0-9] dropped — or outside [a-z0-9], for a Compose
// before 1.21. Compose refuses a project name that does not start with a
// letter or digit (measured on Compose v5.6.0: "-x_devcontainer" and
// "_devcontainer" are "invalid project name"; a folder "-x" fails up with it).
var (
	cgNew          = regexp.MustCompile(`[^-_a-z0-9]`)
	cgOld          = regexp.MustCompile(`[^a-z0-9]`)
	composeProject = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]*$`)
)

func cliComposeProject(folder string, newCompose bool) string {
	name := strings.ToLower(folder + "_devcontainer")
	if newCompose {
		return cgNew.ReplaceAllString(name, "")
	}
	return cgOld.ReplaceAllString(name, "")
}

// Every folder CloneFolder makes — for every repository name GitHub's charset
// allows, here a quarter-million random ones weighted to separators — gives a
// project name Compose accepts, under both of the CLI's derivations. The
// control is the raw repository name, which for the same inputs does not.
func TestEveryCloneFolderIsAComposeProject(t *testing.T) {
	const chars = "aZ09._-._-._-"
	rawFails := 0
	r := rand.New(rand.NewPCG(1, 2))
	for i := 0; i < 250000; i++ {
		b := make([]byte, 1+r.IntN(8))
		for j := range b {
			b[j] = chars[r.IntN(len(chars))]
		}
		name := string(b)
		folder, err := CloneFolder("o/" + name)
		if err != nil {
			t.Fatalf("CloneFolder(o/%s): %v", name, err)
		}
		for _, newCompose := range []bool{true, false} {
			if p := cliComposeProject(folder, newCompose); !composeProject.MatchString(p) {
				t.Fatalf("repository %q: folder %q gives the project %q, which Compose refuses", name, folder, p)
			}
		}
		if err := CheckClonePath("/r", testID, "/r/"+testID+"/"+folder); err != nil {
			t.Fatalf("CheckClonePath refuses CloneFolder's own %q: %v", folder, err)
		}
		if !composeProject.MatchString(cliComposeProject(name, true)) {
			rawFails++
		}
	}
	if rawFails == 0 {
		t.Error("control: no raw repository name gave a project Compose refuses, so the property above proves nothing")
	}
}

// ClonePath and CheckClonePath agree: every path ClonePath makes passes the
// check, and the check refuses a path anywhere but directly under
// <root>/<id>/, or with a folder ClonePath would not make.
func TestClonePathAndItsCheck(t *testing.T) {
	root := "/srv/drydock/ws"
	p, err := ClonePath(root, testID, "krelinga/Alpha")
	if err != nil || p != root+"/"+testID+"/Alpha" {
		t.Fatalf("ClonePath = %q, %v", p, err)
	}
	if err := CheckClonePath(root, testID, p); err != nil {
		t.Errorf("CheckClonePath of ClonePath's own %q: %v", p, err)
	}
	for _, id := range []string{"", ".", "..", "a/b"} {
		if got, err := ClonePath(root, id, "krelinga/alpha"); err == nil {
			t.Errorf("ClonePath with id %q = %q; want a refusal", id, got)
		}
	}
	for _, bad := range []string{
		"", root + "/" + testID, root + "/" + testID + "/", root + "/" + testID + "/.",
		root + "/" + testID + "/..", root + "/" + testID + "/.drydock", root + "/" + testID + "/a/b",
		root + "/" + testID + "/x/../alpha", root + "/OTHER/alpha", "/elsewhere/" + testID + "/alpha",
		root + "/" + testID + "/a b", root + "/" + testID + "/-x", root + "/" + testID + "/.github", "alpha",
	} {
		if err := CheckClonePath(root, testID, bad); err == nil {
			t.Errorf("CheckClonePath(%q) passed", bad)
		}
	}
}

// OrphanClonePath is the local_folder label when there is one — a valid one,
// never replaced by a derivation when it is not — and ClonePath of the
// labelled repository when there is none.
func TestOrphanClonePath(t *testing.T) {
	root := "/srv/drydock/ws"
	labelled := root + "/" + testID + "/old-name"
	if p, err := OrphanClonePath(root, testID, labelled, "krelinga/new-name"); err != nil || p != labelled {
		t.Errorf("with local_folder: %q, %v; want %q", p, err, labelled)
	}
	if p, err := OrphanClonePath(root, testID, "", "krelinga/.Compose"); err != nil || p != root+"/"+testID+"/Compose" {
		t.Errorf("without local_folder: %q, %v", p, err)
	}
	for _, lf := range []string{"/elsewhere/" + testID + "/alpha", root + "/" + testID + "/.drydock"} {
		if p, err := OrphanClonePath(root, testID, lf, "krelinga/alpha"); err == nil {
			t.Errorf("local_folder %q gave %q", lf, p)
		}
	}
	if p, err := OrphanClonePath(root, testID, "", "krelinga/a b"); err == nil {
		t.Errorf("a labelled name outside GitHub's charset gave %q", p)
	}
}

// Create names the clone after the repository as recorded then, and keeps
// the path on the row: a rename afterwards (the catalog rewriting
// full_name) never moves it. A repository name with no letter or digit to
// start a folder gets "repo". One whose name cannot name a folder is refused
// before anything is written — the control is a create of a good one on the
// same store.
func TestCreateNamesTheCloneAfterTheRepository(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	w := f.create(t, 1)
	if want := "/srv/drydock/ws/" + w.ID + "/repo1"; w.HostPath != want {
		t.Fatalf("host_path %q; want %q", w.HostPath, want)
	}
	if _, err := f.store.DB.ExecContext(ctx, `UPDATE repository SET full_name = 'krelinga/renamed' WHERE id = 1`); err != nil {
		t.Fatal(err)
	}
	if got, err := f.store.Get(ctx, w.ID); err != nil || got.HostPath != w.HostPath {
		t.Errorf("after a rename the row says %q, %v; want %q", got.HostPath, err, w.HostPath)
	}

	for id, name := range map[int64]string{4: "krelinga/.github", 5: "krelinga/..."} {
		if _, err := f.store.DB.ExecContext(ctx, `UPDATE repository SET full_name = ? WHERE id = ?`, name, id); err != nil {
			t.Fatal(err)
		}
	}
	if w := f.create(t, 4); w.HostPath != "/srv/drydock/ws/"+w.ID+"/github" {
		t.Errorf(".github: host_path %q", w.HostPath)
	}
	if _, err := f.store.DB.ExecContext(ctx, `UPDATE workspace SET state = 'stopped'`); err != nil {
		t.Fatal(err) // out of the cap's way
	}
	if w := f.create(t, 5); w.HostPath != "/srv/drydock/ws/"+w.ID+"/repo" {
		t.Errorf("...: host_path %q", w.HostPath)
	}

	if _, err := f.store.DB.ExecContext(ctx, `UPDATE repository SET full_name = 'krelinga/a b' WHERE id = 2`); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.Create(ctx, 2, "main"); !errors.Is(err, ErrBadCloneFolder) {
		t.Errorf("a create for krelinga/a b: %v; want ErrBadCloneFolder", err)
	}
	var rows int
	if err := f.store.DB.QueryRowContext(ctx, `SELECT count(*) FROM workspace WHERE repository_id = 2`).Scan(&rows); err != nil || rows != 0 {
		t.Errorf("the refused create left %d rows (%v)", rows, err)
	}
	if w := f.create(t, 3); w.HostPath != "/srv/drydock/ws/"+w.ID+"/repo3" {
		t.Errorf("control: host_path %q", w.HostPath)
	}
}

// Adopt records the clone path it is given, held to CheckClonePath: a path
// that is not a clone under this root is refused and writes no row.
func TestAdoptHoldsTheClonePath(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	ids := []string{"01JAAAAAAAAAAAAAAAAAAAAAA1", "01JAAAAAAAAAAAAAAAAAAAAAA2"}
	labelled := "/srv/drydock/ws/" + ids[0] + "/old-name"
	if err := f.store.Adopt(ctx, Workspace{ID: ids[0], RepositoryID: 31, Branch: "main", State: Running,
		HostPath: labelled}, "krelinga/new-name"); err != nil {
		t.Fatal(err)
	}
	if w, err := f.store.Get(ctx, ids[0]); err != nil || w.HostPath != labelled {
		t.Errorf("adopted: %q, %v; want %q", w.HostPath, err, labelled)
	}
	for _, hp := range []string{"", "/elsewhere/" + ids[1] + "/alpha", "/srv/drydock/ws/" + ids[1] + "/.drydock"} {
		if err := f.store.Adopt(ctx, Workspace{ID: ids[1], RepositoryID: 33, Branch: "main", State: Running,
			HostPath: hp}, "krelinga/alpha"); err == nil {
			t.Errorf("adopted with host_path %q", hp)
		}
	}
	if _, err := f.store.Get(ctx, ids[1]); !errors.Is(err, ErrNotFound) {
		t.Errorf("a refused adopt wrote a row: %v", err)
	}
}
