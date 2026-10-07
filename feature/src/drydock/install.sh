#!/bin/sh
# The Drydock Feature's install (design §11). Runs as root at image build.
# Options arrive as environment variables named after them, uppercased.
set -eu

PREFIX=/usr/local/drydock
here=$(cd "$(dirname "$0")" && pwd)

BOTNAME=${BOTNAME:-}
BOTEMAIL=${BOTEMAIL:-}
BRANCHPREFIX=${BRANCHPREFIX-drydock/}
REQUIREBROKER=${REQUIREBROKER:-true}

# Claude Code, pinned (design §11, "Pin the version"; CLAUDE.md). Drydock
# scrapes Claude Code's terminal output in two places and every spike measured
# undocumented internals of one version — including the refresh lock the
# shared credential volume's safety rests on (Spike 00) — so a version is
# installed exactly, never "latest" or "stable", and DISABLE_AUTOUPDATER=1 in
# containerEnv keeps it there. The default is the version internal/classify
# was recorded against (classify.ClaudeCodeVersion; a Go test asserts the two
# agree), and Drydock passes that same constant as this option, so a Feature
# release can never move a running Drydock's Claude Code. Bump it only through
# the §11.1 ritual: re-run the four Claude Code harnesses, re-record the
# corpus, then change the default, the checksums below and the constant.
CLAUDE_PIN=2.1.289
# SHA-256 of each Linux build at CLAUDE_PIN, from Anthropic's release manifest
# (downloads.claude.ai/claude-code-releases/<version>/manifest.json) when it
# was pinned. Kept here as well as fetched, so the pinned version's bytes
# cannot change under the Feature even if the manifest does.
claude_pinned_sha256() {
	case $1 in
	linux-x64) echo a186b99e4a9c88366cd49df2f7dad56c61fc306ef0140b19ee64b7c42a8d1348 ;;
	linux-arm64) echo d100d5e41dcbee220c80d3a3099292e4b5a508b57cafd181856efedf29f84f28 ;;
	linux-x64-musl) echo a35123e65344b82ab10870f5842cac7e593e4a56b1f6bddaed96bd9b9d42953d ;;
	linux-arm64-musl) echo e9dfa4a0df412a1204c6a5efe3964c0e9f36f05fd86ebec1d9c84abf333e7d65 ;;
	esac
}
CLAUDECODEVERSION=${CLAUDECODEVERSION:-$CLAUDE_PIN}
CLAUDE_DOWNLOADS=https://downloads.claude.ai/claude-code-releases
# The Feature's CLAUDE_CONFIG_DIR (containerEnv), where Drydock mounts the
# shared credential volume (design §7.1).
CLAUDE_DIR=/home/vscode/.claude

say() { echo "drydock feature: $*"; }

