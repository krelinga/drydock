// Package config holds the settings that must not be constants.
//
// Most of this is ordinary. One field is a safety property: LabelPrefix.
package config

import (
	"fmt"
	"net"
	"regexp"
	"strings"
	"time"

	"golang.org/x/net/publicsuffix"
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
	// from — "drydock-preview.net". It must not share UIHost's registrable
	// domain (eTLD+1): not equal, not a subdomain, not a parent, not a
	// sibling. The whole cross-site boundary rests on these being different
	// sites, and Validate refuses the mistake (CrossSite).
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

	// DiskLimitPercent is how full the filesystem holding WorkspaceRoot may
	// be before a create, start or rebuild is refused (design §12, *Disk
	// full*): at or above it, the pre-flight check refuses rather than let a
	// clone or an image build fail part-way. 100 turns the check off.
	DiskLimitPercent int

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

	// ClaudeVolume is the shared Claude credential volume (§7.1): the local
	// Docker volume every workspace container mounts at its CLAUDE_CONFIG_DIR
	// (§6 step 4) — one login, shared by all of them — and the one the
	// identity watch reads (§7.3). Step 4 creates it with the local driver
	// and labels it with LabelPrefix, and refuses one of this name that lacks
	// the label or is not a plain local volume — the refresh lock inside it
	// needs mkdir to be atomic, which NFS and CIFS do not give (Spike 00);
	// the watch refuses to read one without the label. Configuration so a
	// test Drydock, with its own prefix, never touches the real login. Where
	// it is mounted is not configuration: it is the Feature's
	// CLAUDE_CONFIG_DIR (container.ClaudeConfigMountPoint).
	ClaudeVolume string
	// ClaudeBaseImage is the image Drydock builds its Claude image from —
	// the short-lived container that runs `claude auth status --json`
	// against the volume, read-only (§7.3), and that the login handshake
	// will reuse (§7.2). Pinned by digest, as CleanupImage is; Claude Code
	// itself is pinned by version (classify.ClaudeCodeVersion).
	ClaudeBaseImage string
	// IdentityInterval is how often the identity watch reads the volume
	// (§7.3: six hours). It also runs at boot and on demand.
	IdentityInterval time.Duration
	// IdentityExpiringWindow is §7.3's warning window: a login whose
	// refresh token (Claude Code's refreshTokenExpiresAt) expires within it
	// is `expiring` rather than `ok`. Three days, which is when Claude Code
	// itself starts warning (§2.4). Never measured against the access
	// token's expiresAt, which a real login sets about eight hours out.
	IdentityExpiringWindow time.Duration
	// IdentityCheckTimeout bounds each read an identity check makes (the
	// credential file, `auth status`): each is a container that should be
	// done in seconds, and every workspace can write the file it reads, so
	// one made never to end must cost one failed check, not the watch
	// (§7.3). The Claude image's first build has its own, longer bound.
	IdentityCheckTimeout time.Duration
}

// DefaultClaudeVolume is the shared Claude credential volume's name (§6).
const DefaultClaudeVolume = "drydock-claude-config"

// DefaultClaudeBaseImage is node 22 on bookworm-slim by its multi-arch index
// digest: npm is how Claude Code is installed at an exact version, and the
// binary it installs needs glibc.
const DefaultClaudeBaseImage = "node:22-bookworm-slim@sha256:43ac6c60b8f89723f746e8a92ce91abd5017e627ce1ddfe4238355d3a30b772c"

// volumeNamePattern is Docker's own rule for a volume name, and so nothing
// that could read as an option or a second --mount field.
var volumeNamePattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]+$`)

// ValidVolumeName reports whether name can be the shared credential volume.
// The one rule: Validate, step 4 (internal/container) and the identity watch
// all ask it.
func ValidVolumeName(name string) bool { return volumeNamePattern.MatchString(name) }

// DefaultCleanupImage is busybox 1.37.0 by its multi-arch index digest. The
// helper needs only `find` with -mindepth and -delete.
const DefaultCleanupImage = "busybox:1.37.0@sha256:bdf57e528e45e4433820e045b29b4597825a1c9e38353532d90a01445013f82e"

var cleanupImagePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9./_:-]*@sha256:[0-9a-f]{64}$`)

