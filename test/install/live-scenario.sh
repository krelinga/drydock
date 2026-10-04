#!/usr/bin/env bash
# Runs inside the test container (see live.sh).
set -uo pipefail

fails=0
pass() { printf 'ok   %s\n' "$*"; }
fail() { printf 'FAIL %s\n' "$*"; fails=$((fails + 1)); }
check() { local d="$1"; shift; if "$@"; then pass "$d"; else fail "$d"; fi; }

UI=drydock.test
PW="correct horse battery staple"
export DRYDOCK_VERIFY_CACERT=/etc/ssl/drydock/ca.pem
CURL=(curl -s --cacert "$DRYDOCK_VERIFY_CACERT" --resolve "$UI:443:127.0.0.1" --max-time 5)
if [ "$RELEASE" = latest ]; then
	url="https://github.com/$REPO/releases/latest/download/install.sh"
else
	url="https://github.com/$REPO/releases/download/$RELEASE/install.sh"
fi
oneliner() { out=$(curl -fsSL "$url" | bash -s -- "$@" 2>&1); rc=$?; printf '%s\n' "$out" | sed 's/^/    /'; }

oneliner --ui-host "$UI" --cert /etc/ssl/drydock/ui.pem --key /etc/ssl/drydock/ui.key
check "the one-liner installs (its end-to-end 401 check included)" [ "$rc" = 0 ]
v=$(drydock version 2>/dev/null)
if [ "$RELEASE" = latest ]; then
	check "it installed a release build, not dev" [[ "$v" =~ ^v[0-9]+\.[0-9]+\.[0-9]+ ]]
else
	check "it installed $RELEASE" [ "$v" = "$RELEASE" ]
fi
check "drydock is running" systemctl is-active --quiet drydock
check "caddy is running" systemctl is-active --quiet caddy

oneliner
check "a re-run with no flags succeeds" [ "$rc" = 0 ]
check "and finds nothing to do" grep -q "is installed and current" <<<"$out"

printf '%s\n' "$PW" | runuser -u drydock -- drydock passwd --db /var/lib/drydock/drydock.db >/dev/null
code=$("${CURL[@]}" -o /dev/null -w '%{http_code}' -H "Origin: https://$UI" \
	-H 'Content-Type: application/json' -d "{\"password\":\"$PW\"}" "https://$UI/api/auth/session")
check "sign-in through Caddy works (204)" [ "$code" = 204 ]
check "the embedded UI is served" [ "$("${CURL[@]}" -o /dev/null -w '%{http_code}' "https://$UI/")" = 200 ]

printf '\n%s\n' "$([ $fails = 0 ] && echo PASS || echo "$fails FAILED")"
[ $fails = 0 ]
