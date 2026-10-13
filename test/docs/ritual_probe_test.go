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

// The connect ritual (test/rituals/vscode-connect.md, 2.1) records the probe
// .vsix's SHA-256 so a run can be audited: anyone re-packs the commit and
// compares. That holds only if pack.py is reproducible and the digest the
// ritual names is the one the probe's sources pack to now, so a change to
// the probe that leaves the ritual's digest behind fails here.
func TestRitualProbeDigest(t *testing.T) {
	py, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 is not installed")
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

	pack := func(name string) string {
		out := filepath.Join(t.TempDir(), name)
		cmd := exec.Command(py, filepath.Join(root, "test/rituals/vscode-probe/pack.py"), out)
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
	// A second pack after the sources' mtimes move must not change a byte.
	now := filepath.Join(root, "test/rituals/vscode-probe/extension.js")
	if st, err := os.Stat(now); err == nil {
		_ = os.Chtimes(now, st.ModTime().Add(7*time.Second), st.ModTime().Add(7*time.Second))
		defer os.Chtimes(now, st.ModTime(), st.ModTime())
	}
	if second := pack("b.vsix"); second != first {
		t.Fatalf("pack.py is not reproducible: %s then %s", first, second)
	}
	if first != want {
		t.Fatalf("the probe packs to %s, but vscode-connect.md's expected digest is %s: update the ritual", first, want)
	}
}
