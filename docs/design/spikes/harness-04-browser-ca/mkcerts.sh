#!/usr/bin/env bash
# Generate a throwaway CA and two leaf certificates for the browser tier:
# one for the UI host, one wildcard for the preview domain.
#
# Usage: ./mkcerts.sh <out-dir>
#
# The two names are deliberately different registrable domains. `.test` is
# reserved (RFC 6761) and absent from the Public Suffix List, so `drydock.test`
# and `drydock-preview.test` are distinct eTLD+1 — which is the whole property
# the cross-site boundary rests on. A subdomain of one shared parent would not
# be cross-site and the tier would silently prove nothing.
set -euo pipefail

OUT="${1:?usage: mkcerts.sh <out-dir>}"
UI_HOST="${UI_HOST:-drydock.test}"
PREVIEW_DOMAIN="${PREVIEW_DOMAIN:-drydock-preview.test}"

mkdir -p "$OUT"
cd "$OUT"

# --- CA -----------------------------------------------------------------------
openssl req -x509 -newkey rsa:2048 -nodes -sha256 -days 30 \
  -keyout ca.key -out ca.crt \
  -subj "/CN=Drydock spike throwaway CA" \
  -addext "basicConstraints=critical,CA:TRUE,pathlen:0" \
  -addext "keyUsage=critical,keyCertSign,cRLSign" 2>/dev/null

# --- leaf helper --------------------------------------------------------------
# A leaf needs subjectAltName; Chromium has ignored commonName since M58, so a
# cert with only a CN is rejected with ERR_CERT_COMMON_NAME_INVALID.
leaf() {
  local name="$1" san="$2"
  openssl req -newkey rsa:2048 -nodes -sha256 \
    -keyout "$name.key" -out "$name.csr" -subj "/CN=$name" 2>/dev/null
  openssl x509 -req -in "$name.csr" -CA ca.crt -CAkey ca.key -CAcreateserial \
    -out "$name.crt" -days 30 -sha256 \
    -extfile <(printf '%s\n' \
      "subjectAltName=$san" \
      "basicConstraints=critical,CA:FALSE" \
      "keyUsage=critical,digitalSignature,keyEncipherment" \
      "extendedKeyUsage=serverAuth") 2>/dev/null
  rm -f "$name.csr"
}

leaf "$UI_HOST"        "DNS:$UI_HOST"
# One wildcard covers every preview slug. Note a wildcard matches exactly one
# label, so `*.d-p.test` covers `abc123.d-p.test` and not `a.b.d-p.test`.
leaf "$PREVIEW_DOMAIN" "DNS:*.$PREVIEW_DOMAIN,DNS:$PREVIEW_DOMAIN"

printf '%s\n' "wrote CA and leaves to $OUT:"
for f in ca.crt "$UI_HOST.crt" "$PREVIEW_DOMAIN.crt"; do
  printf '  %-28s %s\n' "$f" \
    "$(openssl x509 -in "$f" -noout -ext subjectAltName 2>/dev/null | tail -1 | sed 's/^ *//' || echo 'CA')"
done
