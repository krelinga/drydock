// Package config holds the settings that must not be constants.
//
// Most of this is ordinary. One field is a safety property: LabelPrefix.
package config

import (
	"fmt"
	"regexp"
	"strings"
	"time"
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
	// BrokerDir holds one token-broker socket per workspace (§6 step 5),
	// each bind-mounted into its own container. Drydock makes it 0700:
	// that directory, not the sockets' mode, is what keeps host users out.
	BrokerDir string
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

	// GitHubAppID and GitHubAppKey are the GitHub App (§9): its numeric App
	// ID — not the Client ID — and the path of its private key, a file
	// readable by Drydock alone (mode 0400, §13.5). Never the key itself and
	// never an environment variable. Both or neither: without them Drydock
	// serves, but has no repository list.
	GitHubAppID  int64
	GitHubAppKey string
	// GitHubAPI is the REST API's base URL; a test points it at a fake.
	GitHubAPI string

	// SecretsKey is the path of the secrets master key (§10.2): 32 raw
	// bytes in a file readable by Drydock alone (mode 0400, §13.5), read
	// once at startup. Never the key and never an environment variable.
	// Empty means no secrets can be stored; the secret routes say so. Lose
	// the file and every stored value is unreadable — there is no recovery
	// but storing each one again.
	SecretsKey string
	// Feature is the devcontainer Feature every workspace gets through
	// --additional-features (§6 step 6, §11): an OCI reference, by major
	// tag so a compatible Feature release reaches new containers without a
	// Drydock release. Configuration rather than a constant so a staging
	// Drydock can point at a pre-release Feature.
	Feature string
	// BotName and BotEmail are the Feature's botName and botEmail options:
	// the GitHub App's bot identity, which commits made in a workspace carry
	// (§9.3). The email is built from the bot USER id, not the App ID, and
	// both depend on which App this is — so they are configuration, with the
	// production App's values as the default. Empty leaves git's identity
	// unset in the container.
	BotName  string
	BotEmail string
	// ProvisionTimeout bounds one provisioning run, clone to probe. An image
	// build is the long part; a run past this is failed and named, never
	// left building forever (testing §6.5's "slow" fixture).
	ProvisionTimeout time.Duration
	// CleanupImage is the image a delete runs, as root with no network, to
	// remove what the drydock user cannot: files a root process in the
	// container left in the clone (design §6). Pinned by digest, and
	// Validate refuses anything that is not, for the reason the Feature's
	// Claude Code version is pinned (§11): it runs as root with a host
	// directory mounted, and a tag can be moved to different content.
	// Configuration so a host with a registry mirror can name its own copy.
	CleanupImage string
}

// DefaultCleanupImage is busybox 1.37.0 by its multi-arch index digest. The
// helper needs only `find` with -mindepth and -delete.
const DefaultCleanupImage = "busybox:1.37.0@sha256:bdf57e528e45e4433820e045b29b4597825a1c9e38353532d90a01445013f82e"

var cleanupImagePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9./_:-]*@sha256:[0-9a-f]{64}$`)

// DefaultFeature is the published Feature, by major tag.
const DefaultFeature = "ghcr.io/krelinga/drydock/drydock:0"

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
		BrokerDir:          "/run/drydock/sock",
		DatabasePath:       "/var/lib/drydock/drydock.db",
		LabelPrefix:        "drydock",
		SupervisorCapacity: 4,
		ContainerCap:       10,
		Feature:            DefaultFeature,
		// The production App, krelinga-drydock (App ID 5189455), whose bot
		// user is 337840004 — measured, §9.3.
		BotName:          "krelinga-drydock[bot]",
		BotEmail:         "337840004+krelinga-drydock[bot]@users.noreply.github.com",
		ProvisionTimeout: 30 * time.Minute,
		CleanupImage:     DefaultCleanupImage,
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
	if (c.GitHubAppID != 0) != (c.GitHubAppKey != "") {
		return fmt.Errorf("the GitHub App needs both its App ID and its private key path, or neither")
	}
	if c.GitHubAppID < 0 {
		return fmt.Errorf("GitHub App ID %d must be positive", c.GitHubAppID)
	}
	// The Feature is what gives a workspace its broker clients; without it
	// the socket is mounted and nothing in the container can use it.
	if c.Feature == "" || strings.ContainsAny(c.Feature, " \t\n\"") {
		return fmt.Errorf("feature %q must be a devcontainer Feature reference", c.Feature)
	}
	if strings.ContainsAny(c.BotName+c.BotEmail, "\n\r\x00") {
		return fmt.Errorf("the bot name and email must be single-line")
	}
	if c.ProvisionTimeout < time.Minute {
		return fmt.Errorf("provision timeout %s is shorter than any image build", c.ProvisionTimeout)
	}
	if !cleanupImagePattern.MatchString(c.CleanupImage) {
		return fmt.Errorf("cleanup image %q must be pinned by digest (name@sha256:<64 hex>): it runs as root with a host directory mounted", c.CleanupImage)
	}
	return nil
}
