// Package config holds the settings that must not be constants.
//
// Most of this is ordinary. One field is a safety property: LabelPrefix.
package config

import (
	"fmt"
	"regexp"
	"strings"
)

// Config is Drydock's runtime configuration. Nothing here is a credential —
// the App private key and the secrets master key are mode-0400 files read once
// at startup and never placed in a struct that something might log (§13.5).
type Config struct {
	// UIOrigin is the exact origin string the Origin allowlist compares
	// against — "https://drydock.example.com", with no trailing slash and
	// never a suffix pattern (§13.3).
	UIOrigin string
	// UIHost is the hostname Drydock validates the Host header against,
	// independently of Caddy's site block (§13.3).
	UIHost string
	// PreviewDomain is the separate registrable domain previews are served
	// from — "drydock-preview.net". It must not be a subdomain of UIHost:
	// the whole cross-site boundary rests on these being different eTLD+1,
	// and Validate refuses the mistake.
	PreviewDomain string

	// APISocket and PreviewSocket are the two Unix sockets. Drydock binds
	// no TCP port at all (§13.5).
	APISocket     string
	PreviewSocket string
	// SocketGroup owns both sockets at mode 0660, and Caddy is its only
	// other member.
	SocketGroup string

	// WorkspaceRoot is where clones live: /srv/drydock/ws/<id>/repo.
	WorkspaceRoot string
	// DatabasePath is the SQLite file. An advisory lock on it refuses a
	// second Drydock — though see LabelPrefix for what that does *not*
	// cover.
	DatabasePath string

	// LabelPrefix is configuration rather than a constant, and that is a
	// non-negotiable rather than a nicety (§13.5, testing §5.4).
	//
	// Reconciliation adopts, stops, and deletes containers found by
	// `label=<prefix>.workspace`, so whichever Drydock owns a prefix owns
	// those containers — including their delete path. A second instance on
	// the same daemon pointed at the same prefix will adopt the first's
	// work and can destroy it. The SQLite advisory lock does not help,
	// because the second instance has its own database file.
	//
	// That second instance is not hypothetical: it is what a test run is.
	// Tests use "drydock.test.<run-id>", and reconciliation refuses to
	// adopt a container carrying a prefix other than its own.
	LabelPrefix string

	// SupervisorCapacity is what --capacity is set to for each
	// `remote-control` server. Note the pre-created session counts toward
	// it, so 4 buys three on-demand sessions (Spike 02).
	SupervisorCapacity int

	// ContainerCap is how many workspaces may hold a container, or be
	// building one, at once (§1: 5–15, bounded by the dev server's RAM). A
	// create beyond it is refused rather than queued: nothing here stops a
	// workspace on its own, so the operator chooses what to stop.
	ContainerCap int
}

// WorkspaceLabel is the full label key used for adoption and deletion.
func (c Config) WorkspaceLabel() string { return c.LabelPrefix + ".workspace" }

var labelPrefixPattern = regexp.MustCompile(`^[a-z][a-z0-9.-]*[a-z0-9]$`)

// Default returns the production defaults. LabelPrefix is "drydock", which is
// recorded at first run and thereafter compared rather than re-defaulted —
// changing it on an existing install orphans every container.
func Default() Config {
	return Config{
		APISocket:          "/run/drydock/http.sock",
		PreviewSocket:      "/run/drydock/preview.sock",
		SocketGroup:        "drydock",
		WorkspaceRoot:      "/srv/drydock/ws",
		DatabasePath:       "/var/lib/drydock/drydock.db",
		LabelPrefix:        "drydock",
		SupervisorCapacity: 4,
		ContainerCap:       10,
	}
}

// Validate refuses a configuration that would quietly undo a design property.
// Each check here corresponds to something that otherwise fails silently.
func (c Config) Validate() error {
	if c.LabelPrefix == "" || !labelPrefixPattern.MatchString(c.LabelPrefix) {
		return fmt.Errorf("label prefix %q must match %s", c.LabelPrefix, labelPrefixPattern)
	}
	if c.UIOrigin == "" || c.UIHost == "" {
		return fmt.Errorf("UIOrigin and UIHost are required: the Origin check is exact-match, so there is no safe default")
	}
	if !strings.HasPrefix(c.UIOrigin, "https://") {
		return fmt.Errorf("UIOrigin %q must be https: the session cookie is Secure and __Host- prefixed", c.UIOrigin)
	}
	if strings.HasSuffix(c.UIOrigin, "/") {
		return fmt.Errorf("UIOrigin %q must not end in /: it is compared as an exact string against the Origin header", c.UIOrigin)
	}
	if c.PreviewDomain != "" {
		// The cross-site boundary is the whole mechanism (PF §4, §10.2).
		// A preview domain that is a subdomain of the UI host makes
		// previews same-site, and SameSite stops separating repository
		// code from the control plane — silently, with everything still
		// appearing to work.
		if c.PreviewDomain == c.UIHost || strings.HasSuffix(c.PreviewDomain, "."+c.UIHost) {
			return fmt.Errorf("preview domain %q must be a different registrable domain from %q, not a subdomain of it", c.PreviewDomain, c.UIHost)
		}
	}
	if c.APISocket == c.PreviewSocket {
		return fmt.Errorf("the API and preview muxes must not share a socket: their separation is what keeps repository code off the control plane")
	}
	if c.SupervisorCapacity < 1 {
		return fmt.Errorf("supervisor capacity must be at least 1")
	}
	if c.ContainerCap < 1 {
		return fmt.Errorf("container cap must be at least 1")
	}
	return nil
}
