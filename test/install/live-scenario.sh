#!/usr/bin/env bash
# Runs inside the test container (see live.sh).
set -uo pipefail

fails=0
pass() { printf 'ok   %s\n' "$*"; }
fail() { printf 'FAIL %s\n' "$*"; fails=$((fails + 1)); }
check() { local d="$1"; shift; if "$@"; then pass "$d"; else fail "$d"; fi; }

UI=drydock.test
PW="correct horse battery staple"
# The test CA (certs.sh), which this host does not trust: an operator's
# private CA.
CA=/etc/ssl/drydock/ca.pem
CURL=(curl -s --cacert "$CA" --resolve "$UI:443:127.0.0.1" --max-time 5)
if [ -n "${ASSETS:-}" ]; then
	# A release's assets on local disk (a draft's, before it is published),
	# served the way GitHub serves a published release's downloads.
	(cd "$ASSETS" && exec python3 -m http.server 8000 --bind 127.0.0.1 >/dev/null 2>&1) &
	sleep 0.5
	export DRYDOCK_DOWNLOAD_BASE="http://127.0.0.1:8000/$RELEASE"
	url="$DRYDOCK_DOWNLOAD_BASE/install.sh"
elif [ "$RELEASE" = latest ]; then
	url="https://github.com/$REPO/releases/latest/download/install.sh"
else
	url="https://github.com/$REPO/releases/download/$RELEASE/install.sh"
fi
oneliner() { out=$(curl -fsSL "$url" | bash -s -- "$@" 2>&1); rc=$?; printf '%s\n' "$out" | sed 's/^/    /'; }

# Releases before --ca-cert existed read the CA from a variable instead.
ca_args=(--ca-cert "$CA")
if ! curl -fsSL "$url" | grep -q -- '--ca-cert)'; then
	ca_args=()
	export DRYDOCK_VERIFY_CACERT="$CA"
fi

oneliner --ui-host "$UI" --cert /etc/ssl/drydock/ui.pem --key /etc/ssl/drydock/ui.key "${ca_args[@]}"
check "the one-liner installs (its end-to-end 401 check included)" [ "$rc" = 0 ]
v=$(drydock version 2>/dev/null)
if [ "$RELEASE" = latest ]; then
	# grep, not [[ ]]: check runs its arguments as a command, and [[ is syntax.
	check "it installed a release build, not dev ($v)" grep -Eq '^v[0-9]+\.[0-9]+\.[0-9]+' <<<"$v"
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

# The README's upgrade, for a draft's assets: every existing server runs the
# release that is Latest today, and gets this one by running the same line
# again. So install that release from GitHub, with a password and a stored
# secret, and upgrade it with this one's one-liner and no flags. (While this
# release is a draft, releases/latest is still the previous one.)
if [ -n "${ASSETS:-}" ]; then
	prev_url="https://github.com/$REPO/releases/latest/download/install.sh"
	prev=$(curl -fsSL "$prev_url" | sed -n 's/^RELEASE_VERSION="\(.*\)"$/\1/p')
	check "the release to upgrade from is published ($prev)" grep -Eq '^v[0-9]+\.[0-9]+\.[0-9]+$' <<<"$prev"
	if [ "$prev" = "$RELEASE" ]; then
		echo "    $RELEASE is already Latest: no previous release to upgrade from"
	elif [ -n "$prev" ]; then
		# Back to a host that never had Drydock (Caddy and its Caddyfile stay:
		# the installer's own, which the previous release takes as an upgrade).
		systemctl disable --now --quiet drydock
		rm -rf /etc/drydock /var/lib/drydock /usr/local/bin/drydock /usr/local/bin/drydock.previous \
			/etc/systemd/system/drydock.service /etc/systemd/system/drydock.service.previous
		systemctl daemon-reload
		prev_ca=(--ca-cert "$CA")
		curl -fsSL "$prev_url" | grep -q -- '--ca-cert)' || prev_ca=()
		out=$(curl -fsSL "$prev_url" | env -u DRYDOCK_DOWNLOAD_BASE DRYDOCK_VERIFY_CACERT="$CA" bash -s -- \
			--ui-host "$UI" --cert /etc/ssl/drydock/ui.pem --key /etc/ssl/drydock/ui.key "${prev_ca[@]}" 2>&1)
		rc=$?
		printf '%s\n' "$out" | sed 's/^/    /'
		check "the previous release, $prev, installs from GitHub" [ "$rc" = 0 ]
		check "control: it is $prev that runs" [ "$(drydock version)" = "$prev" ]
		printf '%s\n' "$PW" | runuser -u drydock -- drydock passwd --db /var/lib/drydock/drydock.db >/dev/null
		jar=$(mktemp)
		code=$("${CURL[@]}" -o /dev/null -w '%{http_code}' -c "$jar" -H "Origin: https://$UI" \
			-H 'Content-Type: application/json' -d "{\"password\":\"$PW\"}" "https://$UI/api/auth/session")
		check "sign-in on $prev works (204)" [ "$code" = 204 ]
		put() {
			"${CURL[@]}" -b "$jar" -X PUT -H "Origin: https://$UI" -H 'Content-Type: application/json' \
				-d '{"value":"live-upgrade-value","reach":"reads a scratch bucket"}' "https://$UI/api/secrets/LIVE_KEY"
		}
		check "a secret is stored on $prev" grep -q '"created":true' <<<"$(put)"

		oneliner
		check "the one-liner with no flags upgrades $prev to $RELEASE" [ "$rc" = 0 ]
		check "and says so" grep -q "upgraded Drydock $prev -> $RELEASE" <<<"$out"
		check "it is $RELEASE that runs" [ "$(drydock version)" = "$RELEASE" ]
		pid=$(systemctl show -p MainPID --value drydock)
		check "from the installed file" [ "/proc/$pid/exe" -ef /usr/local/bin/drydock ]
		check "the session from $prev survived the upgrade" \
			[ "$("${CURL[@]}" -o /dev/null -w '%{http_code}' -b "$jar" "https://$UI/api/auth/session")" = 200 ]
		# The same value again is not a rotation only if the stored one still
		# opens under the kept key, through the migrated database.
		check "the secret stored on $prev still decrypts" grep -q '"rotated":false' <<<"$(put)"
	fi
fi

printf '\n%s\n' "$([ $fails = 0 ] && echo PASS || echo "$fails FAILED")"
[ $fails = 0 ]
