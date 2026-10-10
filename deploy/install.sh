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
# externally by design, §13.1), install Caddy, Docker or the devcontainer CLI,
# or touch the firewall.
#
# Everything is inside main(), called on the last line, so a download cut off
# part-way through executes nothing.

set -euo pipefail
# Bracket ranges such as [a-z] in the hostname checks below mean ASCII only in
# the C locale; in others they can match more than they say.
export LC_ALL=C

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
# The secrets master key (design §10.2): 32 raw bytes, created on the first
# install or supplied with --secrets-key. Every stored secret is sealed under
# it, so a different one makes them all unreadable: it is replaced only while
# no secret is stored. A file, never an environment variable.
SECRETS_KEY=$CONF_DIR/secrets.key
API_SOCKET=/run/drydock/http.sock
# Where clones live (/srv/drydock/ws/<id>/repo): Drydock's alone, mode 0700,
# because Caddy is in group drydock and has no business reading a clone.
WS_ROOT=/srv/drydock/ws
# The service's PATH, written into the unit, and the PATH every check below
# resolves docker and the devcontainer CLI on — so a CLI that only root's
# shell can find (nvm, a home directory) is refused here, not at the first
# clone.
SERVICE_PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin

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
  --ca-cert PATH        the CA certificate (PEM) that issued --cert, when it is a
                        private CA this host does not trust; only the installer's
                        final check uses it. Kept for later runs.
  --no-ca-cert          forget a --ca-cert given earlier
  --preview-domain D    previews' separate registrable domain   } optional, all
  --preview-cert PATH   a wildcard certificate for *.D          } three or none
  --preview-key PATH    its private key                         }
  --no-preview          stop serving previews (removes the preview site)
  --vscode-ssh-host USER@HOST
                        the SSH address your VS Code's Remote-SSH reaches this
                        server at ([user@]host[:port], or an ssh config alias):
                        each running workspace then has an Open in VS Code link.
                        That user needs Docker access here. Kept for later runs.
  --no-vscode-ssh-host  forget a --vscode-ssh-host given earlier (no link)
  --github-app-id ID    the GitHub App's numeric App ID (not its Client ID)  } for the
  --github-app-key PATH its private key (.pem); copied to                  } repository
                        /etc/drydock/github-app.pem, mode 0400, owned by     } list; the
                        drydock. Give it again only to replace the key.      } ID is kept
  --secrets-key PATH    the secrets master key, exactly 32 raw bytes (make one with
                        head -c 32 /dev/urandom > secrets.key); copied to
                        /etc/drydock/secrets.key, mode 0400, owned by drydock.
                        Without it the first install generates one. A different
                        key replaces the installed one only while no secret is
                        stored; otherwise it is refused and nothing changes.
  --version vX.Y.Z      install that release instead of the latest (download mode)
  --take-over-caddy     replace an existing /etc/caddy/Caddyfile this installer
                        did not write (it is backed up first)
  --no-password         do not prompt for the operator password
  -h, --help            this text

Settings from earlier runs are kept in /etc/drydock/drydock.env; flags override them.
Without --secrets-key the first install creates the secrets master key,
/etc/drydock/secrets.key, and later runs keep it. Back it up, or keep the file
you gave --secrets-key: without the key no stored secret can be read.
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
		DRYDOCK_CA_CERT) : "${CA_CERT:=$value}" ;;
		DRYDOCK_VSCODE_SSH_HOST) : "${VSCODE_SSH_HOST:=$value}" ;;
		esac
	done <"$CONF"
	if [ "${NO_VSCODE_SSH_HOST:-0}" = 1 ]; then
		VSCODE_SSH_HOST=""
	fi
	if [ "${NO_PREVIEW:-0}" = 1 ]; then
		PREVIEW_DOMAIN="" PREVIEW_CERT="" PREVIEW_KEY=""
	fi
	if [ "${NO_CA_CERT:-0}" = 1 ]; then
		CA_CERT=""
	fi
}

