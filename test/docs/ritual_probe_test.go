package docs

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"testing"
	"time"
)

// The probe's sources, copied whole: pack.py packs the files beside it.
var probeFiles = []string{"pack.py", "package.json", "extension.js"}

// The connect ritual (test/rituals/vscode-connect.md, 2.1) records the probe
// .vsix's SHA-256 so a run can be audited: anyone re-packs the commit and
// compares. That holds only if pack.py is reproducible and the digest the
// ritual names is the one the probe's sources pack to now, so a change to
// the probe that leaves the ritual's digest behind fails here. It packs a
// copy, so the checkout is never touched.
func TestRitualProbeDigest(t *testing.T) {
	py, err := exec.LookPath("python3")
	if err != nil {
		if os.Getenv("DRYDOCK_REQUIRE_PYTHON") != "" {
			t.Fatal("DRYDOCK_REQUIRE_PYTHON is set and python3 is not installed")
		}
		t.Skip("python3 is not installed (set DRYDOCK_REQUIRE_PYTHON to fail instead)")
	}
	doc, err := os.ReadFile(filepath.Join(root, "test/rituals/vscode-connect.md"))
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile("expected digest: `([0-9a-f]{64})`").FindSubmatch(doc)
	if m == nil {
		t.Fatal("vscode-connect.md names no `expected digest: `<sha256>``")
	}
	want := string(m[1])

	src := t.TempDir()
	for _, name := range probeFiles {
		b, err := os.ReadFile(filepath.Join(root, "test/rituals/vscode-probe", name))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(src, name), b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	pack := func(name string) string {
		out := filepath.Join(t.TempDir(), name)
		cmd := exec.Command(py, filepath.Join(src, "pack.py"), out)
		if b, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("pack.py: %v\n%s", err, b)
		}
		b, err := os.ReadFile(out)
		if err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256(b)
		return hex.EncodeToString(sum[:])
	}
	first := pack("a.vsix")
	// Move the copies' mtimes: a second pack must not change a byte.
	later := time.Date(2001, 2, 3, 4, 5, 6, 0, time.UTC)
	for _, name := range probeFiles {
		if err := os.Chtimes(filepath.Join(src, name), later, later); err != nil {
			t.Fatal(err)
		}
	}
	if second := pack("b.vsix"); second != first {
		t.Fatalf("pack.py is not reproducible: %s, then %s after the sources' mtimes moved", first, second)
	}
	if first != want {
		t.Fatalf("the probe packs to %s, but vscode-connect.md's expected digest is %s: update the ritual", first, want)
	}
}
