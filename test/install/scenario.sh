#!/usr/bin/env bash
# Runs inside the test container (see run.sh). Each scenario's negative check
# has its positive control in the scenario before or beside it: a refusal is
# only meaningful next to the same install succeeding.
set -uo pipefail

fails=0
pass() { printf 'ok   %s\n' "$*"; }
fail() { printf 'FAIL %s\n' "$*"; fails=$((fails + 1)); }
check() { # check DESCRIPTION COMMAND...
	local d="$1"; shift
	if "$@"; then pass "$d"; else fail "$d"; fi
}
section() { printf '\n--- %s\n' "$*"; }

UI=drydock.test
PREVIEW=preview-drydock.example
PW="correct horse battery staple"
export DRYDOCK_VERIFY_CACERT=/etc/ssl/drydock/ca.pem
CURL=(curl -s --cacert "$DRYDOCK_VERIFY_CACERT" --resolve "$UI:443:127.0.0.1" --max-time 5)

# install VERSION ARGS...: the README's one-liner, with the download pointed at
# a local copy of that release instead of GitHub. Output goes to $out.
install() {
	local v="$1"; shift
	out=$(cat "/releases/$v/install.sh" |
		DRYDOCK_DOWNLOAD_BASE="http://127.0.0.1:8000/$v" bash -s -- "$@" 2>&1)
	rc=$?
}
mainpid() { systemctl show -p MainPID --value "$1"; }
status() { "${CURL[@]}" -o /dev/null -w '%{http_code}' "$@"; }

section setup
cd /releases && python3 -m http.server 8000 --bind 127.0.0.1 >/dev/null 2>&1 &
sleep 0.5
check "the stock Caddyfile is in place" grep -q "easy way to configure" /etc/caddy/Caddyfile

section "a first install without its settings is refused"
install v0.0.1
check "it fails" [ "$rc" != 0 ]
check "it names the missing flags" grep -q -- "--ui-host, --cert and --key" <<<"$out"
check "it installed nothing" [ ! -e /usr/local/bin/drydock ]