# lowercase NAME FLAG: lowercases the named variable in place, and says so if
# that changed it. Hostnames are case-insensitive to DNS, to certificate
# matching and to Caddy — but a browser sends the Origin header's host in
# lowercase, and Drydock compares Origin as an exact string, so a mixed-case
# --ui-host would refuse every sign-in. Lowercasing loses nothing anyone meant,
# and the one value goes to both Caddy and Drydock, so they cannot disagree. It
# also repairs a drydock.env written by an installer that did not lowercase
# (Drydock itself now refuses to start on one).
lowercase() {
	local name="$1" flag="$2" value lower
	value="${!name:-}"
	lower=$(printf '%s' "$value" | tr 'A-Z' 'a-z')
	if [ "$lower" != "$value" ]; then
		say "using $flag $lower: hostnames are case-insensitive, and browsers send them lowercased"
		printf -v "$name" '%s' "$lower"
	fi
}

validate_config() {
	local here="$1"
	if [ -z "${UI_HOST:-}" ] || [ -z "${UI_CERT:-}" ] || [ -z "${UI_KEY:-}" ]; then
		usage >&2
		die "a first install needs --ui-host, --cert and --key"
	fi
	lowercase UI_HOST --ui-host
	lowercase PREVIEW_DOMAIN --preview-domain
	[[ "$UI_HOST" =~ ^[a-z0-9]([a-z0-9.-]*[a-z0-9])?$ ]] ||
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
		# registrable domains (port forwarding §4): not equal, parent, child
		# or sibling. Drydock refuses this too, but failing here leaves
		# nothing half-installed. The bundle's own binary decides, by the
		# Public Suffix List `serve` uses, rather than a copy of it in shell.
		local why
		why=$("$here/drydock" check-preview-domain --ui-host "$UI_HOST" --preview-domain "$PREVIEW_DOMAIN" 2>&1) ||
			die "--preview-domain refused: ${why#drydock check-preview-domain: }"
		;;
	*) die "--preview-domain, --preview-cert and --preview-key go together: give all three or none" ;;
	esac

	if [ -n "${CA_CERT:-}" ]; then
		[[ "$CA_CERT" == /* ]] || die "--ca-cert must be an absolute path: $CA_CERT"
		[ -f "$CA_CERT" ] && [ -r "$CA_CERT" ] ||
			die "no such file: $CA_CERT (--ca-cert is kept from run to run; give its new path, or --no-ca-cert)"
		# Only the CA's certificate belongs here: its path is written to
		# drydock.env, which is world-readable, and nothing needs the key.
		! grep -q -- 'PRIVATE KEY-----' "$CA_CERT" ||
			die "$CA_CERT holds a private key; --ca-cert takes the CA's certificate, never its key"
		grep -q -- '-----BEGIN CERTIFICATE-----' "$CA_CERT" ||
			die "$CA_CERT is not a PEM certificate (--ca-cert takes the CA's certificate, the file that begins -----BEGIN CERTIFICATE-----)"
	fi

	# The Open in VS Code link's SSH address goes into a URL the operator's
	# VS Code opens, and serve refuses one it would not build. The bundle's own
	# binary decides, by the rule serve uses, so a value written to drydock.env
	# is one drydock starts with.
	if [ -n "${VSCODE_SSH_HOST:-}" ]; then
		local why_ssh
		why_ssh=$("$here/drydock" check-vscode-ssh-host --vscode-ssh-host "$VSCODE_SSH_HOST" 2>&1) ||
			die "--vscode-ssh-host refused: ${why_ssh#drydock check-vscode-ssh-host: }"
	fi

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
	if [ -n "${SECRETS_KEY_SRC:-}" ]; then
		[ -e "$SECRETS_KEY_SRC" ] || die "no such file: $SECRETS_KEY_SRC"
		[ -f "$SECRETS_KEY_SRC" ] || die "--secrets-key $SECRETS_KEY_SRC is not a regular file"
		check_secrets_key_size "$SECRETS_KEY_SRC" "--secrets-key $SECRETS_KEY_SRC"
	fi
}

# check_secrets_key_size FILE WHAT: a master key is exactly 32 raw bytes. Only
# the size is ever said — never a byte of the file.
check_secrets_key_size() {
	local size
	size=$(stat -L -c %s "$1")
	[ "$size" = 32 ] ||
		die "$2 is $size bytes; a secrets master key is exactly 32 raw bytes — not base64 or hex, and no trailing newline. Make one with: head -c 32 /dev/urandom > secrets.key"
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

# plan_secrets_key decides everything about the master key before anything is
# installed — the binary included — so a refusal leaves the host exactly as it
# was, running what it ran. With no key there yet, the key will be the one
# given with --secrets-key, or else 32 bytes of /dev/urandom. A key already
# there is kept — and one that is not a key is refused rather than replaced,
# because replacing it is how every stored secret is lost. That decision
# belongs to a person: restore the file from a backup, or move it aside
# knowingly. A different --secrets-key replaces it only while no secret is
# stored (plan_key_replacement). install_secrets_key carries the plan out.
plan_secrets_key() {
	KEY_ACTION=keep
	if [ -e "$SECRETS_KEY" ] || [ -L "$SECRETS_KEY" ]; then
		if [ ! -f "$SECRETS_KEY" ] || [ -L "$SECRETS_KEY" ] || [ "$(stat -c %s "$SECRETS_KEY")" != 32 ]; then
			die "$SECRETS_KEY is not a 32-byte key file. It is never replaced automatically: every stored secret is sealed under it, and a new key makes them all unreadable. Restore it from your backup — or, accepting that every stored secret is lost, move it aside and re-run."
		fi
		[ -z "${SECRETS_KEY_SRC:-}" ] || plan_key_replacement
		return 0
	fi
	if [ -n "${SECRETS_KEY_SRC:-}" ]; then
		stage_secrets_key
		KEY_ACTION=install
	else
		KEY_ACTION=create
	fi
}

# stage_secrets_key copies --secrets-key beside the installed key, drydock's
# and 0400, as $KEY_TMP. The size is checked again on the copy: the source was
# checked before anything changed, but it is the copy that is installed. A
# copy the run does not install is removed however it ends (on_exit).
stage_secrets_key() {
	KEY_TMP=$(mktemp "$CONF_DIR/.secrets-key.XXXXXX") # 0600, root's, until the chown
	STAGED+=("$KEY_TMP")
	cat "$SECRETS_KEY_SRC" >"$KEY_TMP"
	if [ "$(stat -c %s "$KEY_TMP")" != 32 ]; then
		check_secrets_key_size "$SECRETS_KEY_SRC" "--secrets-key $SECRETS_KEY_SRC"
		die "--secrets-key $SECRETS_KEY_SRC changed while it was being read"
	fi
	chown drydock:drydock "$KEY_TMP"
	chmod 0400 "$KEY_TMP"
}

# stored_secrets prints how many secrets the database holds. Asked by the
# binary this run installs — staged beside the installed one, not yet in its
# place — as drydock, read-only (`drydock count-secrets`): no sqlite3 on the
# host, no instance lock, no migration under a server that may be older, and
# anything it cannot read is a failure, never a zero.
stored_secrets() {
	local n
	n=$(runuser -u drydock -- "$ASK_BIN" count-secrets --db "$DB") || return 1
	[[ "$n" =~ ^[0-9]+$ ]] || return 1
	printf '%s\n' "$n"
}

# plan_key_replacement accepts a --secrets-key that differs from the installed
# key — but only while no secret is stored. Every stored secret is sealed under
# the installed key, and Drydock delivers none while any cannot be opened, so a
# swap with secrets stored breaks every workspace's secrets at once. Then it
# refuses and changes nothing; it never deletes or re-seals a secret. The
# question is asked twice: once with drydock running, so a refusal costs no
# downtime, and again with it stopped, so no secret can be written under the
# old key between the answer and the swap. From that stop on, drydock is
# started again however the run ends: by start_drydock, or by on_exit.
plan_key_replacement() {
	local n
	stage_secrets_key
	if cmp -s "$KEY_TMP" "$SECRETS_KEY"; then
		return 0 # the installed key, given again; on_exit removes the copy
	fi
	local unsure="--secrets-key $SECRETS_KEY_SRC is not the key installed at $SECRETS_KEY, and the installer could not tell whether any secret is stored under the installed one (see the error above). Nothing was changed."
	n=$(stored_secrets) || die "$unsure"
	if [ "$n" = 0 ] && systemctl is-active --quiet drydock; then
		say "stopping drydock to replace the secrets master key"
		DRYDOCK_STOPPED=1
		systemctl stop drydock ||
			die "could not stop drydock to replace the secrets master key (see the error above). Nothing was changed."
		n=$(stored_secrets) || n=unknown
	fi
	[ "$n" != unknown ] || die "$unsure"
	[ "$n" = 0 ] ||
		die "--secrets-key $SECRETS_KEY_SRC is not the key installed at $SECRETS_KEY, and $n stored secret(s) are sealed under the installed key. Replacing it would make every one of them unreadable, and Drydock delivers no secret while any cannot be read. The installed key was left as it is. To keep it, copy $SECRETS_KEY into your secret store and pass that copy as --secrets-key, or leave the flag off. To move to the new key, delete the stored secrets on Drydock's Secrets screen, re-run with --secrets-key, and enter them again."
	KEY_ACTION=replace
}

# install_secrets_key carries out plan_secrets_key's decision. The key is
# never printed, and goes straight into a file only drydock can read.
install_secrets_key() {
	local tmp
	case "$KEY_ACTION" in
	keep)
		# Kept as it is; only its ownership and mode are put right.
		chown drydock:drydock "$SECRETS_KEY"
		chmod 0400 "$SECRETS_KEY"
		;;
	install)
		mv -f "$KEY_TMP" "$SECRETS_KEY"
		KEY_CHANGED=1
		say "installed the secrets master key at $SECRETS_KEY from $SECRETS_KEY_SRC (mode 0400, owner drydock); keep your copy of it safe — without it no stored secret can be read"
		;;
	replace)
		mv -f "$KEY_TMP" "$SECRETS_KEY"
		KEY_CHANGED=1
		say "replaced the secrets master key at $SECRETS_KEY with $SECRETS_KEY_SRC (no secret was stored under the old one); keep your copy of it safe — without it no stored secret can be read"
		;;
	create)
		tmp=$(mktemp "$CONF_DIR/.secrets-key.XXXXXX") # 0600, root's, until the chown
		STAGED+=("$tmp")
		head -c 32 /dev/urandom >"$tmp"
		[ "$(stat -c %s "$tmp")" = 32 ] || die "could not read 32 bytes from /dev/urandom"
		chown drydock:drydock "$tmp"
		chmod 0400 "$tmp"
		mv -f "$tmp" "$SECRETS_KEY"
		KEY_CHANGED=1
		say "created the secrets master key at $SECRETS_KEY (mode 0400, owner drydock); back it up — without it no stored secret can be read"
		;;
	esac
}

check_prerequisites() {
	[ "$(id -u)" = 0 ] || die "run as root (sudo)"
	[ "$(uname -s)" = Linux ] || die "Drydock runs on Linux"
	[ -d /run/systemd/system ] || die "systemd is required"
	command -v caddy >/dev/null ||
		die "Caddy is not installed. Install it from https://caddyserver.com/docs/install (the official package provides the caddy user and service), then re-run."
	id caddy >/dev/null 2>&1 || die "there is no 'caddy' user; install Caddy from its official package"
	command -v curl >/dev/null || die "curl is required (for the final end-to-end check)"
	# Docker and the devcontainer CLI are how a workspace gets a container
	# (design §6). Neither is installed here: Docker is the host's to choose,
	# and both are long-lived dependencies an operator should own.
	PATH=$SERVICE_PATH command -v docker >/dev/null ||
		die "Docker is not installed. Install Docker Engine from https://docs.docker.com/engine/install/ (or your distribution's docker.io package), then re-run."
	getent group docker >/dev/null ||
		die "there is no 'docker' group; Docker's package creates it, and Drydock reaches the daemon through it"
	PATH=$SERVICE_PATH command -v devcontainer >/dev/null ||
		die "the devcontainer CLI is not installed where the service can find it ($SERVICE_PATH). Install Node.js 20 or later, then: npm install -g @devcontainers/cli — it must land in /usr/local/bin or /usr/bin, not in a home directory."
}

# The drydock user reaches Docker through the docker group, which is root on
# this host by another name: whoever can ask the daemon to run a container
# can mount / into it. Design §13.4 already counts the Drydock process as
# "everything, host included", for exactly this reason, so the group grants
# nothing the design has not already priced in — but it is the line that
# makes it true, so it is said here as well.
ensure_docker_access() {
	if ! id -nG drydock | tr ' ' '\n' | grep -qx docker; then
		say "adding drydock to the docker group (root-equivalent; see design §13.4)"
		usermod -aG docker drydock
		GROUPS_CHANGED=1
	fi
	# As the service will run it: as drydock, on the service's PATH. A CLI
	# that resolves but cannot start (a Node too old for it, say) fails here.
	runuser -u drydock -- env PATH="$SERVICE_PATH" HOME=/var/lib/drydock devcontainer --version >/dev/null 2>&1 ||
		die "the devcontainer CLI is installed but does not run as the drydock user; check that Node.js 20 or later is on $SERVICE_PATH"
	# The daemon may legitimately be down at install time; Drydock serves
	# without it and says so. So this one is a warning.
	runuser -u drydock -- env PATH="$SERVICE_PATH" docker info >/dev/null 2>&1 ||
		warn "the drydock user cannot reach the Docker daemon yet (is it running? systemctl enable --now docker). Workspaces will fail to start until it can."
}

# ensure_conf_dir makes /etc/drydock before anything writes into it: the App
# key, the secrets master key and drydock.env all land there, each through a
# temporary file beside it. Once, here, rather than in each writer — a writer
# that assumed another had run first is how a first install with
# --github-app-key failed in v0.2.0.
ensure_conf_dir() {
	install -d -m 0755 -o root -g root "$CONF_DIR"
}

ensure_workspace_root() {
	# /srv/drydock is made root's if it is not there, and left alone if it
	# is; the workspace root inside it is drydock's, 0700, always.
	[ -d "$(dirname "$WS_ROOT")" ] || install -d -m 0755 -o root -g root "$(dirname "$WS_ROOT")"
	install -d -m 0700 -o drydock -g drydock "$WS_ROOT"
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

# caddyfile_policy decides whether the Caddyfile may be replaced; the backup
# it calls for is made by install_caddy_files, once nothing can refuse.
caddyfile_policy() {
	CADDY_BACKUP=""
	[ -f "$CADDYFILE" ] || return 0
	if grep -qF "$CADDYFILE_MARKER" "$CADDYFILE"; then
		return 0 # ours: an upgrade
	fi
	if grep -qF "$CADDY_STOCK_MARKER" "$CADDYFILE"; then
		CADDY_BACKUP="$CADDYFILE.before-drydock.$(date +%Y%m%d%H%M%S)"
		CADDY_BACKUP_SAY="replacing the Caddy package's default Caddyfile (saved to $CADDY_BACKUP)"
		return 0
	fi
	if [ "${TAKE_OVER_CADDY:-0}" = 1 ]; then
		CADDY_BACKUP="$CADDYFILE.before-drydock.$(date +%Y%m%d%H%M%S)"
		CADDY_BACKUP_WARN="taking over $CADDYFILE as asked; the previous file is saved to $CADDY_BACKUP"
		return 0
	fi
	die "$CADDYFILE was not written by this installer. Drydock needs Caddy to itself: its Caddyfile carries a global options block, so it cannot be imported into another one. Move your sites elsewhere, or re-run with --take-over-caddy to replace it (a backup is kept)."
}

write_env_file() {
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
DRYDOCK_CA_CERT=${CA_CERT:-}
DRYDOCK_VSCODE_SSH_HOST=${VSCODE_SSH_HOST:-}
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
	# The unit it replaces is kept for a rollback (see start_drydock).
	rm -f "$UNIT.previous"
	[ -f "$UNIT" ] && cp -p "$UNIT" "$UNIT.previous"
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
  --vscode-ssh-host=\${DRYDOCK_VSCODE_SSH_HOST} \\
  --socket-group=drydock \\
  --secrets-key=$SECRETS_KEY$app_flags
# /run/drydock holds the sockets: group drydock, so Caddy (a supplementary
# member) can reach them and nothing else can. /var/lib/drydock holds the
# database, readable by Drydock alone.
RuntimeDirectory=drydock
RuntimeDirectoryMode=0750
# Kept across a stop or restart, until reboot: each running workspace's
# container bind-mounts its broker directory, /run/drydock/sock/<id>, and a
# bind mount pins that directory's inode. Removed and recreated with the
# service, it would leave every running container with no broker.
RuntimeDirectoryPreserve=yes
StateDirectory=drydock
StateDirectoryMode=0700
# The rest of the filesystem is read-only to Drydock (ProtectSystem=strict)
# except the clones. The per-workspace broker sockets are in
# /run/drydock/sock, inside the RuntimeDirectory, so they need no line here.
ReadWritePaths=$WS_ROOT
# Docker and the devcontainer CLI resolve on this, and only this; the
# installer checks both against the same value.
Environment=PATH=$SERVICE_PATH
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

# stage_caddy_files stages the candidate Caddy files and validates them, with
# the new settings, before anything is installed. install_caddy_files puts
# them in place.
stage_caddy_files() {
	local here="$1"
	CADDY_STAGE=$(mktemp -d)
	STAGED+=("$CADDY_STAGE")
	mkdir "$CADDY_STAGE/sites"
	cp "$here/Caddyfile" "$CADDY_STAGE/Caddyfile"
	# The real Caddyfile imports $SITES_DIR; the staged copy imports the staged
	# sites directory through the same placeholder.
	if [ -n "${PREVIEW_DOMAIN:-}" ]; then
		cp "$here/preview.caddy" "$CADDY_STAGE/sites/preview.caddy"
	fi
	validate_caddy "$CADDY_STAGE"
}

install_caddy_files() {
	if [ -n "$CADDY_BACKUP" ]; then
		[ -z "${CADDY_BACKUP_SAY:-}" ] || say "$CADDY_BACKUP_SAY"
		[ -z "${CADDY_BACKUP_WARN:-}" ] || warn "$CADDY_BACKUP_WARN"
		cp -p "$CADDYFILE" "$CADDY_BACKUP"
	fi
	write_if_changed "$CADDYFILE" 0644 CADDYFILE_CHANGED <"$CADDY_STAGE/Caddyfile"
	install -d -m 0755 "$SITES_DIR"
	if [ -n "${PREVIEW_DOMAIN:-}" ]; then
		write_if_changed "$SITES_DIR/preview.caddy" 0644 CADDYFILE_CHANGED <"$CADDY_STAGE/sites/preview.caddy"
	elif [ -f "$SITES_DIR/preview.caddy" ] && grep -qF "$PREVIEW_MARKER" "$SITES_DIR/preview.caddy"; then
		say "removing the preview site (no preview domain configured)"
		rm -f "$SITES_DIR/preview.caddy"
		CADDYFILE_CHANGED=1
	fi
}

# stage_binary puts the release's binary beside the installed one, as
# $BIN.new, without replacing anything: the key's count-secrets question is
# asked of it (as drydock, who can run a file there), and install_binary moves
# it into place once nothing can refuse. ASK_BIN is the binary this run will
# leave installed.
stage_binary() {
	local here="$1"
	NEW_VERSION=$("$here/drydock" version)
	OLD_VERSION=$([ -x "$BIN" ] && "$BIN" version 2>/dev/null || echo none)
	ASK_BIN=$BIN
	BIN_STAGED=0
	if [ -x "$BIN" ] && cmp -s "$here/drydock" "$BIN"; then
		return 0
	fi
	STAGED+=("$BIN.new")
	install -m 0755 "$here/drydock" "$BIN.new"
	ASK_BIN=$BIN.new
	BIN_STAGED=1
}

# running_elsewhere prints drydock's main PID when that process is running a
# binary other than the installed $BIN — the deleted file an earlier,
# interrupted run replaced — and fails when it is $BIN or nothing runs. The
# comparison is the inode the process executes against the file's, so a file
# replaced with the same name is still told apart.
running_elsewhere() {
	local pid
	pid=$(systemctl show -p MainPID --value drydock 2>/dev/null) || return 1
	[ -n "$pid" ] && [ "$pid" != 0 ] && [ -e "/proc/$pid/exe" ] || return 1
	[ ! "/proc/$pid/exe" -ef "$BIN" ] || return 1
	printf '%s\n' "$pid"
}

# plan_stale_process: when drydock runs a binary that is no longer the
# installed one, this run restarts it, says which version was really running,
# and keeps a copy of what was running for the rollback — that, not the file
# an interrupted run left, is the release known to start. Read before
# anything stops drydock, while the process is still there to read.
plan_stale_process() {
	local pid running
	RESTART_STALE=0
	pid=$(running_elsewhere) || return 0
	running=$("/proc/$pid/exe" version 2>/dev/null || echo unknown)
	say "drydock is running $running, not the installed $BIN ($OLD_VERSION): an earlier run did not finish; it will be restarted"
	OLD_VERSION=$running
	STAGED+=("$BIN.running")
	cp "/proc/$pid/exe" "$BIN.running"
	chmod 0755 "$BIN.running"
	RESTART_STALE=1
}

install_binary() {
	if [ "$RESTART_STALE" = 1 ]; then
		mv -f "$BIN.running" "$BIN.previous"
	elif [ "$BIN_STAGED" = 1 ] && [ -x "$BIN" ]; then
		cp -p "$BIN" "$BIN.previous"
	fi
	[ "$BIN_STAGED" = 1 ] || return 0
	mv -f "$BIN.new" "$BIN"
	BINARY_CHANGED=1
}

# on_exit runs however the bundle install ends. Whatever was staged and not
# installed is removed, and a drydock this run stopped and has not handed to
# start_drydock is started again — so neither a refusal nor a failure after
# the key's stop leaves the host down or a copy of a key lying in /etc/drydock.
on_exit() {
	local rc=$?
	trap - EXIT
	[ "${#STAGED[@]}" = 0 ] || rm -rf -- "${STAGED[@]}"
	if [ "$DRYDOCK_STOPPED" = 1 ]; then
		warn "starting drydock again, which this run stopped"
		systemctl daemon-reload || true
		systemctl start drydock || warn "drydock did not start; see: journalctl -u drydock"
	fi
	exit "$rc"
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
	# From here on starting drydock is this function's, rollback included.
	DRYDOCK_STOPPED=0
	systemctl enable --quiet drydock
	# A new supplementary group (docker) reaches the process only on a restart.
	if [ "${BINARY_CHANGED:-0}" = 1 ] || [ "${UNIT_CHANGED:-0}" = 1 ] || [ "${ENV_CHANGED:-0}" = 1 ] || [ "${KEY_CHANGED:-0}" = 1 ] ||
		[ "${GROUPS_CHANGED:-0}" = 1 ] || [ "$RESTART_STALE" = 1 ] || ! systemctl is-active --quiet drydock; then
		say "starting drydock"
		systemctl restart drydock # SIGTERM first: a clean stop
	fi
	if wait_for_drydock; then
		local pid
		# "installed and current" is said only of the process that runs the
		# installed file.
		if pid=$(running_elsewhere); then
			die "drydock is running $(readlink "/proc/$pid/exe"), not $BIN; restart it with: systemctl restart drydock"
		fi
		return 0
	fi
	journalctl -u drydock -n 20 --no-pager >&2 || true
	if { [ "${BINARY_CHANGED:-0}" = 1 ] || [ "$RESTART_STALE" = 1 ]; } && [ -x "$BIN.previous" ]; then
		warn "drydock $NEW_VERSION did not start; rolling back to $OLD_VERSION"
		mv -f "$BIN.previous" "$BIN"
		# The unit goes back too: a new one can pass a flag the old binary
		# does not know (--secrets-key did), and then the rollback would
		# not start either.
		if [ "${UNIT_CHANGED:-0}" = 1 ] && [ -f "$UNIT.previous" ]; then
			mv -f "$UNIT.previous" "$UNIT"
			systemctl daemon-reload
		fi
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
# should: an unauthenticated API request is refused with 401, over a TLS
# connection this host verified. Against --ca-cert alone when one was given (a
# private CA), otherwise against the system's trust store.
#
# With previews on, the same for the preview site (port forwarding §13): one
# name under the wildcard — drydock-check, which can never be a slug — which
# the preview socket answers with a redirect to its own denied page, over the
# wildcard certificate, verified the same way. A preview site Caddy serves with
# a certificate no device trusts is a preview no device can open.
verify() {
	verify_site "$UI_HOST" "https://$UI_HOST/api/auth/session" --cert 401
	if [ -n "${PREVIEW_DOMAIN:-}" ]; then
		verify_site "drydock-check.$PREVIEW_DOMAIN" "https://drydock-check.$PREVIEW_DOMAIN/" --preview-cert \
			"302 https://drydock-check.$PREVIEW_DOMAIN/.drydock/denied"
	fi
}

# verify_site HOST URL CERT_FLAG WANT: URL, resolved to this host, answers WANT
# over TLS this host verified — a status, or a status and the URL it redirects
# to. CERT_FLAG names the flag that gave the certificate.
verify_site() {
	local host="$1" url="$2" flag="$3" want="$4" fmt='%{http_code}' code rc=0 err reason trust="this host's trust store" ca=()
	case "$want" in *" "*) fmt='%{http_code} %{redirect_url}' ;; esac
	if [ -n "${CA_CERT:-}" ]; then
		ca=(--cacert "$CA_CERT")
		trust="$CA_CERT"
	fi
	err=$(mktemp)
	code=$(curl -sS -o /dev/null -w "$fmt" --max-time 10 "${ca[@]}" \
		--resolve "$host:443:127.0.0.1" "$url" 2>"$err") || rc=$?
	reason=$(tr '\n' ' ' <"$err" | sed 's/^curl: ([0-9]*) //; s/ *$//')
	rm -f "$err"
	code="${code% }" # no redirect: the status alone
	[ "$rc" = 0 ] && [ "$code" = "$want" ] && return 0

	# 60 is curl's "the certificate was not verified": an unknown CA, a name
	# the certificate does not carry, or a chain missing its intermediates.
	# The check has failed either way. A second request without verification
	# only decides which sentence to print — whether everything behind TLS
	# works, or something else is broken too. It never turns a failure into a
	# pass.
	if [ "$rc" = 60 ] && [ "$(curl -sk -o /dev/null -w "$fmt" --max-time 10 \
		--resolve "$host:443:127.0.0.1" "$url" 2>/dev/null)" = "$want" ]; then
		local hint="If the certificate is from a private CA, re-run with --ca-cert <the CA's certificate, PEM> (it is kept for later runs)."
		[ -n "${CA_CERT:-}" ] && hint="Check that $CA_CERT is the certificate of the CA that issued $flag (re-run with the right one, or --no-ca-cert for a publicly trusted certificate)."
		die "Drydock is installed and running, and answers through Caddy, but this host could not verify the certificate Caddy serves for $host against $trust: $reason. $hint Also check that its names include $host and that $flag is the full chain, leaf first, then the intermediates: a phone needs them too."
	fi
	die "end-to-end check failed: $url answered '$code' through Caddy, want '$want'${reason:+ ($reason)}. See: journalctl -u caddy -u drydock"
}

install_bundle() {
	local here="$1"
	STAGED=()
	DRYDOCK_STOPPED=0
	RESTART_STALE=0
	trap on_exit EXIT

	# Checks: each can refuse, and none changes anything.
	check_prerequisites
	load_config
	validate_config "$here"
	check_caddy_can_read "$UI_CERT" "$UI_KEY"
	[ -n "${PREVIEW_DOMAIN:-}" ] && check_caddy_can_read "$PREVIEW_CERT" "$PREVIEW_KEY"
	caddyfile_policy

	# The account, its Docker access and its directories: idempotent
	# groundwork, and what the staging below needs.
	ensure_account
	ensure_docker_access
	ensure_workspace_root
	ensure_conf_dir

	# Every decision that can still refuse, against staged copies: the Caddy
	# config, then the master key, which asks the staged binary. A refusal
	# here leaves the installed binary, its running process, the key and every
	# file as they were (and drydock running, if the key's check stopped it).
	stage_binary "$here"
	stage_caddy_files "$here"
	plan_stale_process
	plan_secrets_key

	# Only now is anything replaced.
	install_binary
	install_app_key
	install_secrets_key
	write_env_file
	write_unit
	write_caddy_dropin
	install_caddy_files
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
		--ca-cert) CA_CERT="${2:?--ca-cert needs a value}"; shift 2 ;;
		--no-ca-cert) NO_CA_CERT=1; shift ;;
		--vscode-ssh-host) VSCODE_SSH_HOST="${2:?--vscode-ssh-host needs a value}"; shift 2 ;;
		--no-vscode-ssh-host) NO_VSCODE_SSH_HOST=1; shift ;;
		--github-app-id) APP_ID="${2:?--github-app-id needs a value}"; shift 2 ;;
		--github-app-key) APP_KEY_SRC="${2:?--github-app-key needs a value}"; shift 2 ;;
		--secrets-key) SECRETS_KEY_SRC="${2:?--secrets-key needs a value}"; shift 2 ;;
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
	[ -n "${CA_CERT:-}" ] && args+=(--ca-cert "$CA_CERT")
	[ "${NO_CA_CERT:-0}" = 1 ] && args+=(--no-ca-cert)
	[ -n "${VSCODE_SSH_HOST:-}" ] && args+=(--vscode-ssh-host "$VSCODE_SSH_HOST")
	[ "${NO_VSCODE_SSH_HOST:-0}" = 1 ] && args+=(--no-vscode-ssh-host)
	[ -n "${APP_ID:-}" ] && args+=(--github-app-id "$APP_ID")
	[ -n "${APP_KEY_SRC:-}" ] && args+=(--github-app-key "$APP_KEY_SRC")
	[ -n "${SECRETS_KEY_SRC:-}" ] && args+=(--secrets-key "$SECRETS_KEY_SRC")
	[ "${TAKE_OVER_CADDY:-0}" = 1 ] && args+=(--take-over-caddy)
	[ "${NO_PASSWORD:-0}" = 1 ] && args+=(--no-password)
	download_and_reexec "$version" "${args[@]}"
}

main "$@"
