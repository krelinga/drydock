package workspace

import (
	"context"
	"errors"
	"testing"
)

const testID = "01JABCDEFGHJKMNPQRSTVWXYZ0"

// CloneFolder is the repository's name without its owner, in its own case,
// for every name GitHub's charset allows — and nothing else: a name that is
// not one path element inside <id>/, or is Drydock's own directory there, is
// refused rather than cleaned.
func TestCloneFolder(t *testing.T) {
	for full, want := range map[string]string{
		"krelinga/alpha": "alpha", "krelinga/Drydock.Testbed_A": "Drydock.Testbed_A",
		"o/.github": ".github", "o/a-_b..c": "a-_b..c", "o/...": "...", "o/-x": "-x",
		"o/.drydocks": ".drydocks",
	} {
		if got, err := CloneFolder(full); err != nil || got != want {
			t.Errorf("CloneFolder(%q) = %q, %v; want %q", full, got, err, want)
		}
	}
	for _, bad := range []string{
		"", "alpha", "/alpha", "o/", "o/.", "o/..", "o/a/b", "o/../x", "o/a b", "o/a+b", "o/ü", "o/a\x00",
		"o/.drydock", "o/.DryDock",
	} {
		if got, err := CloneFolder(bad); err == nil {
			t.Errorf("CloneFolder(%q) = %q; want a refusal", bad, got)
		}
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
		root + "/" + testID + "/a b", "alpha",
	} {
		if err := CheckClonePath(root, testID, bad); err == nil {
			t.Errorf("CheckClonePath(%q) passed", bad)
		}
	}
}

// Create names the clone after the repository as recorded then, and keeps
// the path on the row: a rename afterwards (the catalog rewriting
// full_name) never moves it. A repository whose name cannot name a folder is
// refused before anything is written — the control is a create of a good one
// on the same store.
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

	if _, err := f.store.DB.ExecContext(ctx, `UPDATE repository SET full_name = 'krelinga/.drydock' WHERE id = 2`); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.Create(ctx, 2, "main"); !errors.Is(err, ErrBadCloneFolder) {
		t.Errorf("a create for krelinga/.drydock: %v; want ErrBadCloneFolder", err)
	}
	var rows int
	if err := f.store.DB.QueryRowContext(ctx, `SELECT count(*) FROM workspace WHERE repository_id = 2`).Scan(&rows); err != nil || rows != 0 {
		t.Errorf("the refused create left %d rows (%v)", rows, err)
	}
	if w := f.create(t, 3); w.HostPath != "/srv/drydock/ws/"+w.ID+"/repo3" {
		t.Errorf("control: host_path %q", w.HostPath)
	}
}

// Adopt takes the clone's path from the container (its local_folder
// label), so a repository renamed since keeps its folder; derives it from
// the labelled name when the container has none; and refuses a label that
// is not a clone path under this root.
func TestAdoptTakesTheClonePathFromTheContainer(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	ids := []string{"01JAAAAAAAAAAAAAAAAAAAAAA1", "01JAAAAAAAAAAAAAAAAAAAAAA2", "01JAAAAAAAAAAAAAAAAAAAAAA3"}
	labelled := "/srv/drydock/ws/" + ids[0] + "/old-name"
	if err := f.store.Adopt(ctx, Workspace{ID: ids[0], RepositoryID: 31, Branch: "main", State: Running,
		HostPath: labelled}, "krelinga/new-name"); err != nil {
		t.Fatal(err)
	}
	if w, err := f.store.Get(ctx, ids[0]); err != nil || w.HostPath != labelled {
		t.Errorf("adopted with local_folder: %q, %v; want %q", w.HostPath, err, labelled)
	}
	if err := f.store.Adopt(ctx, Workspace{ID: ids[1], RepositoryID: 32, Branch: "main", State: Stopped},
		"krelinga/Compose"); err != nil {
		t.Fatal(err)
	}
	if w, err := f.store.Get(ctx, ids[1]); err != nil || w.HostPath != "/srv/drydock/ws/"+ids[1]+"/Compose" {
		t.Errorf("adopted without local_folder: %q, %v", w.HostPath, err)
	}
	for _, hp := range []string{"/elsewhere/" + ids[2] + "/alpha", "/srv/drydock/ws/" + ids[2] + "/.drydock"} {
		if err := f.store.Adopt(ctx, Workspace{ID: ids[2], RepositoryID: 33, Branch: "main", State: Running,
			HostPath: hp}, "krelinga/alpha"); err == nil {
			t.Errorf("adopted with local_folder %q", hp)
		}
	}
	if err := f.store.Adopt(ctx, Workspace{ID: ids[2], RepositoryID: 33, Branch: "main", State: Running},
		"krelinga/.."); err == nil {
		t.Error("adopted a container labelled with a name that cannot name a folder")
	}
	if _, err := f.store.Get(ctx, ids[2]); !errors.Is(err, ErrNotFound) {
		t.Errorf("a refused adopt wrote a row: %v", err)
	}
}
