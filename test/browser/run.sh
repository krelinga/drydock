#!/usr/bin/env bash
# The browser tier (testing §10): real Chromium, real Caddy on the shipped
# Caddyfile, the real `drydock serve`, and a throwaway CA trusted through NSS.
#
#   test/browser/run.sh                 # the whole tier
#   test/browser/run.sh cookies.spec.ts # one file; any Playwright arguments pass through
#
# Needs go, node (with `npm ci` done in web/), caddy, openssl, certutil
# (libnss3-tools), and Playwright's Chromium:
#   (cd web && npx playwright install --with-deps chromium)
# DRYDOCK_BROWSER_KEEP=1 keeps the temp root for a post-mortem.
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
web="$(cd "$here/../../web" && pwd)"

missing=()
for c in go node caddy openssl certutil; do
  command -v "$c" >/dev/null || missing+=("$c")
done
[ -d "$web/node_modules/@playwright/test" ] || missing+=("web/node_modules (run: cd web && npm ci)")
if [ ${#missing[@]} -gt 0 ]; then
  printf 'browser tier: missing %s\n' "${missing[@]}" >&2
  exit 1
fi

# The specs live here and the toolchain lives in web/. Playwright compiles
# them to CommonJS, so NODE_PATH is enough for `@playwright/test` to resolve.
export NODE_PATH="$web/node_modules"
cd "$web"
# Playwright only strips types, so check them first.
npx tsc --noEmit -p "$here"
exec npx playwright test -c "$here" "$@"
