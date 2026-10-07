package container

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestTheFeatureTestsPrepareVolumesAsStepFourDoes: the Feature's own tests
// prepare their volumes with a copy of the owner helper's script
// (feature/prepare-test-volumes.sh), since that job has no Drydock to run.
// A copy drifts silently, so this reads it and compares. The control is that
// the markers are found at all.
func TestTheFeatureTestsPrepareVolumesAsStepFourDoes(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "..", "feature", "prepare-test-volumes.sh"))
	if err != nil {
		t.Fatal(err)
	}
	_, rest, ok1 := strings.Cut(string(b), "# BEGIN ownerScript\n")
	body, _, ok2 := strings.Cut(rest, "\n# END ownerScript")
	if !ok1 || !ok2 {
		t.Fatal("the script's ownerScript markers are missing")
	}
	if want := "owner_script='" + ownerScript + "'"; body != want {
		t.Errorf("feature/prepare-test-volumes.sh has drifted from ownerScript:\n got %s\nwant %s", body, want)
	}
	if !strings.Contains(string(b), "busybox:1.37.0@sha256:") {
		t.Error("the script's image is not the pinned busybox")
	}
}