section "first install"
install v0.0.1 --ui-host "$UI" --cert /etc/ssl/drydock/ui.pem --key /etc/ssl/drydock/ui.key
check "it succeeds (and its own end-to-end 401 check passed)" [ "$rc" = 0 ] || printf '%s\n' "$out"
check "it reports a fresh install" grep -q "installed Drydock v0.0.1" <<<"$out"
check "the binary is v0.0.1" [ "$(drydock version)" = v0.0.1 ]
check "with no terminal it says how to set the password" grep -q "drydock passwd" <<<"$out"
check "the stock Caddyfile was backed up" compgen -G "/etc/caddy/Caddyfile.before-drydock.*" >/dev/null
check "drydock runs as drydock" [ "$(ps -o user= -p "$(mainpid drydock)")" = drydock ]
check "the socket is group drydock, 0660" [ "$(stat -c '%G %a' /run/drydock/http.sock)" = "drydock 660" ]
check "the database is drydock's alone" [ "$(stat -c '%U %a' /var/lib/drydock)" = "drydock 700" ]
check "caddy's admin endpoint is not on loopback" [ "$(curl -s -o /dev/null -w '%{http_code}' http://127.0.0.1:2019/config/)" = 000 ]
check "an unauthenticated API call is 401" [ "$(status "https://$UI/api/repos")" = 401 ]
check "a foreign Host gets nothing" [ "$(curl -sk -o /dev/null -w '%{http_code}' --resolve other.test:443:127.0.0.1 https://other.test/)" != 200 ]

printf '%s\n' "$PW" | runuser -u drydock -- drydock passwd --db /var/lib/drydock/drydock.db >/dev/null
jar=$(mktemp)
code=$("${CURL[@]}" -o /dev/null -w '%{http_code}' -c "$jar" -H "Origin: https://$UI" \
	-H 'Content-Type: application/json' -d "{\"password\":\"$PW\"}" "https://$UI/api/auth/session")
check "sign-in through Caddy works (204)" [ "$code" = 204 ]
check "and the session cookie works" [ "$(status -b "$jar" "https://$UI/api/auth/session")" = 200 ]

section "re-run, nothing new"
pid_d=$(mainpid drydock) pid_c=$(mainpid caddy)
install v0.0.1
check "a re-run needs no flags" [ "$rc" = 0 ] || printf '%s\n' "$out"
check "it says it is current" grep -q "v0.0.1 is installed and current" <<<"$out"
check "drydock was not restarted" [ "$(mainpid drydock)" = "$pid_d" ]
check "caddy was not restarted" [ "$(mainpid caddy)" = "$pid_c" ]
# passwd would end every session, so a live one is the proof it was not run.
check "the password was left alone" [ "$(status -b "$jar" "https://$UI/api/auth/session")" = 200 ]

section "upgrade"
install v0.0.2
check "it succeeds" [ "$rc" = 0 ] || printf '%s\n' "$out"
check "it reports the move" grep -q "upgraded Drydock v0.0.1 -> v0.0.2" <<<"$out"
check "the binary is v0.0.2" [ "$(drydock version)" = v0.0.2 ]
check "drydock was restarted" [ "$(mainpid drydock)" != "$pid_d" ]
check "the session survived the upgrade" [ "$(status -b "$jar" "https://$UI/api/auth/session")" = 200 ]

section "an upgrade that cannot start is rolled back"
install v0.0.3
check "it fails" [ "$rc" != 0 ]
check "it says it rolled back" grep -q "rolled back to v0.0.2" <<<"$out"
check "the binary is v0.0.2 again" [ "$(drydock version)" = v0.0.2 ]
check "drydock is running" systemctl is-active --quiet drydock
check "the session still works" [ "$(status -b "$jar" "https://$UI/api/auth/session")" = 200 ]

section "previews on, then off"
check "control: no preview site yet" [ "$(status --resolve "a-b.$PREVIEW:443:127.0.0.1" "https://a-b.$PREVIEW/")" = 000 ]
install v0.0.2 --preview-domain "$PREVIEW" --preview-cert /etc/ssl/drydock/preview.pem --preview-key /etc/ssl/drydock/preview.key
check "enabling previews succeeds" [ "$rc" = 0 ] || printf '%s\n' "$out"
check "the preview site is installed" [ -f /etc/caddy/drydock.d/preview.caddy ]
check "a preview host now answers over TLS" [ "$(status --resolve "a-b.$PREVIEW:443:127.0.0.1" "https://a-b.$PREVIEW/")" != 000 ]
check "drydock was given the preview domain" grep -q -- "--preview-domain=$PREVIEW" "/proc/$(mainpid drydock)/cmdline" 2>/dev/null ||
	tr '\0' ' ' <"/proc/$(mainpid drydock)/cmdline" | grep -q -- "--preview-domain=$PREVIEW"
install v0.0.2
check "a re-run keeps previews" [ -f /etc/caddy/drydock.d/preview.caddy ]
install v0.0.2 --no-preview
check "--no-preview succeeds" [ "$rc" = 0 ] || printf '%s\n' "$out"
check "the preview site is gone" [ ! -e /etc/caddy/drydock.d/preview.caddy ]
check "a preview host no longer answers" [ "$(status --resolve "a-b.$PREVIEW:443:127.0.0.1" "https://a-b.$PREVIEW/")" = 000 ]
check "the UI still does" [ "$(status -b "$jar" "https://$UI/api/auth/session")" = 200 ]

section "the GitHub App"
check "control: without an App the repo list says so (503)" [ "$(status -b "$jar" "https://$UI/api/repos")" = 503 ]
openssl genrsa -traditional -out /root/app.pem 2048 2>/dev/null && chmod 0644 /root/app.pem # looser on purpose
install v0.0.2 --github-app-id Iv23liAbCdEf
check "a Client ID instead of the App ID is refused" [ "$rc" != 0 ] && grep -q "numeric App ID" <<<"$out"
install v0.0.2 --github-app-id 5189455
check "an App ID with no key yet is refused" [ "$rc" != 0 ] && grep -q "needs --github-app-key" <<<"$out"
pid_d=$(mainpid drydock)
install v0.0.2 --github-app-id 5189455 --github-app-key /root/app.pem
check "configuring the App succeeds" [ "$rc" = 0 ] || printf '%s\n' "$out"
check "the key is drydock's alone, mode 0400" [ "$(stat -c '%U %G %a' /etc/drydock/github-app.pem)" = "drydock drydock 400" ]
check "drydock was restarted onto it" [ "$(mainpid drydock)" != "$pid_d" ]
check "the repo list is now configured (200)" [ "$(status -b "$jar" "https://$UI/api/repos")" = 200 ]
check "the key is passed as a path" tr '\0' ' ' <"/proc/$(mainpid drydock)/cmdline" | grep -q -- "--github-app-key=/etc/drydock/github-app.pem"
keyline=$(sed -n 2p /root/app.pem)
check "the key is not in drydock's environment" bash -c "! tr '\\0' '\\n' </proc/$(mainpid drydock)/environ | grep -qF -- '$keyline'"
check "nor in its settings file" bash -c "! grep -qF -- '$keyline' /etc/drydock/drydock.env"
pid_d=$(mainpid drydock)
install v0.0.2
check "a re-run without the flags keeps the App" [ "$rc" = 0 ] && grep -q -- "--github-app-id" /etc/systemd/system/drydock.service
check "and restarts nothing" [ "$(mainpid drydock)" = "$pid_d" ]

section "a key caddy cannot read is refused before anything changes"
cp /etc/ssl/drydock/ui.key /root/private.key && chmod 0600 /root/private.key
before=$(sha256sum /etc/drydock/drydock.env)
install v0.0.2 --key /root/private.key
check "it fails" [ "$rc" != 0 ]
check "it says why" grep -q "caddy user cannot read" <<<"$out"
check "the settings are unchanged" [ "$(sha256sum /etc/drydock/drydock.env)" = "$before" ]

section "someone else's Caddyfile"
printf 'example.org {\n\trespond "not drydock"\n}\n' >/etc/caddy/Caddyfile
install v0.0.2
check "it is refused" [ "$rc" != 0 ]
check "it explains --take-over-caddy" grep -q -- "--take-over-caddy" <<<"$out"
check "the file is untouched" grep -q "not drydock" /etc/caddy/Caddyfile
install v0.0.2 --take-over-caddy
check "--take-over-caddy succeeds" [ "$rc" = 0 ] || printf '%s\n' "$out"
check "it is ours now" grep -q "Drydock's entire LAN-facing surface" /etc/caddy/Caddyfile
check "the foreign one was backed up" grep -lq "not drydock" /etc/caddy/Caddyfile.before-drydock.*

printf '\n%s\n' "$([ $fails = 0 ] && echo PASS || echo "$fails FAILED")"
[ $fails = 0 ]
