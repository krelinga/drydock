#!/usr/bin/env bash
# Builds the release assets: what the release workflow uploads, and what the
# installer test installs. No Node needed — the UI is committed, built, under
# internal/web/dist.
#
#   deploy/package.sh VERSION OUTDIR [ARCH...]     (default arches: amd64 arm64)
#
# OUTDIR gets, for each ARCH, drydock_linux_ARCH.tar.gz holding
#   drydock/{drydock,install.sh,Caddyfile,preview.caddy,VERSION}
# plus SHA256SUMS over the tarballs, and install.sh: the standalone installer,
# stamped with VERSION so it fetches the tarball it was released with.
set -euo pipefail

[ $# -ge 2 ] || { echo "usage: $0 VERSION OUTDIR [ARCH...]" >&2; exit 2; }
version="$1" out="$2"
shift 2
arches=("$@")
[ ${#arches[@]} -gt 0 ] || arches=(amd64 arm64)
[[ "$version" =~ ^v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?$ ]] || { echo "version must look like v1.2.3, not $version" >&2; exit 2; }

root=$(cd "$(dirname "$0")/.." && pwd)
mkdir -p "$out"
out=$(cd "$out" && pwd)
stage=$(mktemp -d)
trap 'rm -rf "$stage"' EXIT

for arch in "${arches[@]}"; do
	dir="$stage/$arch/drydock"
	mkdir -p "$dir"
	(cd "$root" && CGO_ENABLED=0 GOOS=linux GOARCH="$arch" \
		go build -trimpath -ldflags "-s -w -X main.version=$version" -o "$dir/drydock" ./cmd/drydock)
	install -m 0755 "$root/deploy/install.sh" "$dir/install.sh"
	install -m 0644 "$root/deploy/Caddyfile" "$root/deploy/preview.caddy" "$dir/"
	echo "$version" >"$dir/VERSION"
	# Reproducible: fixed owner and mtime, sorted entries.
	tar --sort=name --owner=0 --group=0 --numeric-owner --mtime='1970-01-01 00:00:00Z' \
		-C "$stage/$arch" -cf - drydock | gzip -n >"$out/drydock_linux_$arch.tar.gz"
done

(cd "$out" && sha256sum drydock_linux_*.tar.gz >SHA256SUMS)
sed "s/^RELEASE_VERSION=\"__DRYDOCK_RELEASE_VERSION__\"\$/RELEASE_VERSION=\"$version\"/" \
	"$root/deploy/install.sh" >"$out/install.sh"
grep -q "^RELEASE_VERSION=\"$version\"\$" "$out/install.sh" || { echo "failed to stamp install.sh" >&2; exit 1; }
chmod 0755 "$out/install.sh"
ls -l "$out"