# git; socat for the broker client (nc -U is the client's fallback); jq to
# merge .claude.json; curl and CA certificates to fetch gh and Claude Code.
install_packages() {
	if command -v apt-get >/dev/null 2>&1; then
		apt-get update -q
		DEBIAN_FRONTEND=noninteractive apt-get install -y -q --no-install-recommends "$@"
		rm -rf /var/lib/apt/lists/*
	elif command -v apk >/dev/null 2>&1; then
		apk add --no-cache "$@"
	elif command -v dnf >/dev/null 2>&1; then
		dnf install -y "$@"
	else
		echo "drydock feature: cannot install $* on this image: no apt-get, apk or dnf" >&2
		exit 1
	fi
}
need=""
command -v git >/dev/null 2>&1 || need="$need git"
command -v socat >/dev/null 2>&1 || need="$need socat"
command -v jq >/dev/null 2>&1 || need="$need jq"
command -v curl >/dev/null 2>&1 || need="$need curl"
[ -e /etc/ssl/certs/ca-certificates.crt ] || need="$need ca-certificates"
# Claude Code's musl build needs the C++ runtime Alpine leaves out.
if command -v apk >/dev/null 2>&1; then need="$need libgcc libstdc++"; fi
# shellcheck disable=SC2086 # word splitting is the point
[ -z "$need" ] || install_packages $need

# gh, the real one the shim execs (design §11). Installed here rather than
# through dependsOn on ghcr.io/devcontainers/features/github-cli: the dev
# container CLI writes an injected Feature's dependencies into the
# repository's devcontainer-lock.json, so a dependency would rewrite every
# lockfile a repository commits (design §6, "The repository's lockfile").
# A gh the image already has is kept; the shim finds it.
have_real_gh() {
	[ -x "$PREFIX/real/gh" ] && return 0
	old_ifs=$IFS
	IFS=:
	for d in $PATH; do
		[ -n "$d" ] && [ -x "$d/gh" ] || continue
		[ "$(readlink -f "$d/gh")" = "$PREFIX/bin/gh" ] && continue
		IFS=$old_ifs
		return 0
	done
	IFS=$old_ifs
	return 1
}
install_gh() {
	if command -v apt-get >/dev/null 2>&1; then
		# GitHub's own repository, as cli.github.com documents it.
		need=""
		command -v curl >/dev/null 2>&1 || need="$need curl"
		[ -e /etc/ssl/certs/ca-certificates.crt ] || need="$need ca-certificates"
		# shellcheck disable=SC2086 # word splitting is the point
		[ -z "$need" ] || install_packages $need
		mkdir -p -m 755 /etc/apt/keyrings
		curl -fsSL https://cli.github.com/packages/githubcli-archive-keyring.gpg \
			-o /etc/apt/keyrings/githubcli-archive-keyring.gpg
		chmod 0644 /etc/apt/keyrings/githubcli-archive-keyring.gpg
		echo "deb [arch=$(dpkg --print-architecture) signed-by=/etc/apt/keyrings/githubcli-archive-keyring.gpg] https://cli.github.com/packages stable main" \
			>/etc/apt/sources.list.d/github-cli.list
		install_packages gh
	elif command -v apk >/dev/null 2>&1; then
		# GitHub publishes no apk repository; Alpine packages gh in community.
		apk add --no-cache github-cli
	elif command -v dnf >/dev/null 2>&1; then
		command -v curl >/dev/null 2>&1 || dnf install -y curl
		curl -fsSL https://cli.github.com/packages/rpm/gh-cli.repo -o /etc/yum.repos.d/gh-cli.repo
		dnf install -y --repo gh-cli gh
	else
		echo "drydock feature: cannot install gh on this image: no apt-get, apk or dnf. gh is part of every Drydock workspace (design §11); use an image with one of them, or with gh already installed." >&2
		exit 1
	fi
	have_real_gh || {
		echo "drydock feature: gh did not install" >&2
		exit 1
	}
	gh_from=drydock
}
gh_from=image
have_real_gh || install_gh

# The clients, in their own directory and — because Debian's /etc/profile
# replaces PATH in login shells, and the supervisor's command is one (§10.3)
# — linked from /usr/local/bin, which precedes /usr/bin everywhere.
mkdir -p "$PREFIX/bin" "$PREFIX/hooks" "$PREFIX/etc" "$PREFIX/real"
for f in "$here"/bin/*; do
	install -m 0755 "$f" "$PREFIX/bin/"
done
if [ -e /usr/local/bin/gh ] && [ "$(readlink /usr/local/bin/gh || true)" != "$PREFIX/bin/gh" ]; then
	# A real gh where the shim must go: keep it, where the shim looks last.
	mv /usr/local/bin/gh "$PREFIX/real/gh"
fi
for t in drydock-broker drydock-credential drydock-probe drydock-secrets drydock-preflight drydock-claude-config gh; do
	ln -sf "$PREFIX/bin/$t" "/usr/local/bin/$t"
done

# Claude Code, at exactly CLAUDECODEVERSION, from Anthropic's own release
# downloads and checked against its SHA-256 — the artifacts and the check
# claude.ai/install.sh uses. Not that script itself: it always downloads and
# runs the *latest* binary to perform an install, and installs into the
# installing user's home (root's, here). This is installed system-wide and,
# like the clients, linked from both PATH directories, so a login shell finds
# it too.
install_claude() {
	v=$CLAUDECODEVERSION
	echo "$v" | grep -Eqx '[0-9]+\.[0-9]+\.[0-9]+' || {
		echo "drydock feature: claudeCodeVersion '$v' is not an exact version. Claude Code is pinned (design §11): give X.Y.Z, never latest or stable." >&2
		exit 1
	}
	case $(uname -m) in
	x86_64 | amd64) arch=x64 ;;
	aarch64 | arm64) arch=arm64 ;;
	*)
		echo "drydock feature: Claude Code has no build for $(uname -m)" >&2
		exit 1
		;;
	esac
	platform=linux-$arch
	if ls /lib/libc.musl-*.so.1 >/dev/null 2>&1; then platform=$platform-musl; fi

	manifest=$(curl -fsSL "$CLAUDE_DOWNLOADS/$v/manifest.json") || {
		echo "drydock feature: could not fetch Claude Code $v's release manifest; is $v a released version?" >&2
		exit 1
	}
	sha=$(printf '%s' "$manifest" | jq -r --arg p "$platform" '.platforms[$p].checksum // empty')
	echo "$sha" | grep -Eqx '[0-9a-f]{64}' || {
		echo "drydock feature: Claude Code $v's manifest has no checksum for $platform" >&2
		exit 1
	}
	if [ "$v" = "$CLAUDE_PIN" ]; then
		pinned=$(claude_pinned_sha256 "$platform")
		[ "$sha" = "$pinned" ] || {
			echo "drydock feature: Claude Code $v's manifest gives a different checksum for $platform than the one this Feature pinned ($pinned); refusing it." >&2
			exit 1
		}
	else
		say "Claude Code $v is not the version this Feature pins ($CLAUDE_PIN); checking it against Anthropic's manifest alone"
	fi

	dest=$PREFIX/claude/$v
	mkdir -p "$dest"
	curl -fsSL -o "$dest/claude.download" "$CLAUDE_DOWNLOADS/$v/$platform/claude"
	got=$(sha256sum "$dest/claude.download" | cut -d' ' -f1)
	[ "$got" = "$sha" ] || {
		rm -f "$dest/claude.download"
		echo "drydock feature: Claude Code $v for $platform failed its checksum: got $got, expected $sha" >&2
		exit 1
	}
	chmod 0755 "$dest/claude.download"
	mv "$dest/claude.download" "$dest/claude"
	if [ -e /usr/local/bin/claude ] && [ "$(readlink -f /usr/local/bin/claude)" != "$dest/claude" ]; then
		# The image's own Claude Code: kept, but never found first.
		mv /usr/local/bin/claude "$PREFIX/real/claude"
	fi
	ln -sf "$dest/claude" "$PREFIX/bin/claude"
	ln -sf "$dest/claude" /usr/local/bin/claude

	# Run it once, in a throwaway home, so a binary that does not run on this
	# image — or is not the version asked for — fails the build rather than
	# the first session.
	h=$(mktemp -d)
	out=$(env -i HOME="$h" PATH=/usr/local/bin:/usr/bin:/bin DISABLE_AUTOUPDATER=1 "$dest/claude" --version 2>&1) || true
	rm -rf "$h"
	case $out in
	"$v "*) ;;
	*)
		echo "drydock feature: the installed Claude Code reports '$out', not $v" >&2
		exit 1
		;;
	esac
}
install_claude

# The shared credential volume's mount point, made in the image, owned by
# the remote user, mode 0700. It no longer decides the volume's owner:
# Docker copies a directory's owner into a volume only while the volume is
# empty, and Drydock gives a new volume to its own uid and leaves a marker in
# it before any workspace mounts it (design §7.1) — because the CLI's UID
# update re-owns only the remote user's home, so for a remote user that is
# not vscode this directory kept the build-time uid, and the first such
# workspace gave a fresh volume to the wrong uid. It still matters where
# nothing has prepared the volume (this Feature's own tests).
# drydock-preflight refuses a container whose remote user does not own the
# volume.
remote_user=${_REMOTE_USER:-root}
mkdir -p "$CLAUDE_DIR"
chmod 0700 "$CLAUDE_DIR"
if id "$remote_user" >/dev/null 2>&1; then
	chown "$(id -u "$remote_user"):$(id -g "$remote_user")" "$CLAUDE_DIR"
fi

# CLAUDE_ENV_FILE's script (design §10.3, Spike 03): one constant line that
# delegates to the helper. Claude Code reads its text once per session and
# passes it as argv to every command, so it must never hold a value and never
# need rewriting — which is why it is a file shipped with the Feature rather
# than anything generated here.
install -m 0644 "$here/etc/claude-env.sh" "$PREFIX/etc/claude-env.sh"

# Hooks: a pre-push guard, and pass-throughs so core.hooksPath does not
# silence the repository's own hooks.
for h in applypatch-msg pre-applypatch post-applypatch pre-commit pre-merge-commit \
	prepare-commit-msg commit-msg post-commit pre-rebase post-checkout post-merge \
	pre-push pre-auto-gc post-rewrite sendemail-validate post-index-change \
	reference-transaction push-to-checkout; do
	ln -sf "$PREFIX/bin/drydock-hook" "$PREFIX/hooks/$h"
done

cat >"$PREFIX/etc/feature.env" <<CONF
BRANCH_PREFIX=$BRANCHPREFIX
REQUIRE_BROKER=$REQUIREBROKER
GH_INSTALLED_BY=$gh_from
CLAUDE_CODE_VERSION=$CLAUDECODEVERSION
CONF

# System-wide git config: the install does not know the remote user's home.
git config --system credential.https://github.com.helper "$PREFIX/bin/drydock-credential"
git config --system credential.https://github.com.useHttpPath false
git config --system core.hooksPath "$PREFIX/hooks"
[ -z "$BOTNAME" ] || git config --system user.name "$BOTNAME"
[ -z "$BOTEMAIL" ] || git config --system user.email "$BOTEMAIL"

say "installed (Claude Code $CLAUDECODEVERSION, branch prefix '${BRANCHPREFIX}', broker required: $REQUIREBROKER, gh from: $gh_from)"
