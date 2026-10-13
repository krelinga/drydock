#!/usr/bin/env bash
# The VS Code extension's client tier (vscode-extension §12.4): the
# extension's client, stream and session code, on its production transport,
# against the real Drydock (./stack, a long-running go test) behind real
# Caddy on the shipped Caddyfile, with a throwaway CA trusted through
# NODE_EXTRA_CA_CERTS and certificate validation fully on.
#
#   test/extension/run.sh                 # the client tier
#   test/extension/run.sh -t 'redirect'   # any Vitest arguments pass through
#   test/extension/run.sh host            # the extension-host tier: the pinned
#                                         # VS Code (checked by SHA-256, cached in
#                                         # ~/.cache/drydock-vscode), under xvfb-run
#                                         # when there is no display
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
if [ "${1:-}" = host ]; then
  shift
  # VS Code's extension host trusts the system store, not NODE_EXTRA_CA_CERTS
  # (Spike 05 #1), so its CA must be made and trusted beforehand, by
  # system-ca.sh, as root: CI does; here it is the operator's choice.
  #
  # DRYDOCK_SPIKE_05_NEGATIVE=1 skips that, for Spike 05 #1's negative: the
  # tier's own CA, named by NODE_EXTRA_CA_CERTS alone, which the extension
  # host ignores — so the sign-in spec fails, and the wrong-host spec reports
  # UNABLE_TO_VERIFY_LEAF_SIGNATURE. The run is expected to fail.
  if [ "${DRYDOCK_SPIKE_05_NEGATIVE:-}" = 1 ]; then
    echo 'extension-host tier: Spike 05 #1 negative: NODE_EXTRA_CA_CERTS only, expected to fail' >&2
    unset DRYDOCK_EXTENSION_CERTS
  else
    certs=${DRYDOCK_EXTENSION_CERTS:-}
    if [ -z "$certs" ] || ! openssl verify -CApath /etc/ssl/certs "$certs/ui.crt" >/dev/null 2>&1; then
      echo 'extension-host tier: its CA must be in the system trust store, which this script never edits:' >&2
      echo '  dir=$(sudo test/extension/system-ca.sh install)' >&2
      echo '  DRYDOCK_EXTENSION_CERTS=$dir test/extension/run.sh host' >&2
      echo '  sudo test/extension/system-ca.sh remove "$dir"' >&2
      exit 1
    fi
    # A system-wide root is acceptable only name-constrained (system-ca.sh).
    if ! openssl x509 -in "$certs/ca.crt" -noout -ext nameConstraints 2>/dev/null | grep -q critical; then
      echo "extension-host tier: $certs/ca.crt is trusted system-wide without critical nameConstraints; remove it" >&2
      exit 1
    fi
    if [ -e "$certs/ca.key" ]; then
      echo "extension-host tier: $certs/ca.key still exists: a trusted CA's key must not outlive its leaves" >&2
      exit 1
    fi
  fi
  # The extension under test is the packaged .vsix, built now: the bytes a
  # release uploads.
  npm run --silent package
  if [ -z "${DISPLAY:-}" ]; then
    command -v xvfb-run >/dev/null || { echo 'extension-host tier: no DISPLAY and no xvfb-run' >&2; exit 1; }
    exec xvfb-run -a npx vitest run --config vitest.host.config.mts "$@"
  fi
  exec npx vitest run --config vitest.host.config.mts "$@"
fi
exec npx vitest run --config vitest.client.config.mts "$@"
