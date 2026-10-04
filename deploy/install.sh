#!/usr/bin/env bash
# Drydock installer: installs, or upgrades in place. Safe to re-run.
#
#   curl -fsSL https://github.com/krelinga/drydock/releases/latest/download/install.sh \
#     | sudo bash -s -- --ui-host drydock.example.com \
#         --cert /etc/ssl/drydock.pem --key /etc/ssl/drydock.key
#
# Re-run the same line — with or without the flags — to upgrade to the latest
# release. The first run's settings are kept in /etc/drydock/drydock.env, so a
# re-run needs none of them.
#
# Two modes, one file:
#   * run from an extracted release tarball, it installs the binary beside it;
#   * run on its own (as above), it downloads the release tarball for this
#     machine, checks it against the release's SHA256SUMS, and re-runs the copy
#     of itself inside.
#
# What it does NOT do: build Drydock, obtain certificates (provisioned
# externally by design, §13.1), install Caddy, or touch the firewall.
#
# Everything is inside main(), called on the last line, so a download cut off
# part-way through executes nothing.

set -euo pipefail

REPO="krelinga/drydock"
# The standalone copy of this script published with each release has this
# stamped with that release's tag, so it fetches the matching tarball. Unstamped
# (a copy from a git checkout), it fetches the latest release.
RELEASE_VERSION="__DRYDOCK_RELEASE_VERSION__"

BIN=/usr/local/bin/drydock
CONF_DIR=/etc/drydock
CONF=$CONF_DIR/drydock.env
UNIT=/etc/systemd/system/drydock.service
CADDYFILE=/etc/caddy/Caddyfile
SITES_DIR=/etc/caddy/drydock.d
CADDY_DROPIN=/etc/systemd/system/caddy.service.d/drydock.conf
DB=/var/lib/drydock/drydock.db
# The GitHub App's private key: the one credential Drydock stores (design §4).
# A file Drydock alone can read, never an environment variable (§13.5).
APP_KEY=$CONF_DIR/github-app.pem
API_SOCKET=/run/drydock/http.sock

# Markers that identify files this installer owns. A Caddyfile without the
# first is someone else's, and is never overwritten without --take-over-caddy.
CADDYFILE_MARKER="# Drydock's entire LAN-facing surface"
PREVIEW_MARKER="# The preview site (port forwarding"
# The Caddy package's default Caddyfile, which is safe to replace (after a
# backup): it serves a placeholder page and nothing else.
CADDY_STOCK_MARKER="The Caddyfile is an easy way to configure your Caddy web server."

say() { printf '==> %s\n' "$*"; }
warn() { printf 'warning: %s\n' "$*" >&2; }
die() {
	printf 'error: %s\n' "$*" >&2
	exit 1
}

usage() {
	cat <<'EOF'
Install or upgrade Drydock. Run as root.

  --ui-host HOST        the hostname the UI is served at (required on first install)
  --cert PATH           its certificate, PEM          (required on first install)
  --key PATH            its private key, PEM          (required on first install)
  --preview-domain D    previews' separate registrable domain   } optional, all
  --preview-cert PATH   a wildcard certificate for *.D          } three or none
  --preview-key PATH    its private key                         }
  --no-preview          stop serving previews (removes the preview site)
  --github-app-id ID    the GitHub App's numeric App ID (not its Client ID)  } for the
  --github-app-key PATH its private key (.pem); copied to                  } repository
                        /etc/drydock/github-app.pem, mode 0400, owned by     } list; the
                        drydock. Give it again only to replace the key.      } ID is kept
  --version vX.Y.Z      install that release instead of the latest (download mode)
  --take-over-caddy     replace an existing /etc/caddy/Caddyfile this installer
                        did not write (it is backed up first)
  --no-password         do not prompt for the operator password
  -h, --help            this text

Settings from earlier runs are kept in /etc/drydock/drydock.env; flags override them.
EOF
}

# ---------------------------------------------------------------------------
# Download mode: fetch, verify, and re-run the bundled copy.
# ---------------------------------------------------------------------------

