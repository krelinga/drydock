package container

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeVolumes is a docker that keeps volumes as files: vols/<name>.json holds
// what `docker volume inspect` prints for it. create writes one only when the
// name is new — as the real create is a no-op for an existing name — so a
// test can seed a foreign or NFS-backed volume and see what Ensure does.
const fakeVolumes = `d=$(dirname "$0")/vols; mkdir -p "$d"
[ -e "$(dirname "$0")/docker-fail" ] && { echo 'Cannot connect to the Docker daemon' >&2; exit 1; }
case "$1 $2" in
"volume ls") ls "$d" | sed 's/\.json$//' ;;
"volume create")
  shift 2; label=; drv=
  while [ "$1" != -- ]; do case "$1" in --label) label=$2; shift ;; --driver) drv=$2; shift ;; esac; shift; done
  name=$2
  [ -e "$d/$name.json" ] || printf '{"Name":"%s","Driver":"%s","Labels":{"%s":"%s"},"Options":null}' \
    "$name" "$drv" "${label%%=*}" "${label#*=}" >"$d/$name.json"
  echo "$name" ;;
"volume inspect") printf '[%s]\n' "$(cat "$d/$4.json")" ;;
*) exit 64 ;;
esac`

func seedVolume(t *testing.T, dir, name, json string) {
	t.Helper()
	os.MkdirAll(filepath.Join(dir, "vols"), 0o755)
	if err := os.WriteFile(filepath.Join(dir, "vols", name+".json"), []byte(json), 0o644); err != nil {
		t.Fatal(err)
	}
}

func creates(t *testing.T, dir string) [][]string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, "docker.argv"))
	if err != nil {
		return nil
	}
	// The recorder ends each call with a "--" line, and create's own argv
	// has one before the name: a create is the lines up to its second.
	var out [][]string
	lines := strings.Split(string(b), "\n")
	for i := 0; i+1 < len(lines); i++ {
		if lines[i] != "volume" || lines[i+1] != "create" || (i > 0 && lines[i-1] != "--") {
			continue
		}
		var call []string
		seps := 0
		for j := i; j < len(lines) && seps < 2; j++ {
			if lines[j] == "--" {
				seps++
				if seps == 2 {
					break
				}
			}
			call = append(call, lines[j])
		}
		out = append(out, call)
	}
	return out
}

// TestEnsureClaudeVolumeCreatesOnce: the first call makes the volume, local
// and labelled with the prefix; every later call finds it and makes nothing.
// A volume whose name merely contains this one is not mistaken for it —
// docker's name filter matches substrings — which is the positive control
// for "found it, so did not create it".
func TestEnsureClaudeVolumeCreatesOnce(t *testing.T) {
	run, dir := fakes(t, map[string]string{"docker": fakeVolumes})
	seedVolume(t, dir, "drydock-claude-config-old", `{"Name":"drydock-claude-config-old","Driver":"local","Labels":{"drydock.test.v.claude-config":"true"}}`)
	m := Manager{Run: run, LabelPrefix: "drydock.test.v"}
	ctx := context.Background()

	created, err := m.EnsureClaudeVolume(ctx, "drydock-claude-config")
	if err != nil || !created {
		t.Fatalf("first: created %v, err %v", created, err)
	}
	for i := 0; i < 2; i++ {
		created, err = m.EnsureClaudeVolume(ctx, "drydock-claude-config")
		if err != nil || created {
			t.Fatalf("again: created %v, err %v", created, err)
		}
	}
	c := creates(t, dir)
	if len(c) != 1 {
		t.Fatalf("volume create ran %d times: %v", len(c), c)
	}
	if got, want := strings.Join(c[0], " "), "volume create --driver local --label drydock.test.v.claude-config=true -- drydock-claude-config"; got != want {
		t.Errorf("create argv\n got %s\nwant %s", got, want)
	}
}

// TestEnsureClaudeVolumeRefusesForeignAndNetworkVolumes: a volume of the
// configured name that this Drydock did not make — no label, or another
// prefix's — is never adopted, and neither is one that is not a plain local
// volume: a different driver, or the local driver mounting an NFS or CIFS
// export through its options. Neither is created over. The control is the
// same volume labelled and plain, which is accepted.
func TestEnsureClaudeVolumeRefusesForeignAndNetworkVolumes(t *testing.T) {
	const label = `"Labels":{"drydock.claude-config":"true"}`
	for _, c := range []struct {
		name, json string
		want       error
	}{
		{"accepted", `{"Name":"v1","Driver":"local",` + label + `,"Options":null}`, nil},
		{"accepted with empty options", `{"Name":"v1","Driver":"local",` + label + `,"Options":{}}`, nil},
		{"no label", `{"Name":"v1","Driver":"local","Labels":null}`, ErrForeignVolume},
		{"another prefix", `{"Name":"v1","Driver":"local","Labels":{"drydock.test.other.claude-config":"true"}}`, ErrForeignVolume},
		{"nfs through the local driver", `{"Name":"v1","Driver":"local",` + label + `,"Options":{"type":"nfs","o":"addr=10.0.0.2,rw","device":":/export/claude"}}`, ErrVolumeNotLocal},
		{"cifs through the local driver", `{"Name":"v1","Driver":"local",` + label + `,"Options":{"type":"cifs","device":"//nas/claude"}}`, ErrVolumeNotLocal},
		{"another driver", `{"Name":"v1","Driver":"rclone",` + label + `}`, ErrVolumeNotLocal},
	} {
		t.Run(c.name, func(t *testing.T) {
			run, dir := fakes(t, map[string]string{"docker": fakeVolumes})
			seedVolume(t, dir, "v1", c.json)
			created, err := Manager{Run: run, LabelPrefix: "drydock"}.EnsureClaudeVolume(context.Background(), "v1")
			if c.want == nil && err != nil {
				t.Fatalf("refused: %v", err)
			}
			if c.want != nil && !errors.Is(err, c.want) {
				t.Fatalf("err %v; want %v", err, c.want)
			}
			if created || len(creates(t, dir)) != 0 {
				t.Error("an existing volume was created over")
			}
		})
	}
}

func TestEnsureClaudeVolumeRefusesABadNameAndReportsDocker(t *testing.T) {
	run, dir := fakes(t, map[string]string{"docker": fakeVolumes})
	m := Manager{Run: run, LabelPrefix: "drydock"}
	for _, bad := range []string{"", "-rm", "a,target=/", "a=b", "x"} {
		if _, err := m.EnsureClaudeVolume(context.Background(), bad); err == nil {
			t.Errorf("volume name %q accepted", bad)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "docker.argv")); err == nil {
		t.Error("docker ran for a bad name")
	}
	os.WriteFile(filepath.Join(dir, "docker-fail"), nil, 0o644)
	if _, err := m.EnsureClaudeVolume(context.Background(), "v1"); err == nil || !strings.Contains(err.Error(), "Cannot connect") {
		t.Errorf("err %v; want docker's message", err)
	}
}

func TestArgsMountTheClaudeVolume(t *testing.T) {
	m := Manager{LabelPrefix: "drydock"}
	s := spec()
	s.ClaudeVolume = "drydock-claude-config"
	args, err := m.Args(s)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(args, " "); !strings.Contains(got, "--mount type=volume,source=drydock-claude-config,target=/home/vscode/.claude") {
		t.Errorf("argv lacks the volume mount:\n%s", got)
	}
	for _, bad := range []string{"v,target=/etc", "v=1", "-v"} {
		s.ClaudeVolume = bad
		if _, err := m.Args(s); err == nil {
			t.Errorf("volume name %q accepted into --mount", bad)
		}
	}
}
