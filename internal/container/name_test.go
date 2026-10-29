package container

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/krelinga/drydock/internal/dockerguard"
)

// zeros is a Random that always draws 0: every suffix is "0000".
type zeros struct{}

func (zeros) Read(p []byte) (int, error) { clear(p); return len(p), nil }

type failing struct{}

func (failing) Read([]byte) (int, error) { return 0, errors.New("no randomness") }

// A container's name is <prefix>-<repo>-<last six of the id>-<suffix>: the
// repository without its owner, cut to NameRepoMax; the id as the UI shows
// it, lowercased; four characters of the ULID's alphabet from the reader.
// Every one is a Docker name and one internal/vscode's link accepts.
func TestContainerName(t *testing.T) {
	const id = "01JABCDEFGHJKMNPQRSTVWXYZ0"
	for _, c := range []struct{ prefix, full, want string }{
		{"drydock", "krelinga/alpha", "drydock-alpha-vwxyz0-0000"},
		{"drydock.test", "Owner/My.Repo_2", "drydock.test-My.Repo_2-vwxyz0-0000"},
		{"drydock", "o/" + strings.Repeat("r", 100), "drydock-" + strings.Repeat("r", NameRepoMax) + "-vwxyz0-0000"},
		{"drydock", "o/-dash", "drydock--dash-vwxyz0-0000"},
	} {
		got, err := ContainerName(c.prefix, c.full, id, zeros{})
		if err != nil || got != c.want {
			t.Errorf("%s %s: %q %v, want %q", c.prefix, c.full, got, err, c.want)
		}
		if !dockerguard.ValidName(got) || !regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]*$`).MatchString(got) {
			t.Errorf("%q is not a name Docker or the VS Code link takes", got)
		}
	}
	got, err := ContainerName("drydock", "krelinga/alpha", id, bytes.NewReader([]byte{0, 31, 32, 255}))
	if err != nil || got != "drydock-alpha-vwxyz0-0z0z" {
		t.Errorf("suffix from 0,31,32,255: %q %v", got, err)
	}
	for _, bad := range []struct{ prefix, full, id string }{
		{"drydock", "noslash", id},
		{"drydock", "o/r", "ABC"},
		{"", "o/r", id},
	} {
		if got, err := ContainerName(bad.prefix, bad.full, bad.id, zeros{}); err == nil {
			t.Errorf("%+v: named %q", bad, got)
		}
	}
	if got, err := ContainerName("drydock", "o/r", id, failing{}); err == nil {
		t.Errorf("with no randomness: named %q", got)
	}
}

// Each up's policy names its container afresh: two ups of one workspace —
// a rebuild beside a container still standing — never share a name. With a
// reader that repeats itself (the control) they would.
func TestEachUpNamesItsContainerAfresh(t *testing.T) {
	m := Manager{LabelPrefix: "drydock"}
	s := upSpec(t)
	seen := map[string]bool{}
	for range 50 {
		p, err := m.guardPolicy(s)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.HasPrefix(p.Name, "drydock-foo-vwxyz0-") || seen[p.Name] {
			t.Fatalf("name %q (seen before: %v)", p.Name, seen[p.Name])
		}
		seen[p.Name] = true
	}
	m.Random = zeros{}
	a, _ := m.guardPolicy(s)
	b, _ := m.guardPolicy(s)
	if a.Name != b.Name {
		t.Errorf("control: a fixed reader gave %q and %q", a.Name, b.Name)
	}
	m.Random = failing{}
	if _, _, err := m.Up(context.Background(), s); err == nil {
		t.Error("an up with no name for its container ran")
	}
}

// The name reaches the guard's policy and never up's argv: the CLI does not
// see it, so its container matching and ${devcontainerId} are what they were.
// The policy the guard read during the up names the container; the CLI's
// argv carries no --name.
func TestUpGivesTheGuardTheNameAndTheCLINothing(t *testing.T) {
	m, dir := upThroughGuard(t, "")
	m.Random = zeros{}
	s := upSpec(t)
	if _, _, err := m.Up(context.Background(), s); err != nil {
		t.Fatal(err)
	}
	if a := strings.Join(argv(t, dir, "devcontainer"), " "); strings.Contains(a, "--name") || strings.Contains(a, "vwxyz0-0000") {
		t.Errorf("up's argv carries the name:\n%s", a)
	}
	b, err := os.ReadFile(filepath.Join(dir, "policy-seen.json"))
	if err != nil {
		t.Fatal(err)
	}
	var seen dockerguard.Policy
	if err := json.Unmarshal(b, &seen); err != nil || seen.Name != "drydock.test-foo-vwxyz0-0000" {
		t.Errorf("the guard's policy: %+v", seen)
	}
	if d := argv(t, dir, "docker"); len(d) < 3 || d[1] != "--name" || d[2] != seen.Name {
		t.Errorf("docker ran with %q", d)
	}
}