// DefaultFeature is the published Feature, by major tag. Major 1 is the
// Feature with Claude Code (§11): it refuses a container without the shared
// credential volume this Drydock mounts, so a Drydock from before it, which
// mounts none, stays on :0.
const DefaultFeature = "ghcr.io/krelinga/drydock/drydock:1"

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
		DiskLimitPercent:   90,
		Feature:            DefaultFeature,
		// The production App, krelinga-drydock (App ID 5189455), whose bot
		// user is 337840004 — measured, §9.3.
		BotName:          "krelinga-drydock[bot]",
		BotEmail:         "337840004+krelinga-drydock[bot]@users.noreply.github.com",
		ProvisionTimeout: 30 * time.Minute,
		CleanupImage:     DefaultCleanupImage,

		ClaudeVolume:           DefaultClaudeVolume,
		ClaudeBaseImage:        DefaultClaudeBaseImage,
		IdentityInterval:       6 * time.Hour,
		IdentityExpiringWindow: 72 * time.Hour,
		IdentityCheckTimeout:   2 * time.Minute,
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
	// Lowercase, all three. A browser serializes an origin's host in
	// lowercase and the Origin check is an exact string comparison, so an
	// origin configured as https://Drydock.Example.com refuses every sign-in
	// with forbidden_origin while the case-insensitive Host check lets the
	// same requests through. And CrossSite below compares registrable
	// domains as exact strings, which a difference in case would slip past.
	for _, f := range []struct{ name, flag, value string }{
		{"UIOrigin", "--ui-origin", c.UIOrigin},
		{"UIHost", "--ui-host", c.UIHost},
		{"preview domain", "--preview-domain", c.PreviewDomain},
	} {
		if f.value != strings.ToLower(f.value) {
			return fmt.Errorf("%s %q must be lowercase (%s %s): browsers send hostnames lowercased and the Origin check is exact, so every sign-in would be refused", f.name, f.value, f.flag, strings.ToLower(f.value))
		}
	}
	if c.PreviewDomain != "" {
		// The cross-site boundary is the whole mechanism (PF §4, §10.2).
		// A preview domain that is a subdomain of the UI host makes
		// previews same-site, and SameSite stops separating repository
		// code from the control plane — silently, with everything still
		// appearing to work.
		if err := CrossSite(c.UIHost, c.PreviewDomain); err != nil {
			return err
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
	if c.DiskLimitPercent < 1 || c.DiskLimitPercent > 100 {
		return fmt.Errorf("the disk limit is a percentage from 1 to 100 (100 turns the check off)")
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
	// Without the shared volume every workspace would need its own login,
	// and the Feature refuses a container with nothing at CLAUDE_CONFIG_DIR.
	if !ValidVolumeName(c.ClaudeVolume) {
		return fmt.Errorf("claude volume %q must be a Docker volume name", c.ClaudeVolume)
	}
	// The Claude image reads the login every container runs on, as root
	// with DAC_READ_SEARCH; a tag can be moved to different content.
	if !cleanupImagePattern.MatchString(c.ClaudeBaseImage) {
		return fmt.Errorf("claude base image %q must be pinned by digest (name@sha256:<64 hex>): it reads the shared login", c.ClaudeBaseImage)
	}
	if c.IdentityInterval < time.Minute {
		return fmt.Errorf("identity interval %s is shorter than a minute: each check starts a container", c.IdentityInterval)
	}
	// Zero would make `expiring` unreachable, and the banner would go from
	// nothing straight to "signed out" — the one warning §2.4 says to give.
	if c.IdentityExpiringWindow <= 0 {
		return fmt.Errorf("identity expiring window %s must be positive", c.IdentityExpiringWindow)
	}
	// Zero would be no bound at all — the hang it exists for — and a few
	// seconds would fail a healthy check on a busy daemon.
	if c.IdentityCheckTimeout < 10*time.Second {
		return fmt.Errorf("identity check timeout %s is shorter than ten seconds: each read starts a container", c.IdentityCheckTimeout)
	}
	return nil
}

// CrossSite refuses a preview domain that is same-site with the UI host: one
// whose registrable domain (eTLD+1, by the Public Suffix List browsers use to
// decide SameSite) is the UI host's. Equal, subdomain, parent and sibling are
// all the same mistake. preview.example.com beside drydock.example.com is
// same-site, so the session cookie's SameSite=Lax stops separating repository
// code from the control plane, silently (PF §4, §10.5; overall §13.3). A name
// whose site cannot be decided (a public suffix itself, or empty) is refused
// rather than guessed at; an IP address is its own site. Both names are
// expected lowercase, which Validate checks first. The installer asks this
// through `drydock check-preview-domain` rather than reimplementing the list.
func CrossSite(uiHost, previewDomain string) error {
	ui, err := site(uiHost)
	if err != nil {
		return fmt.Errorf("UI host %q %v, so no preview domain can be proved cross-site with it", uiHost, err)
	}
	pv, err := site(previewDomain)
	if err != nil {
		return fmt.Errorf("preview domain %q %v", previewDomain, err)
	}
	if ui == pv {
		return fmt.Errorf("preview domain %q must be a different registrable domain from %q: both are on %q, which makes previews same-site with the UI", previewDomain, uiHost, ui)
	}
	return nil
}

// site is a host's registrable domain: the unit SameSite compares.
func site(host string) (string, error) {
	host = strings.TrimSuffix(host, ".")
	if host == "" {
		return "", fmt.Errorf("is empty")
	}
	if net.ParseIP(host) != nil {
		return host, nil
	}
	s, err := publicsuffix.EffectiveTLDPlusOne(host)
	if err != nil {
		return "", fmt.Errorf("has no registrable domain (%v)", err)
	}
	return s, nil
}
