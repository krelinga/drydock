package config

import (
	"strings"
	"testing"
	"time"
)

// TestValidateRefusesABrokenIdentityWatch: the Claude image reads the shared
// login, so it is pinned by digest like the cleanup helper; the volume must be
// a name Docker accepts; and a zero expiring window would make `expiring`
// unreachable. The control is the defaults, which validate, and each case
// changes one field from them.
func TestValidateRefusesABrokenIdentityWatch(t *testing.T) {
	base := Default()
	base.UIOrigin, base.UIHost = "https://drydock.example.com", "drydock.example.com"
	if err := base.Validate(); err != nil {
		t.Fatalf("control: the defaults do not validate: %v", err)
	}
	if base.ClaudeVolume != "drydock-claude-config" || base.IdentityInterval != 6*time.Hour ||
		base.IdentityExpiringWindow != 72*time.Hour || base.IdentityCheckTimeout != 2*time.Minute {
		t.Errorf("defaults moved from the design's values (§6, §7.3): %q %s %s",
			base.ClaudeVolume, base.IdentityInterval, base.IdentityExpiringWindow)
	}
	for name, mut := range map[string]func(*Config){
		"unpinned image":  func(c *Config) { c.ClaudeBaseImage = "node:22-bookworm-slim" },
		"empty image":     func(c *Config) { c.ClaudeBaseImage = "" },
		"empty volume":    func(c *Config) { c.ClaudeVolume = "" },
		"volume as path":  func(c *Config) { c.ClaudeVolume = "/var/lib/x" },
		"volume as flag":  func(c *Config) { c.ClaudeVolume = "-v" },
		"volume w/ comma": func(c *Config) { c.ClaudeVolume = "a,readonly=false" },
		"zero window":     func(c *Config) { c.IdentityExpiringWindow = 0 },
		"tiny interval":   func(c *Config) { c.IdentityInterval = time.Second },
		"no check bound":  func(c *Config) { c.IdentityCheckTimeout = 0 },
		"tiny check":      func(c *Config) { c.IdentityCheckTimeout = time.Second },
	} {
		c := base
		mut(&c)
		if err := c.Validate(); err == nil {
			t.Errorf("%s validated", name)
		}
	}
}

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

// TestValidateRefusesUppercaseHosts: a browser sends the Origin's host in
// lowercase and the Origin check is exact, so a mixed-case UIOrigin refuses
// every sign-in while the case-insensitive Host check lets the same requests
// through. Validate refuses it at startup instead, naming the lowercase fix.
// The control is the same settings in lowercase, which validate.
func TestValidateRefusesUppercaseHosts(t *testing.T) {
	base := Default()
	base.UIOrigin, base.UIHost, base.PreviewDomain = "https://drydock.example.com", "drydock.example.com", "drydock-preview.net"
	if err := base.Validate(); err != nil {
		t.Fatalf("control: lowercase settings do not validate: %v", err)
	}
	for _, tc := range []struct {
		name   string
		mutate func(*Config)
		want   string
	}{
		{"origin", func(c *Config) { c.UIOrigin = "https://Drydock.example.com" }, "--ui-origin https://drydock.example.com"},
		{"host", func(c *Config) { c.UIHost = "drydock.Example.com" }, "--ui-host drydock.example.com"},
		{"both", func(c *Config) { c.UIOrigin, c.UIHost = "https://DRYDOCK.EXAMPLE.COM", "DRYDOCK.EXAMPLE.COM" }, "must be lowercase"},
		// Uppercase would also slip past the suffix match that keeps the
		// preview domain off the UI's registrable domain.
		{"preview", func(c *Config) { c.PreviewDomain = "P.DRYDOCK.EXAMPLE.COM" }, "--preview-domain p.drydock.example.com"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := base
			tc.mutate(&c)
			err := c.Validate()
			if err == nil {
				t.Fatalf("validated %q / %q / %q", c.UIOrigin, c.UIHost, c.PreviewDomain)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not name the fix %q", err, tc.want)
			}
		})
	}
}
