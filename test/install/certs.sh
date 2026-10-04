#!/usr/bin/env bash
# Runs inside the test container: a throwaway CA, a certificate for the UI
# host, and a wildcard for the preview domain, keys readable by group caddy —
# what an operator is told to provide.
set -euo pipefail
UI=drydock.test
PREVIEW=preview-drydock.example
mkdir -p /etc/ssl/drydock && cd /etc/ssl/drydock
openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:P-256 -nodes -days 2 \
	-subj /CN=test-ca -keyout ca.key -out ca.pem 2>/dev/null
leaf() { # leaf NAME SAN
	openssl req -newkey ec -pkeyopt ec_paramgen_curve:P-256 -nodes -subj "/CN=$1" \
		-keyout "$1.key" -out "$1.csr" 2>/dev/null
	openssl x509 -req -in "$1.csr" -CA ca.pem -CAkey ca.key -CAcreateserial -days 2 \
		-extfile <(printf 'subjectAltName=%s' "$2") -out "$1.pem" 2>/dev/null
}
leaf ui "DNS:$UI"
leaf preview "DNS:*.$PREVIEW"
chgrp caddy ./*.key && chmod 0640 ./*.key