arch() {
	case "$(uname -m)" in
	x86_64 | amd64) echo amd64 ;;
	# Only amd64 is released. Anything else fails here, before downloading,
	# rather than on a 404 for a tarball that was never built.
	*) die "unsupported architecture $(uname -m); releases are built for amd64 (x86_64) only" ;;
	esac
}

download_and_reexec() {
	local version="$1"
	shift
	command -v curl >/dev/null || die "curl is required to download a release"
	command -v sha256sum >/dev/null || die "sha256sum is required to verify a release"

	local asset base tmp
	asset="drydock_linux_$(arch).tar.gz"
	if [ -n "${DRYDOCK_DOWNLOAD_BASE:-}" ]; then
		base="$DRYDOCK_DOWNLOAD_BASE" # a mirror, or the installer test's local server
	elif [ "$version" = latest ]; then
		base="https://github.com/$REPO/releases/latest/download"
	else
		base="https://github.com/$REPO/releases/download/$version"
	fi
	tmp=$(mktemp -d)
	# shellcheck disable=SC2064 # expand now: the variable is local
	trap "rm -rf '$tmp'" EXIT

	say "downloading $asset ($version)"
	curl -fsSL --retry 3 -o "$tmp/$asset" "$base/$asset" || die "could not download $base/$asset"
	curl -fsSL --retry 3 -o "$tmp/SHA256SUMS" "$base/SHA256SUMS" || die "could not download $base/SHA256SUMS"
	# This protects against a corrupt or truncated download, not a compromised
	# release: the checksum comes from the same place as the tarball.
	(cd "$tmp" && grep -E "  $asset\$" SHA256SUMS | sha256sum -c --quiet -) ||
		die "checksum mismatch for $asset; refusing to install it"
	tar -xzf "$tmp/$asset" -C "$tmp"
	[ -x "$tmp/drydock/install.sh" ] || die "the release tarball has no drydock/install.sh"
	# Not exec: the EXIT trap has to survive to clean up.
	bash "$tmp/drydock/install.sh" "$@"
}

# ---------------------------------------------------------------------------
# Bundle mode: the actual install.
# ---------------------------------------------------------------------------

# write_if_changed DEST MODE VAR: writes stdin to DEST atomically, and sets the
# named variable to 1 if the content differed. Unchanged files are left alone so
# a re-run restarts only what actually changed.
write_if_changed() {
	local dest="$1" mode="$2" flag="$3" tmp
	tmp=$(mktemp "$(dirname "$dest")/.drydock-install.XXXXXX")
	cat >"$tmp"
	if [ -f "$dest" ] && cmp -s "$tmp" "$dest"; then
		rm -f "$tmp"
		return 0
	fi
	chmod "$mode" "$tmp"
	mv -f "$tmp" "$dest"
	printf -v "$flag" 1
}

load_config() {
	# Only these keys are read, with simple KEY=value parsing — never `source`,
	# so the file can never run code as root.
	[ -f "$CONF" ] || return 0
	local key value
	while IFS='=' read -r key value; do
		case "$key" in
		DRYDOCK_UI_HOST) : "${UI_HOST:=$value}" ;;
		DRYDOCK_UI_CERT) : "${UI_CERT:=$value}" ;;
		DRYDOCK_UI_KEY) : "${UI_KEY:=$value}" ;;
		DRYDOCK_PREVIEW_DOMAIN) : "${PREVIEW_DOMAIN:=$value}" ;;
		DRYDOCK_PREVIEW_CERT) : "${PREVIEW_CERT:=$value}" ;;
		DRYDOCK_PREVIEW_KEY) : "${PREVIEW_KEY:=$value}" ;;
		DRYDOCK_GITHUB_APP_ID) : "${APP_ID:=$value}" ;;
		esac
	done <"$CONF"
	if [ "${NO_PREVIEW:-0}" = 1 ]; then
		PREVIEW_DOMAIN="" PREVIEW_CERT="" PREVIEW_KEY=""
	fi
}

