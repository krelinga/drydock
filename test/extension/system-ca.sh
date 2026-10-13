#!/usr/bin/env bash
# The extension-host tier's CA, in the system trust store (Spike 05 #1's
# fallback, vscode-extension §12.9). VS Code's extension host does not trust
# a CA named by NODE_EXTRA_CA_CERTS — measured: its fetch refuses the tier's
# certificate with UNABLE_TO_VERIFY_LEAF_SIGNATURE though the variable is in
# its environment — but it does read the system store when
# http.systemCertificates is on (VS Code's default). So the host tier's CA
# goes there, for the run, and nowhere else.
#
#   sudo test/extension/system-ca.sh install <dir>   # make the CA and leaves in <dir>, trust the CA
#   sudo test/extension/system-ca.sh remove <dir>    # untrust it (the CI runner is discarded anyway)
#
# CI runs it as root on a runner that lives for one job. run.sh never runs it:
# on a long-lived machine, editing the trust store is the operator's choice,
# made by running this by hand (and `remove` afterwards).
set -euo pipefail

cmd=${1:-}
dir=${2:-}
[ -n "$cmd" ] && [ -n "$dir" ] || { echo "usage: $0 install|remove <dir>" >&2; exit 2; }
[ "$(id -u)" = 0 ] || { echo "$0: run as root: it edits the system trust store" >&2; exit 1; }
name=drydock-extension-tier-ca.crt

case "$cmd" in
install)
  mkdir -p "$dir"
  cd "$dir"
  openssl req -x509 -newkey rsa:2048 -nodes -sha256 -days 2 -keyout ca.key -out ca.crt \
    -subj '/CN=Drydock extension-tier throwaway CA' \
    -addext 'basicConstraints=critical,CA:TRUE,pathlen:0' -addext 'keyUsage=critical,keyCertSign,cRLSign' 2>/dev/null
  leaf() {
    printf 'subjectAltName=%s\nbasicConstraints=critical,CA:FALSE\nkeyUsage=critical,digitalSignature,keyEncipherment\nextendedKeyUsage=serverAuth\n' "$2" >"$1.ext"
    openssl req -newkey rsa:2048 -nodes -sha256 -keyout "$1.key" -out "$1.csr" -subj "/CN=$1" 2>/dev/null
    openssl x509 -req -in "$1.csr" -CA ca.crt -CAkey ca.key -CAcreateserial -out "$1.crt" -days 2 -sha256 -extfile "$1.ext" 2>/dev/null
  }
  # The same leaves as test/extension/stack.ts makeTierCerts.
  leaf ui 'DNS:drydock.test'
  leaf preview 'DNS:*.drydock-preview.test,DNS:drydock-preview.test'
  leaf other 'DNS:other.test'
  # The tier's user reads the keys.
  [ -n "${SUDO_UID:-}" ] && chown -R "$SUDO_UID:${SUDO_GID:-$SUDO_UID}" "$dir"
  cp ca.crt "/usr/local/share/ca-certificates/$name"
  update-ca-certificates >/dev/null
  echo "trusted $dir/ca.crt system-wide as $name"
  ;;
remove)
  rm -f "/usr/local/share/ca-certificates/$name"
  update-ca-certificates --fresh >/dev/null
  echo "untrusted $name"
  ;;
*)
  echo "usage: $0 install|remove <dir>" >&2
  exit 2
  ;;
esac
