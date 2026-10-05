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

say() { echo "drydock feature: $*"; }

# git, and socat for the broker client (nc -U is the client's fallback).
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
for t in drydock-broker drydock-credential drydock-probe drydock-secrets gh; do
	ln -sf "$PREFIX/bin/$t" "/usr/local/bin/$t"
done

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
CONF

# System-wide git config: the install does not know the remote user's home.
git config --system credential.https://github.com.helper "$PREFIX/bin/drydock-credential"
git config --system credential.https://github.com.useHttpPath false
git config --system core.hooksPath "$PREFIX/hooks"
[ -z "$BOTNAME" ] || git config --system user.name "$BOTNAME"
[ -z "$BOTEMAIL" ] || git config --system user.email "$BOTEMAIL"

say "installed (branch prefix '${BRANCHPREFIX}', broker required: $REQUIREBROKER, gh from: $gh_from)"
