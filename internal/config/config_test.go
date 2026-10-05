package config

import (
	"strings"
	"testing"
)

// TestValidateRefusesAnUnpinnedCleanupImage: the cleanup helper runs as root
// with a host directory mounted, so its image must be pinned by digest. The
// control is the default, which validates.
func TestValidateRefusesAnUnpinnedCleanupImage(t *testing.T) {
	c := Default()
	c.UIOrigin, c.UIHost = "https://drydock.example.com", "drydock.example.com"
	if err := c.Validate(); err != nil {
		t.Fatalf("control: the defaults do not validate: %v", err)
	}
	for _, img := range []string{"", "busybox", "busybox:1.37.0", "busybox:latest",
		"busybox@sha256:" + strings.Repeat("a", 63), "busybox@sha256:" + strings.Repeat("A", 64),
		"-v@sha256:" + strings.Repeat("a", 64)} {
		c.CleanupImage = img
		if err := c.Validate(); err == nil {
			t.Errorf("cleanup image %q validated", img)
		}
	}
}
