package classify

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestTheFeaturePinsTheRecordedVersion: the Claude Code the Feature installs
// by default, the version its install.sh carries checksums for, and the
// version CLAUDE.md says the spikes were re-measured on are all the version
// this corpus was recorded against. Drydock passes ClaudeCodeVersion to the
// Feature explicitly and step 7 refuses a container reporting another, so a
// drift here fails every workspace — loudly, but at a distance from the bump
// that caused it. This is where it fails first.
func TestTheFeaturePinsTheRecordedVersion(t *testing.T) {
	root := filepath.Join("..", "..")
	b, err := os.ReadFile(filepath.Join(root, "feature", "src", "drydock", "devcontainer-feature.json"))
	if err != nil {
		t.Fatal(err)
	}
	var f struct {
		Options map[string]struct {
			Default any `json:"default"`
		} `json:"options"`
		ContainerEnv map[string]string `json:"containerEnv"`
	}
	if err := json.Unmarshal(b, &f); err != nil {
		t.Fatal(err)
	}
	if got := f.Options["claudeCodeVersion"].Default; got != ClaudeCodeVersion {
		t.Errorf("the Feature's claudeCodeVersion default is %v; the corpus is %s", got, ClaudeCodeVersion)
	}
	if f.ContainerEnv["DISABLE_AUTOUPDATER"] != "1" {
		t.Errorf("the Feature's containerEnv does not set DISABLE_AUTOUPDATER=1: %v", f.ContainerEnv)
	}

	sh, err := os.ReadFile(filepath.Join(root, "feature", "src", "drydock", "install.sh"))
	if err != nil {
		t.Fatal(err)
	}
	pin := regexp.MustCompile(`(?m)^CLAUDE_PIN=(\S+)$`).FindSubmatch(sh)
	if pin == nil || string(pin[1]) != ClaudeCodeVersion {
		t.Errorf("install.sh's CLAUDE_PIN is %q; the corpus is %s", pin, ClaudeCodeVersion)
	}
	for _, p := range []string{"linux-x64", "linux-arm64", "linux-x64-musl", "linux-arm64-musl"} {
		if !regexp.MustCompile(`(?m)^\s+` + regexp.QuoteMeta(p) + `\) echo [0-9a-f]{64} ;;$`).Match(sh) {
			t.Errorf("install.sh pins no checksum for %s", p)
		}
	}

	md, err := os.ReadFile(filepath.Join(root, "CLAUDE.md"))
	if err != nil {
		t.Fatal(err)
	}
	if flat := strings.Join(strings.Fields(string(md)), " "); !strings.Contains(flat, "re-measured on `"+ClaudeCodeVersion+"`") {
		t.Errorf("CLAUDE.md does not say the spikes are re-measured on %s", ClaudeCodeVersion)
	}
}