validate_config() {
	if [ -z "${UI_HOST:-}" ] || [ -z "${UI_CERT:-}" ] || [ -z "${UI_KEY:-}" ]; then
		usage >&2
		die "a first install needs --ui-host, --cert and --key"
	fi
	[[ "$UI_HOST" =~ ^[A-Za-z0-9]([A-Za-z0-9.-]*[A-Za-z0-9])?$ ]] ||
		die "--ui-host must be a bare hostname such as drydock.example.com, not \"$UI_HOST\""
	[[ "$UI_HOST" == *.* ]] || die "--ui-host must be a fully qualified name; the Host check needs one (§13.1)"
	local f
	for f in "$UI_CERT" "$UI_KEY"; do
		[[ "$f" == /* ]] || die "certificate paths must be absolute: $f"
		[ -f "$f" ] || die "no such file: $f"
	done

	local n=0
	[ -n "${PREVIEW_DOMAIN:-}" ] && n=$((n + 1))
	[ -n "${PREVIEW_CERT:-}" ] && n=$((n + 1))
	[ -n "${PREVIEW_KEY:-}" ] && n=$((n + 1))
	case $n in
	0) ;;
	3)
		for f in "$PREVIEW_CERT" "$PREVIEW_KEY"; do
			[[ "$f" == /* ]] || die "certificate paths must be absolute: $f"
			[ -f "$f" ] || die "no such file: $f"
		done
		# The whole cross-site boundary rests on these being different
		# registrable domains (port forwarding §4); Drydock refuses this too,
		# but failing here leaves nothing half-installed.
		if [ "$PREVIEW_DOMAIN" = "$UI_HOST" ] || [[ "$PREVIEW_DOMAIN" == *".$UI_HOST" ]] || [[ "$UI_HOST" == *".$PREVIEW_DOMAIN" ]]; then
			die "--preview-domain must be a different registrable domain from --ui-host, not a parent or child of it"
		fi
		;;
	*) die "--preview-domain, --preview-cert and --preview-key go together: give all three or none" ;;
	esac

	if [ -n "${APP_ID:-}" ]; then
		[[ "$APP_ID" =~ ^[1-9][0-9]*$ ]] || die "--github-app-id must be the numeric App ID, not \"$APP_ID\" (the Client ID starts with Iv)"
		if [ -z "${APP_KEY_SRC:-}" ] && [ ! -f "$APP_KEY" ]; then
			die "--github-app-id needs --github-app-key the first time: there is no key at $APP_KEY yet"
		fi
	elif [ -n "${APP_KEY_SRC:-}" ]; then
		die "--github-app-key needs --github-app-id"
	fi
	if [ -n "${APP_KEY_SRC:-}" ]; then
		[ -f "$APP_KEY_SRC" ] || die "no such file: $APP_KEY_SRC"
		grep -q -- '-----BEGIN .*PRIVATE KEY-----' "$APP_KEY_SRC" ||
			die "$APP_KEY_SRC is not a PEM private key (download it from the App's settings page: Private keys → Generate)"
	fi
}

# install_app_key copies the App key into place, readable by drydock alone.
# Only when a new one was given: a re-run without --github-app-key keeps the
# key that is there.
install_app_key() {
	[ -n "${APP_KEY_SRC:-}" ] || return 0
	local tmp
	tmp=$(mktemp "$CONF_DIR/.github-app.XXXXXX")
	cat "$APP_KEY_SRC" >"$tmp"
	chown drydock:drydock "$tmp"
	chmod 0400 "$tmp"
	if [ -f "$APP_KEY" ] && cmp -s "$tmp" "$APP_KEY"; then
		rm -f "$tmp"
		return 0
	fi
	mv -f "$tmp" "$APP_KEY"
	KEY_CHANGED=1
	say "installed the GitHub App key at $APP_KEY (mode 0400, owner drydock)"
}

check_prerequisites() {
	[ "$(id -u)" = 0 ] || die "run as root (sudo)"
	[ "$(uname -s)" = Linux ] || die "Drydock runs on Linux"
	[ -d /run/systemd/system ] || die "systemd is required"
	command -v caddy >/dev/null ||
		die "Caddy is not installed. Install it from https://caddyserver.com/docs/install (the official package provides the caddy user and service), then re-run."
	id caddy >/dev/null 2>&1 || die "there is no 'caddy' user; install Caddy from its official package"
	command -v curl >/dev/null || die "curl is required (for the final end-to-end check)"
}

ensure_account() {
	if ! getent group drydock >/dev/null; then
		say "creating group drydock"
		groupadd --system drydock
	fi
	if ! id drydock >/dev/null 2>&1; then
		say "creating user drydock"
		useradd --system --gid drydock --no-create-home --home-dir /var/lib/drydock \
			--shell /usr/sbin/nologin drydock
	fi
}

# Caddy reads the key; check it can, as the caddy user, before anything is
# changed — a key Caddy cannot read fails at start with an error far from here.
check_caddy_can_read() {
	local f
	for f in "$@"; do
		runuser -u caddy -- test -r "$f" || die "the caddy user cannot read $f (fix its permissions, e.g. group caddy and mode 0640)"
	done
}

caddyfile_policy() {
	[ -f "$CADDYFILE" ] || return 0
	if grep -qF "$CADDYFILE_MARKER" "$CADDYFILE"; then
		return 0 # ours: an upgrade
	fi
	local backup
	backup="$CADDYFILE.before-drydock.$(date +%Y%m%d%H%M%S)"
	if grep -qF "$CADDY_STOCK_MARKER" "$CADDYFILE"; then
		say "replacing the Caddy package's default Caddyfile (saved to $backup)"
		cp -p "$CADDYFILE" "$backup"
		return 0
	fi
	if [ "${TAKE_OVER_CADDY:-0}" = 1 ]; then
		warn "taking over $CADDYFILE as asked; the previous file is saved to $backup"
		cp -p "$CADDYFILE" "$backup"
		return 0
	fi
	die "$CADDYFILE was not written by this installer. Drydock needs Caddy to itself: its Caddyfile carries a global options block, so it cannot be imported into another one. Move your sites elsewhere, or re-run with --take-over-caddy to replace it (a backup is kept)."
}

write_env_file() {
	install -d -m 0755 "$CONF_DIR"
	# Paths and names only — nothing secret, which is why it can be 0644 and
	# read by both services.
	write_if_changed "$CONF" 0644 ENV_CHANGED <<EOF
# Written by the Drydock installer; edit and re-run the installer to apply.
DRYDOCK_UI_HOST=$UI_HOST
DRYDOCK_UI_CERT=$UI_CERT
DRYDOCK_UI_KEY=$UI_KEY
DRYDOCK_PREVIEW_DOMAIN=${PREVIEW_DOMAIN:-}
DRYDOCK_PREVIEW_CERT=${PREVIEW_CERT:-}
DRYDOCK_PREVIEW_KEY=${PREVIEW_KEY:-}
DRYDOCK_GITHUB_APP_ID=${APP_ID:-}
EOF
}

write_unit() {
	# The App flags appear only when an App is configured: systemd cannot
	# drop an empty argument, and Drydock refuses an ID without a key rather
	# than ignoring one.
	local app_flags=""
	if [ -n "${APP_ID:-}" ]; then
		app_flags=" \\
  --github-app-id=\${DRYDOCK_GITHUB_APP_ID} \\
  --github-app-key=$APP_KEY"
	fi
	write_if_changed "$UNIT" 0644 UNIT_CHANGED <<EOF
# Written by the Drydock installer; re-running it overwrites this file.
[Unit]
Description=Drydock
Documentation=https://github.com/krelinga/drydock
After=network.target

[Service]
User=drydock
Group=drydock
EnvironmentFile=/etc/drydock/drydock.env
ExecStart=/usr/local/bin/drydock serve \\
  --ui-origin=https://\${DRYDOCK_UI_HOST} \\
  --ui-host=\${DRYDOCK_UI_HOST} \\
  --preview-domain=\${DRYDOCK_PREVIEW_DOMAIN} \\
  --socket-group=drydock$app_flags
# /run/drydock holds the sockets: group drydock, so Caddy (a supplementary
# member) can reach them and nothing else can. /var/lib/drydock holds the
# database, readable by Drydock alone.
RuntimeDirectory=drydock
RuntimeDirectoryMode=0750
StateDirectory=drydock
StateDirectoryMode=0700
Restart=on-failure
RestartSec=2
NoNewPrivileges=yes
ProtectSystem=strict
ProtectHome=yes
PrivateTmp=yes

[Install]
WantedBy=multi-user.target
EOF
}

write_caddy_dropin() {
	install -d -m 0755 "$(dirname "$CADDY_DROPIN")"
	write_if_changed "$CADDY_DROPIN" 0644 DROPIN_CHANGED <<'EOF'
# Written by the Drydock installer; re-running it overwrites this file.
[Service]
# The Caddyfile is all placeholders; `systemctl reload caddy` re-adapts it, so
# the values belong to the unit rather than to anyone's shell.
EnvironmentFile=/etc/drydock/drydock.env
# Caddy is the one other member of the socket's group (§13.1).
SupplementaryGroups=drydock
# For the 0600 admin socket the Caddyfile moves admin to, off localhost:2019.
RuntimeDirectory=caddy
RuntimeDirectoryMode=0700
EOF
}

# validate_caddy DIR: adapt the candidate Caddyfile staged in DIR, with the new
# settings, before anything under /etc/caddy changes. A mistake here then costs
# nothing: the running Caddy keeps its old, working config.
validate_caddy() {
	local stage="$1" out
	if ! out=$(env DRYDOCK_UI_HOST="$UI_HOST" DRYDOCK_UI_CERT="$UI_CERT" DRYDOCK_UI_KEY="$UI_KEY" \
		DRYDOCK_PREVIEW_DOMAIN="${PREVIEW_DOMAIN:-}" DRYDOCK_PREVIEW_CERT="${PREVIEW_CERT:-}" \
		DRYDOCK_PREVIEW_KEY="${PREVIEW_KEY:-}" DRYDOCK_CADDY_SITES="$stage/sites" \
		caddy validate --config "$stage/Caddyfile" --adapter caddyfile 2>&1); then
		printf '%s\n' "$out" | tail -5 >&2
		die "the new Caddy configuration does not validate; nothing under /etc/caddy was changed"
	fi
}

install_caddy_files() {
	local here="$1" stage
	stage=$(mktemp -d)
	mkdir "$stage/sites"
	cp "$here/Caddyfile" "$stage/Caddyfile"
	# The real Caddyfile imports $SITES_DIR; the staged copy imports the staged
	# sites directory through the same placeholder.
	if [ -n "${PREVIEW_DOMAIN:-}" ]; then
		cp "$here/preview.caddy" "$stage/sites/preview.caddy"
	fi
	validate_caddy "$stage"

	write_if_changed "$CADDYFILE" 0644 CADDYFILE_CHANGED <"$here/Caddyfile"
	install -d -m 0755 "$SITES_DIR"
	if [ -n "${PREVIEW_DOMAIN:-}" ]; then
		write_if_changed "$SITES_DIR/preview.caddy" 0644 CADDYFILE_CHANGED <"$here/preview.caddy"
	elif [ -f "$SITES_DIR/preview.caddy" ] && grep -qF "$PREVIEW_MARKER" "$SITES_DIR/preview.caddy"; then
		say "removing the preview site (no preview domain configured)"
		rm -f "$SITES_DIR/preview.caddy"
		CADDYFILE_CHANGED=1
	fi
	rm -rf "$stage"
}

install_binary() {
	local here="$1"
	NEW_VERSION=$("$here/drydock" version)
	OLD_VERSION=$([ -x "$BIN" ] && "$BIN" version 2>/dev/null || echo none)
	if [ -x "$BIN" ] && cmp -s "$here/drydock" "$BIN"; then
		return 0
	fi
	if [ -x "$BIN" ]; then
		cp -p "$BIN" "$BIN.previous"
	fi
	install -m 0755 "$here/drydock" "$BIN.new"
	mv -f "$BIN.new" "$BIN"
	BINARY_CHANGED=1
}

wait_for_drydock() {
	local i
	for i in $(seq 1 50); do
		if systemctl is-active --quiet drydock && [ -S "$API_SOCKET" ]; then
			return 0
		fi
		sleep 0.2
	done
	return 1
}

start_drydock() {
	systemctl enable --quiet drydock
	if [ "${BINARY_CHANGED:-0}" = 1 ] || [ "${UNIT_CHANGED:-0}" = 1 ] || [ "${ENV_CHANGED:-0}" = 1 ] || [ "${KEY_CHANGED:-0}" = 1 ] ||
		! systemctl is-active --quiet drydock; then
		say "starting drydock"
		systemctl restart drydock # SIGTERM first: a clean stop
	fi
	if wait_for_drydock; then
		return 0
	fi
	journalctl -u drydock -n 20 --no-pager >&2 || true
	if [ "${BINARY_CHANGED:-0}" = 1 ] && [ -x "$BIN.previous" ]; then
		warn "drydock $NEW_VERSION did not start; rolling back to $OLD_VERSION"
		mv -f "$BIN.previous" "$BIN"
		systemctl restart drydock
		wait_for_drydock && die "the upgrade failed and was rolled back to $OLD_VERSION; see the log above"
	fi
	die "drydock did not start; see the log above"
}

start_caddy() {
	systemctl enable --quiet caddy
	# A new supplementary group or runtime directory takes a restart; a reload
	# only re-reads the Caddyfile.
	if [ "${DROPIN_CHANGED:-0}" = 1 ] || ! systemctl is-active --quiet caddy; then
		say "restarting caddy"
		systemctl restart caddy
	elif [ "${CADDYFILE_CHANGED:-0}" = 1 ] || [ "${ENV_CHANGED:-0}" = 1 ]; then
		say "reloading caddy"
		systemctl reload caddy
	fi
	systemctl is-active --quiet caddy || {
		journalctl -u caddy -n 20 --no-pager >&2 || true
		die "caddy is not running; see the log above"
	}
}

set_password() {
	[ "${NO_PASSWORD:-0}" = 1 ] && return 0
	# Never stdin: under `curl | sudo bash`, stdin is the rest of this script.
	if (exec </dev/tty) 2>/dev/null; then
		runuser -u drydock -- "$BIN" passwd --db "$DB" --if-unset </dev/tty ||
			warn "the password was not set; run: sudo -u drydock $BIN passwd --db $DB"
	elif ! runuser -u drydock -- "$BIN" passwd --db "$DB" --if-unset </dev/null >/dev/null 2>&1; then
		warn "no terminal to ask on; set the operator password with: sudo -u drydock $BIN passwd --db $DB"
	fi
}

# The whole chain — Caddy, TLS, the socket, the gate — answering the way it
# should: an unauthenticated API request is refused with 401.
verify() {
	local code ca=()
	# For a private CA; a publicly trusted certificate needs nothing here.
	[ -n "${DRYDOCK_VERIFY_CACERT:-}" ] && ca=(--cacert "$DRYDOCK_VERIFY_CACERT")
	code=$(curl -s -o /dev/null -w '%{http_code}' --max-time 10 "${ca[@]}" \
		--resolve "$UI_HOST:443:127.0.0.1" "https://$UI_HOST/api/auth/session" || true)
	if [ "$code" != 401 ]; then
		die "end-to-end check failed: https://$UI_HOST/api/auth/session answered '$code' through Caddy, want 401. Check the certificate matches $UI_HOST and is trusted, and see: journalctl -u caddy -u drydock"
	fi
}

install_bundle() {
	local here="$1"
	check_prerequisites
	load_config
	validate_config
	check_caddy_can_read "$UI_CERT" "$UI_KEY"
	[ -n "${PREVIEW_DOMAIN:-}" ] && check_caddy_can_read "$PREVIEW_CERT" "$PREVIEW_KEY"
	caddyfile_policy

	ensure_account
	install_binary "$here"
	install_app_key
	write_env_file
	write_unit
	write_caddy_dropin
	install_caddy_files "$here"
	systemctl daemon-reload

	start_drydock
	start_caddy
	set_password
	verify

	if [ "$OLD_VERSION" = none ]; then
		say "installed Drydock $NEW_VERSION"
	elif [ "$OLD_VERSION" = "$NEW_VERSION" ]; then
		say "Drydock $NEW_VERSION is installed and current"
	else
		say "upgraded Drydock $OLD_VERSION -> $NEW_VERSION"
	fi
	say "open https://$UI_HOST"
}

main() {
	local version="" here
	while [ $# -gt 0 ]; do
		case "$1" in
		--ui-host) UI_HOST="${2:?--ui-host needs a value}"; shift 2 ;;
		--cert) UI_CERT="${2:?--cert needs a value}"; shift 2 ;;
		--key) UI_KEY="${2:?--key needs a value}"; shift 2 ;;
		--preview-domain) PREVIEW_DOMAIN="${2:?--preview-domain needs a value}"; shift 2 ;;
		--preview-cert) PREVIEW_CERT="${2:?--preview-cert needs a value}"; shift 2 ;;
		--preview-key) PREVIEW_KEY="${2:?--preview-key needs a value}"; shift 2 ;;
		--no-preview) NO_PREVIEW=1; shift ;;
		--github-app-id) APP_ID="${2:?--github-app-id needs a value}"; shift 2 ;;
		--github-app-key) APP_KEY_SRC="${2:?--github-app-key needs a value}"; shift 2 ;;
		--version) version="${2:?--version needs a value}"; shift 2 ;;
		--take-over-caddy) TAKE_OVER_CADDY=1; shift ;;
		--no-password) NO_PASSWORD=1; shift ;;
		-h | --help) usage; exit 0 ;;
		*) usage >&2; die "unknown option: $1" ;;
		esac
	done

	# Bundle mode when this script sits in an extracted release, beside the
	# binary it is meant to install. BASH_SOURCE is empty under `curl | bash`.
	here=""
	if [ -n "${BASH_SOURCE[0]:-}" ] && [ -f "${BASH_SOURCE[0]}" ]; then
		here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
	fi
	if [ -n "$here" ] && [ -x "$here/drydock" ] && [ -f "$here/Caddyfile" ]; then
		[ -z "$version" ] || warn "--version is ignored when installing from an extracted release"
		install_bundle "$here"
		return
	fi

	if [ -z "$version" ]; then
		version="$RELEASE_VERSION"
		[ "$version" = "__DRYDOCK_RELEASE_VERSION__" ] && version=latest
	fi
	# Re-pass only what was given; --version is consumed here.
	local args=()
	[ -n "${UI_HOST:-}" ] && args+=(--ui-host "$UI_HOST")
	[ -n "${UI_CERT:-}" ] && args+=(--cert "$UI_CERT")
	[ -n "${UI_KEY:-}" ] && args+=(--key "$UI_KEY")
	[ -n "${PREVIEW_DOMAIN:-}" ] && args+=(--preview-domain "$PREVIEW_DOMAIN")
	[ -n "${PREVIEW_CERT:-}" ] && args+=(--preview-cert "$PREVIEW_CERT")
	[ -n "${PREVIEW_KEY:-}" ] && args+=(--preview-key "$PREVIEW_KEY")
	[ "${NO_PREVIEW:-0}" = 1 ] && args+=(--no-preview)
	[ -n "${APP_ID:-}" ] && args+=(--github-app-id "$APP_ID")
	[ -n "${APP_KEY_SRC:-}" ] && args+=(--github-app-key "$APP_KEY_SRC")
	[ "${TAKE_OVER_CADDY:-0}" = 1 ] && args+=(--take-over-caddy)
	[ "${NO_PASSWORD:-0}" = 1 ] && args+=(--no-password)
	download_and_reexec "$version" "${args[@]}"
}

main "$@"
