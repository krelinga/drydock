package docs

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// The extension is versioned with Drydock (vscode-extension §9.2): one tag,
// one version. release-please moves extension/package.json's version through
// release-please-config.json's extra-files, so the two can differ only when
// someone edits one by hand — or when that extra-files entry is lost, which
// this also checks, since a release would then ship a .vsix claiming the
// previous version.
func TestExtensionVersionIsTheRelease(t *testing.T) {
	read := func(name string, v any) {
		t.Helper()
		b, err := os.ReadFile(filepath.Join(root, name))
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(b, v); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
	}
	var manifest map[string]string
	read(".release-please-manifest.json", &manifest)
	var pkg struct {
		Version string `json:"version"`
	}
	read("extension/package.json", &pkg)
	// The control: both were read.
	if manifest["."] == "" || pkg.Version == "" {
		t.Fatalf("versions not found: manifest %q, extension %q", manifest["."], pkg.Version)
	}
	if pkg.Version != manifest["."] {
		t.Errorf("extension/package.json is version %s, the release is %s: they move together (release-please-config.json extra-files)", pkg.Version, manifest["."])
	}

	var cfg struct {
		Packages map[string]struct {
			ExtraFiles []struct {
				Type, Path, JSONPath string
			} `json:"extra-files"`
		} `json:"packages"`
	}
	read("release-please-config.json", &cfg)
	found := false
	for _, f := range cfg.Packages["."].ExtraFiles {
		if f.Type == "json" && f.Path == "extension/package.json" && f.JSONPath == "$.version" {
			found = true
		}
	}
	if !found {
		t.Error(`release-please-config.json does not move extension/package.json's version: its "." package needs extra-files {"type": "json", "path": "extension/package.json", "jsonpath": "$.version"}`)
	}
}
