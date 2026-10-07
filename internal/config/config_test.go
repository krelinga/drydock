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
		// Uppercase would also slip past CrossSite's exact comparison of
		// registrable domains.
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

// TestValidateRefusesASameSitePreviewDomain: previews must be on a different
// registrable domain from the UI (PF §4, §10.5), or SameSite stops separating
// them. Equal, child, parent and sibling all share it; so do two names under a
// private public suffix the PSL knows, while two such names under different
// owners do not. The controls validate: genuinely different domains, and
// siblings under a public suffix, which browsers treat as different sites.
func TestValidateRefusesASameSitePreviewDomain(t *testing.T) {
	base := Default()
	base.UIOrigin, base.UIHost = "https://drydock.example.com", "drydock.example.com"
	for _, tc := range []struct {
		name, ui, preview string
		ok                bool
	}{
		{"different domain", "drydock.example.com", "drydock-preview.net", true},
		{"different domain, same TLD", "drydock.example.com", "example-preview.com", true},
		{"different owners under a private suffix", "alice.github.io", "bob.github.io", true},
		{"different registrable domain under a multi-label suffix", "drydock.example.co.uk", "preview.example2.co.uk", true},
		{"unlisted TLD, different names", "drydock.lan", "preview.lan", true},
		{"equal", "drydock.example.com", "drydock.example.com", false},
		{"subdomain", "drydock.example.com", "p.drydock.example.com", false},
		{"parent", "drydock.example.com", "example.com", false},
		{"sibling", "drydock.example.com", "preview.example.com", false},
		{"cousin", "drydock.home.example.com", "p.lab.example.com", false},
		{"sibling under a multi-label suffix", "drydock.example.co.uk", "preview.example.co.uk", false},
		{"same owner under a private suffix", "drydock.alice.github.io", "preview.alice.github.io", false},
		{"preview is a public suffix", "drydock.example.com", "co.uk", false},
		{"UI host is a public suffix", "github.io", "drydock-preview.net", false},
		{"UI host is a single label", "drydock", "drydock-preview.net", false},
		{"trailing dot", "drydock.example.com", "preview.example.com.", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := base
			c.UIOrigin, c.UIHost, c.PreviewDomain = "https://"+tc.ui, tc.ui, tc.preview
			err := c.Validate()
			if tc.ok && err != nil {
				t.Fatalf("control: %q beside %q refused: %v", tc.preview, tc.ui, err)
			}
			if !tc.ok && err == nil {
				t.Fatalf("%q beside %q validated: previews would be same-site with the UI", tc.preview, tc.ui)
			}
			if !tc.ok && !strings.Contains(err.Error(), "registrable domain") {
				t.Errorf("error %q does not say why", err)
			}
		})
	}
}
