#!/usr/bin/env bash
# Spike 04 — can a headless Chromium be made to trust a locally-generated CA
# cleanly enough for `__Host-` cookie semantics to be real?
#
# Usage: ./run.sh [nss|spki|ignore|none|all]
#
#   nss      trust the CA properly, via Chromium's NSS store (the recommendation)
#   spki     --ignore-certificate-errors-spki-list, no system state (fallback)
#   ignore   Playwright's ignoreHTTPSErrors (the option the testing plan rules out)
#   none     no trust at all — the control that proves the harness can fail
#   all      the whole matrix
#
# The CA is generated per run, trusted only for the duration of the run, and
# **removed from the trust store on exit**, including on Ctrl-C. A throwaway CA
# left permanently trusted in the operator's own browser store would be a real
# hole, so that cleanup is not optional and the harness owns it rather than
# leaving it to a README step.
set -uo pipefail

MODE="${1:-nss}"
S="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
RUN="${DRYDOCK_SPIKE_RUN:-$(mktemp -d -t drydock-spike-04-XXXXXX)}"
PORT="${PORT:-8443}"
# Playwright and its Chromium go outside the repo, so a run leaves no
# node_modules behind in a design-docs tree.
PW_DIR="${PW_DIR:-$HOME/.cache/drydock-spike-04}"
NSSDB="sql:$HOME/.pki/nssdb"
CA_NICK="drydock-spike-04-ca"
UI_HOST="${UI_HOST:-drydock.test}"
export UI_HOST
export PREVIEW_DOMAIN="${PREVIEW_DOMAIN:-drydock-preview.test}"

SRV_PID=""
cleanup() {
  [ -n "$SRV_PID" ] && kill "$SRV_PID" 2>/dev/null
  # Always, even if a mode never added it.
  certutil -d "$NSSDB" -D -n "$CA_NICK" 2>/dev/null
}
trap cleanup EXIT INT TERM

need() { command -v "$1" >/dev/null || { echo "missing: $1 ($2)"; exit 1; }; }
need openssl "apt-get install openssl"
need node "the devcontainer carries it"
need certutil "apt-get install libnss3-tools — needed for the nss mode"

# --- Playwright, out of tree -------------------------------------------------
if ! (cd "$PW_DIR" 2>/dev/null && node -e 'require.resolve("@playwright/test")' 2>/dev/null); then
  echo "installing Playwright + Chromium into $PW_DIR (~150 MB, once)"
  mkdir -p "$PW_DIR"
  (cd "$PW_DIR" && npm init -y >/dev/null 2>&1 &&
    npm i -D @playwright/test@latest >/dev/null 2>&1 &&
    npx playwright install --with-deps chromium >/dev/null 2>&1) || {
    echo "Playwright install failed; run it by hand in $PW_DIR"; exit 1; }
fi

echo "chromium: $(cd "$PW_DIR" && npx playwright --version)"
echo "artifacts: $RUN"
echo

"$S/mkcerts.sh" "$RUN" >/dev/null

one_mode() {
  local mode="$1"
  echo "=== $mode${WRONG_CERT:+ (wrong cert for $UI_HOST)} ==="

  # Trust only for this mode, and only if it asked for it.
  certutil -d "$NSSDB" -D -n "$CA_NICK" 2>/dev/null
  if [ "$mode" = nss ]; then
    mkdir -p "$HOME/.pki/nssdb"
    [ -f "$HOME/.pki/nssdb/cert9.db" ] ||
      certutil -d "$NSSDB" -N --empty-password
    certutil -d "$NSSDB" -A -t "C,," -n "$CA_NICK" -i "$RUN/ca.crt"
  fi

  node "$S/server.mjs" "$RUN" "$PORT" > "$RUN/server-$mode.log" 2>&1 &
  SRV_PID=$!
  for _ in $(seq 1 25); do
    grep -q listening "$RUN/server-$mode.log" 2>/dev/null && break
    sleep 0.2
  done

  # ESM resolves bare imports relative to the *script's* directory, not the cwd,
  # so the driver has to sit next to the node_modules it imports.
  cp "$S/spike.mjs" "$PW_DIR/spike.mjs"
  (cd "$PW_DIR" && node spike.mjs "$RUN" "$PORT" "$mode") \
    > "$RUN/result-$mode.json" 2> "$RUN/stderr-$mode.log"

  node -e '
    const r = require(process.argv[1]);
    let n = 0;
    for (const x of r.results) {
      if (x.pass) n++;
      console.log(` ${x.pass ? "PASS" : "FAIL"}  ${x.name}` +
                  (x.detail ? `  [${x.detail}]` : ""));
    }
    console.log(` -> ${n}/${r.results.length}`);
  ' "$RUN/result-$mode.json" 2>/dev/null ||
    { echo " harness error:"; tail -5 "$RUN/stderr-$mode.log" | sed 's/^/   /'; }

  kill "$SRV_PID" 2>/dev/null; SRV_PID=""
  certutil -d "$NSSDB" -D -n "$CA_NICK" 2>/dev/null
  echo
}

case "$MODE" in
  all) for m in none spki ignore nss; do one_mode "$m"; done ;;
  # Serve the UI host a certificate that is validly signed by the trusted CA
  # and wrong for the host. Only a route that does real validation should still
  # refuse it — which is what separates the three that otherwise tie.
  wrongcert)
    export WRONG_CERT=1
    for m in nss spki ignore; do one_mode "$m"; done
    unset WRONG_CERT ;;
  *)   one_mode "$MODE" ;;
esac

echo "the test CA has been removed from $NSSDB"
