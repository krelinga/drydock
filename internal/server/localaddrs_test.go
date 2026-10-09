//go:build !browsertier

package server

import (
	"os"
	"strings"
	"testing"
)

// TestReleaseBuildRefusesLocalAddresses: the build a release ships asks the
// kernel for this host's addresses (a nil LocalAddrs), and the release
// script builds with no tags — so the browser tier's -tags browsertier, which
// lists none, can never be what an operator runs.
func TestReleaseBuildRefusesLocalAddresses(t *testing.T) {
	if previewLocalAddrs != nil || browserTierBuild {
		t.Fatal("the untagged build does not refuse this host's own addresses")
	}
	b, err := os.ReadFile("../../deploy/package.sh")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), "go build") {
		t.Fatal("control: deploy/package.sh no longer runs go build; move this check to wherever the release is built")
	}
	if strings.Contains(string(b), "-tags") || strings.Contains(string(b), "browsertier") {
		t.Error("deploy/package.sh builds with tags")
	}
}
