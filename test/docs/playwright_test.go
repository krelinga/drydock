package docs

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"testing"
)

// CI's browser job runs in Playwright's own image, which carries the browser
// builds of exactly one Playwright release. web/ pins @playwright/test, and a
// bump there that leaves the image behind would run the new test runner
// against the old browsers: Playwright refuses to launch a build it does not
// expect, or, worse, a §11.6 bump would be measured on the browsers it was
// meant to replace. So the image's tag must be the lockfile's version, and
// the image must be pinned by digest as well.
var playwrightImageRE = regexp.MustCompile(`mcr\.microsoft\.com/playwright:v([0-9]+\.[0-9]+\.[0-9]+)-([a-z]+)(@sha256:[0-9a-f]{64})?`)

func TestPlaywrightImageMatchesLockfile(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(root, "web", "package-lock.json"))
	if err != nil {
		t.Fatal(err)
	}
	var lock struct {
		Packages map[string]struct {
			Version string `json:"version"`
		} `json:"packages"`
	}
	if err := json.Unmarshal(raw, &lock); err != nil {
		t.Fatal(err)
	}
	want := lock.Packages["node_modules/@playwright/test"].Version
	if want == "" {
		t.Fatal("web/package-lock.json names no @playwright/test version")
	}
	if core := lock.Packages["node_modules/playwright-core"].Version; core != want {
		t.Errorf("web/package-lock.json: playwright-core %s beside @playwright/test %s", core, want)
	}

	ci, err := os.ReadFile(filepath.Join(root, ".github", "workflows", "ci.yml"))
	if err != nil {
		t.Fatal(err)
	}
	refs := playwrightImageRE.FindAllSubmatch(ci, -1)
	// The positive control: without a reference this test would pass for a
	// browser job that no longer uses the image at all.
	if len(refs) == 0 {
		t.Fatal("ci.yml names no mcr.microsoft.com/playwright image; the browser job runs in one")
	}
	for _, m := range refs {
		if got := string(m[1]); got != want {
			t.Errorf("ci.yml runs the browser job in %s, but web/package-lock.json pins @playwright/test %s: bump the image with the package (testing §11.6)", m[0], want)
		}
		if len(m[3]) == 0 {
			t.Errorf("ci.yml's %s is not pinned by digest: append @sha256:… from `docker buildx imagetools inspect`", m[0])
		}
	}
}
