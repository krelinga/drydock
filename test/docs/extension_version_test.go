package docs

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// extraFile is one of release-please-config.json's extra-files.
type extraFile struct {
	Type     string `json:"type"`
	Path     string `json:"path"`
	JSONPath string `json:"jsonpath"`
}

// The version stamps the extension carries, each with the extra-files entry
// that moves it.
var extensionStamps = []extraFile{
	{"json", "extension/package.json", "$.version"},
	{"json", "extension/package-lock.json", "$.version"},
	{"json", "extension/package-lock.json", `$.packages[""].version`},
}

func readJSON(t *testing.T, name string, v any) {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(root, name))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, v); err != nil {
		t.Fatalf("%s: %v", name, err)
	}
}

// The extension is versioned with Drydock (vscode-extension §9.2): one tag,
// one version. release-please moves the extension's version stamps through
// release-please-config.json's extra-files, so they can differ from the
// release only when someone edits one by hand — or when an extra-files entry
// is lost, which this also checks, since a release would then ship a .vsix
// (or a lock file) claiming the previous version.
func TestExtensionVersionIsTheRelease(t *testing.T) {
	var manifest map[string]string
	readJSON(t, ".release-please-manifest.json", &manifest)
	var pkg struct {
		Version string `json:"version"`
	}
	readJSON(t, "extension/package.json", &pkg)
	var lock struct {
		Version  string `json:"version"`
		Packages map[string]struct {
			Version string `json:"version"`
		} `json:"packages"`
	}
	readJSON(t, "extension/package-lock.json", &lock)

	release := manifest["."]
	// The control: the release and every stamp were read.
	if release == "" || pkg.Version == "" || lock.Version == "" || lock.Packages[""].Version == "" {
		t.Fatalf("versions not found: release %q, package.json %q, lock %q, lock packages[\"\"] %q",
			release, pkg.Version, lock.Version, lock.Packages[""].Version)
	}
	for _, s := range []struct{ where, got string }{
		{"extension/package.json $.version", pkg.Version},
		{"extension/package-lock.json $.version", lock.Version},
		{`extension/package-lock.json $.packages[""].version`, lock.Packages[""].Version},
	} {
		if s.got != release {
			t.Errorf("%s is %s, the release is %s: they move together (release-please-config.json extra-files)", s.where, s.got, release)
		}
	}

	var cfg struct {
		Packages map[string]struct {
			ExtraFiles []extraFile `json:"extra-files"`
		} `json:"packages"`
	}
	readJSON(t, "release-please-config.json", &cfg)
	have := map[extraFile]bool{}
	for _, f := range cfg.Packages["."].ExtraFiles {
		have[f] = true
	}
	for _, want := range extensionStamps {
		if !have[want] {
			t.Errorf("release-please-config.json does not move %s %s: its \".\" package needs the extra-files entry %+v", want.Path, want.JSONPath, want)
		}
	}
}
