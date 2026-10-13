#!/usr/bin/env bash
# The VS Code extension's client tier (vscode-extension §12.4): the
# extension's client, stream and session code, on its production transport,
# against the real Drydock (./stack, a long-running go test) behind real
# Caddy on the shipped Caddyfile, with a throwaway CA trusted through
# NODE_EXTRA_CA_CERTS and certificate validation fully on.
#
#   test/extension/run.sh                 # the whole tier
#   test/extension/run.sh -t 'redirect'   # any Vitest arguments pass through
#
# Needs go, node (with `npm ci` done in extension/), caddy, openssl, git, and
# drydock.test resolving to 127.0.0.1: Node has no host-resolver flag, and
# nothing is added to the production client to fake one. This script never
# edits /etc/hosts itself; CI does, as root, before the tier runs.
# DRYDOCK_EXTENSION_KEEP=1 keeps the stack's temp root for a post-mortem.
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ext="$(cd "$here/../../extension" && pwd)"

missing=()
for c in go node caddy openssl git getent; do
  command -v "$c" >/dev/null || missing+=("$c")
done
[ -d "$ext/node_modules/vitest" ] || missing+=("extension/node_modules (run: cd extension && npm ci)")
if [ ${#missing[@]} -gt 0 ]; then
  printf 'extension client tier: missing %s\n' "${missing[@]}" >&2
  exit 1
fi
if ! getent ahostsv4 drydock.test | grep -q '^127\.0\.0\.1 '; then
  echo 'extension client tier: drydock.test must resolve to 127.0.0.1. Add this line to /etc/hosts:' >&2
  echo '  127.0.0.1 drydock.test' >&2
  exit 1
fi

cd "$ext"
exec npx vitest run --config vitest.client.config.mts "$@"
