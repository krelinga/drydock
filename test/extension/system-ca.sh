#!/usr/bin/env bash
# The extension-host tier's CA, in the system trust store (Spike 05 #1's
# fallback, vscode-extension §12.9). VS Code's extension host does not trust
# a CA named by NODE_EXTRA_CA_CERTS — measured: its fetch refuses the tier's
# certificate with UNABLE_TO_VERIFY_LEAF_SIGNATURE though the variable is in
# its environment — but it does read the system store when
# http.systemCertificates is on (VS Code's default). So the host tier's CA
# goes there, for the run, and nowhere else.
#
#   dir=$(sudo test/extension/system-ca.sh install)   # prints the new directory
#   DRYDOCK_EXTENSION_CERTS=$dir test/extension/run.sh host
#   sudo test/extension/system-ca.sh remove "$dir"    # untrust it and delete the directory
#
# What makes a system-wide root acceptable for the length of a run:
#   - It is name-constrained (critical): it can vouch only for the names the
#     tier serves — drydock.test, drydock-preview.test and its subdomains,
#     other.test — and for no IP address. A certificate it signed for any
#     other name fails validation everywhere that honours the extension,
#     which OpenSSL, Node and Chromium all do.
#   - Its private key is deleted as soon as the three leaves are signed, so
#     nothing can mint another certificate under it, constrained or not.
#   - The directory is made by mktemp, as root, 0700, and the CA is trusted
#     from it before it is handed to the invoking user (who needs the leaf
#     keys for Caddy): nothing can pre-create it, or swap ca.crt in before
#     root reads it.
#
# CI runs it on a runner that lives for one job. run.sh never runs it: on a
# long-lived machine, editing the trust store is the operator's choice.
set -euo pipefail

name=drydock-extension-tier-ca.crt
[ "$(id -u)" = 0 ] || { echo "$0: run as root: it edits the system trust store" >&2; exit 1; }

# The constraint, in OpenSSL's syntax. Keep it to exactly the names the
# tier's leaves carry (below, and test/extension/stack.ts makeTierCerts).
constraints='critical,permitted;DNS:drydock.test,permitted;DNS:drydock-preview.test,permitted;DNS:other.test,excluded;IP:0.0.0.0/0.0.0.0,excluded;IP:0:0:0:0:0:0:0:0/0:0:0:0:0:0:0:0'

case "${1:-}" in
install)
  dir=$(mktemp -d /tmp/drydock-host-ca.XXXXXXXX)
  chmod 0700 "$dir"
  cd "$dir"
  openssl req -x509 -newkey rsa:2048 -nodes -sha256 -days 2 -keyout ca.key -out ca.crt \
    -subj '/CN=Drydock extension-tier throwaway CA (name-constrained)' \
    -addext 'basicConstraints=critical,CA:TRUE,pathlen:0' -addext 'keyUsage=critical,keyCertSign,cRLSign' \
    -addext "nameConstraints=$constraints" 2>/dev/null
  leaf() {
    printf 'subjectAltName=%s\nbasicConstraints=critical,CA:FALSE\nkeyUsage=critical,digitalSignature,keyEncipherment\nextendedKeyUsage=serverAuth\n' "$2" >"$1.ext"
    openssl req -newkey rsa:2048 -nodes -sha256 -keyout "$1.key" -out "$1.csr" -subj "/CN=$1" 2>/dev/null
    openssl x509 -req -in "$1.csr" -CA ca.crt -CAkey ca.key -CAcreateserial -out "$1.crt" -days 2 -sha256 -extfile "$1.ext" 2>/dev/null
  }
  leaf ui 'DNS:drydock.test'
  leaf preview 'DNS:*.drydock-preview.test,DNS:drydock-preview.test'
  leaf other 'DNS:other.test'
  # Nothing signs anything under it again.
  shred -u ca.key 2>/dev/null || rm -f ca.key
  # Refuse to trust a CA without its constraint (a typo above would
  # otherwise install an unconstrained root).
  openssl x509 -in ca.crt -noout -ext nameConstraints | grep -q 'critical' || { echo "$0: the CA has no critical nameConstraints" >&2; exit 1; }
  touch .drydock-host-ca
  cp ca.crt "/usr/local/share/ca-certificates/$name"
  update-ca-certificates >/dev/null 2>&1
  # Only now is the directory the invoking user's: the leaf keys are Caddy's.
  [ -n "${SUDO_UID:-}" ] && chown -R "$SUDO_UID:${SUDO_GID:-$SUDO_UID}" "$dir"
  echo "$dir"
  ;;
remove)
  dir=${2:-}
  rm -f "/usr/local/share/ca-certificates/$name"
  update-ca-certificates --fresh >/dev/null 2>&1
  if [ -n "$dir" ]; then
    # Only a directory install made: refused unless, once canonicalised, it
    # is exactly mktemp's name under /tmp, a real directory (not a link),
    # owned by the user install handed it to, and carrying install's marker.
    not_ours() { echo "$0: $dir is not a directory install made: $1" >&2; exit 1; }
    [ -L "$dir" ] && not_ours 'a symbolic link'
    real=$(realpath -e -- "$dir" 2>/dev/null) || not_ours 'it does not exist'
    [[ "$real" =~ ^/tmp/drydock-host-ca\.[A-Za-z0-9]{8}$ ]] || not_ours "its real path is $real"
    [ -d "$real" ] && [ ! -L "$real" ] || not_ours 'not a directory'
    owner=$(stat -c %u -- "$real")
    want=${SUDO_UID:-0}
    [ "$owner" = "$want" ] || not_ours "it is owned by uid $owner, not $want"
    [ -f "$real/.drydock-host-ca" ] && [ ! -L "$real/.drydock-host-ca" ] || not_ours 'it has no marker'
    rm -rf -- "$real"
    dir=$real
  fi
  echo "untrusted $name${dir:+ and removed $dir}" >&2
  ;;
*)
  echo "usage: $0 install | remove <dir>" >&2
  exit 2
  ;;
esac
