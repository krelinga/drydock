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
# The test CA (certs.sh). This host does not trust it — exactly an operator's
# private CA — so the installer's own check passes only through --ca-cert.
CA=/etc/ssl/drydock/ca.pem
CURL=(curl -s --cacert "$CA" --resolve "$UI:443:127.0.0.1" --max-time 5)

# install VERSION ARGS...: the README's one-liner, with the download pointed at
# a local copy of that release instead of GitHub. Output goes to $out.
install() {
	local v="$1"; shift
	out=$(cat "/releases/$v/install.sh" |
		DRYDOCK_DOWNLOAD_BASE="http://127.0.0.1:8000/$v" bash -s -- "$@" 2>&1)
	rc=$?
	# Every word any run printed, for the sweeps that say what never was.
	printf '%s\n' "$out" >>/root/installer-output.log
}
# forget_drydock: back to a host that never had Drydock. Caddy's files are
# left, since they belong to the Caddy package and its Caddyfile is the
# installer's own.
forget_drydock() {
	systemctl disable --now --quiet drydock
	rm -rf /etc/drydock /var/lib/drydock /usr/local/bin/drydock /usr/local/bin/drydock.previous \
		/etc/systemd/system/drydock.service /etc/systemd/system/drydock.service.previous /etc/systemd/system/caddy.service.d/drydock.conf
	systemctl daemon-reload
}
mainpid() { systemctl show -p MainPID --value "$1"; }
status() { "${CURL[@]}" -o /dev/null -w '%{http_code}' "$@"; }

section setup
cd /releases && python3 -m http.server 8000 --bind 127.0.0.1 >/dev/null 2>&1 &
sleep 0.5
check "the stock Caddyfile is in place" grep -q "easy way to configure" /etc/caddy/Caddyfile

section "a host without Docker or the devcontainer CLI is refused"
mv /usr/local/bin/devcontainer /root/devcontainer.hidden
install v0.0.1 --ui-host "$UI" --cert /etc/ssl/drydock/ui.pem --key /etc/ssl/drydock/ui.key
check "without the devcontainer CLI it fails" [ "$rc" != 0 ]
check "it says how to install the CLI" grep -q "npm install -g @devcontainers/cli" <<<"$out"
check "it installed nothing" [ ! -e /usr/local/bin/drydock ]
mv /root/devcontainer.hidden /usr/local/bin/devcontainer
mv /usr/bin/docker /root/docker.hidden
install v0.0.1 --ui-host "$UI" --cert /etc/ssl/drydock/ui.pem --key /etc/ssl/drydock/ui.key
check "without Docker it fails" [ "$rc" != 0 ]
check "it says to install Docker" grep -q "Docker is not installed" <<<"$out"
check "it installed nothing" [ ! -e /usr/local/bin/drydock ]
mv /root/docker.hidden /usr/bin/docker
check "control: the daemon this test installs against is up" docker info

section "a first install without its settings is refused"
install v0.0.1
check "it fails" [ "$rc" != 0 ]
check "it names the missing flags" grep -q -- "--ui-host, --cert and --key" <<<"$out"
check "it installed nothing" [ ! -e /usr/local/bin/drydock ]

section "first install"
install v0.0.1 --ui-host "$UI" --cert /etc/ssl/drydock/ui.pem --key /etc/ssl/drydock/ui.key --ca-cert "$CA"
check "it succeeds (and its own end-to-end 401 check passed)" [ "$rc" = 0 ] || printf '%s\n' "$out"
check "it reports a fresh install" grep -q "installed Drydock v0.0.1" <<<"$out"
# The re-run below passes no flags and still passes its check: that is the
# proof this is read back, since this host does not trust the test CA.
check "the CA certificate's path is kept for later runs" grep -qx "DRYDOCK_CA_CERT=$CA" /etc/drydock/drydock.env
check "the binary is v0.0.1" [ "$(drydock version)" = v0.0.1 ]
check "with no terminal it says how to set the password" grep -q "drydock passwd" <<<"$out"
check "the stock Caddyfile was backed up" compgen -G "/etc/caddy/Caddyfile.before-drydock.*" >/dev/null
check "drydock runs as drydock" [ "$(ps -o user= -p "$(mainpid drydock)")" = drydock ]
check "the socket is group drydock, 0660" [ "$(stat -c '%G %a' /run/drydock/http.sock)" = "drydock 660" ]
check "the database is drydock's alone" [ "$(stat -c '%U %a' /var/lib/drydock)" = "drydock 700" ]
check "caddy's admin endpoint is not on loopback" [ "$(curl -s -o /dev/null -w '%{http_code}' http://127.0.0.1:2019/config/)" = 000 ]
check "an unauthenticated API call is 401" [ "$(status "https://$UI/api/repos")" = 401 ]
check "a foreign Host gets nothing" [ "$(curl -sk -o /dev/null -w '%{http_code}' --resolve other.test:443:127.0.0.1 https://other.test/)" != 200 ]

section "the service can build a workspace container"
# Checked inside the running service's own mount namespace, so ProtectSystem,
# PrivateTmp and ReadWritePaths are the unit's, not a copy of them — then as
# drydock, with the groups and PATH the service has.
SP=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin
in_service() { nsenter -t "$(mainpid drydock)" -m -- runuser -u drydock -- env PATH="$SP" HOME=/var/lib/drydock "$@"; }
check "drydock is in the docker group" bash -c "id -nG drydock | tr ' ' '\n' | grep -qx docker"
check "the workspace root is drydock's alone, 0700" [ "$(stat -c '%U %a' /srv/drydock/ws)" = "drydock 700" ]
check "the service can write the workspace root" in_service touch /srv/drydock/ws/.probe
mkdir -p /opt/drydock-probe && chown drydock:drydock /opt/drydock-probe # not install(1): install() is the scenario's own
check "control: drydock can write its own directory outside the service" runuser -u drydock -- touch /opt/drydock-probe/x
check "but the service cannot: ProtectSystem=strict is in force" bash -c "! nsenter -t $(mainpid drydock) -m -- runuser -u drydock -- touch /opt/drydock-probe/y 2>/dev/null"
rm -rf /opt/drydock-probe
check "the service reaches the Docker daemon" in_service docker info
mkdir -p /srv/drydock/ws/probe/repo /srv/drydock/ws/probe/.drydock
echo '{"image":"debian:bookworm-slim"}' >/srv/drydock/ws/probe/.drydock/devcontainer.json
chown -R drydock:drydock /srv/drydock/ws/probe
upout=$(in_service devcontainer up --workspace-folder /srv/drydock/ws/probe/repo --no-lockfile \
	--id-label drydock.installtest.workspace=probe --override-config /srv/drydock/ws/probe/.drydock/devcontainer.json 2>/dev/null)
check "devcontainer up works as the service, a repository with no config and all" grep -q '"outcome":"success"' <<<"$upout" ||
	printf '%s\n' "$upout"
check "and devcontainer exec into it" in_service devcontainer exec --workspace-folder /srv/drydock/ws/probe/repo \
	--id-label drydock.installtest.workspace=probe --override-config /srv/drydock/ws/probe/.drydock/devcontainer.json -- true
docker ps -aq --filter label=drydock.installtest.workspace | xargs -r docker rm -f >/dev/null
rm -rf /srv/drydock/ws/probe /srv/drydock/ws/.probe
check "the boot reconciliation reached Docker too" bash -c "! journalctl -u drydock -o cat | grep -q 'drydock: reconcile'"
check "control: that is the service's journal" bash -c "journalctl -u drydock -o cat | grep -q 'serving on'"

printf '%s\n' "$PW" | runuser -u drydock -- drydock passwd --db /var/lib/drydock/drydock.db >/dev/null
jar=$(mktemp)
code=$("${CURL[@]}" -o /dev/null -w '%{http_code}' -c "$jar" -H "Origin: https://$UI" \
	-H 'Content-Type: application/json' -d "{\"password\":\"$PW\"}" "https://$UI/api/auth/session")
check "sign-in through Caddy works (204)" [ "$code" = 204 ]
check "and the session cookie works" [ "$(status -b "$jar" "https://$UI/api/auth/session")" = 200 ]

section "the secrets master key"
SK=/etc/drydock/secrets.key
check "it was created, drydock's alone, mode 0400" [ "$(stat -c '%U %G %a %s' "$SK")" = "drydock drydock 400 32" ]
check "the installer said to back it up" grep -q "back it up" <<<"$out"
skb64=$(base64 -w0 "$SK") skhex=$(od -An -tx1 "$SK" | tr -d ' \n')
check "it was never printed" bash -c "! grep -qF -- '$skb64' <<<\"\$1\" && ! grep -qF -- '$skhex' <<<\"\$1\"" _ "$out"
check "drydock is given it as a path" bash -c "tr '\\0' ' ' </proc/$(mainpid drydock)/cmdline | grep -q -- '--secrets-key=$SK'"
check "it is not in drydock's environment" bash -c "! base64 -w0 /proc/$(mainpid drydock)/environ | grep -qF -- '$skb64' && ! grep -qF -- '$skb64' /etc/drydock/drydock.env"
# put NAME VALUE: PUT a secret through Caddy; prints the response body.
put() {
	"${CURL[@]}" -b "$jar" -X PUT -H "Origin: https://$UI" -H 'Content-Type: application/json' \
		-d "{\"value\":\"$2\",\"reach\":\"reads a scratch bucket\"}" "https://$UI/api/secrets/$1"
}
check "a secret can be stored through Caddy" grep -q '"created":true' <<<"$(put TEST_KEY install-test-value)"
check "the list never carries the value" bash -c "! grep -q install-test-value <<<\"\$(\"\$@\")\"" _ "${CURL[@]}" -b "$jar" "https://$UI/api/secrets"
check "a reserved name is refused" grep -q secret_name_reserved <<<"$(put GH_TOKEN x)"
key_sum=$(sha256sum "$SK")

section "re-run, nothing new"
pid_d=$(mainpid drydock) pid_c=$(mainpid caddy)
install v0.0.1
check "a re-run needs no flags" [ "$rc" = 0 ] || printf '%s\n' "$out"
check "it says it is current" grep -q "v0.0.1 is installed and current" <<<"$out"
check "drydock was not restarted" [ "$(mainpid drydock)" = "$pid_d" ]
check "caddy was not restarted" [ "$(mainpid caddy)" = "$pid_c" ]
# passwd would end every session, so a live one is the proof it was not run.
check "the password was left alone" [ "$(status -b "$jar" "https://$UI/api/auth/session")" = 200 ]
check "the master key was kept" [ "$(sha256sum "$SK")" = "$key_sum" ]

section "upgrade"
install v0.0.2
check "it succeeds" [ "$rc" = 0 ] || printf '%s\n' "$out"
check "it reports the move" grep -q "upgraded Drydock v0.0.1 -> v0.0.2" <<<"$out"
check "the binary is v0.0.2" [ "$(drydock version)" = v0.0.2 ]
check "drydock was restarted" [ "$(mainpid drydock)" != "$pid_d" ]
check "the session survived the upgrade" [ "$(status -b "$jar" "https://$UI/api/auth/session")" = 200 ]
check "the master key survived it" [ "$(sha256sum "$SK")" = "$key_sum" ]
# The same value again is not a rotation only if the stored one still opens:
# under a different key it would not, and the PUT would report a rotation.
check "and the stored secret still decrypts under it" grep -q '"rotated":false' <<<"$(put TEST_KEY install-test-value)"

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

section "a private CA this host does not trust"
check "control: this host's own trust store does not trust the test CA" \
	[ "$(curl -s -o /dev/null -w '%{http_code}' --resolve "$UI:443:127.0.0.1" "https://$UI/api/auth/session")" = 000 ]
install v0.0.2 --no-ca-cert
check "without --ca-cert the final check fails" [ "$rc" != 0 ]
check "it says everything is running, and that only the certificate is unverified" \
	grep -q "Drydock is installed and running, and answers through Caddy, but this host could not verify" <<<"$out"
check "it gives curl's reason" grep -q "unable to get local issuer certificate" <<<"$out"
check "it says to pass --ca-cert" grep -q -- "re-run with --ca-cert" <<<"$out"
check "and it is right: drydock is running" systemctl is-active --quiet drydock
check "and the session answers, to a client that trusts the CA" [ "$(status -b "$jar" "https://$UI/api/auth/session")" = 200 ]
check "--no-ca-cert forgot the CA" grep -qx "DRYDOCK_CA_CERT=" /etc/drydock/drydock.env
install v0.0.2 --ca-cert /etc/ssl/drydock/ui.key
check "a private key is refused as --ca-cert" [ "$rc" != 0 ]
check "and it says so" grep -q "never its key" <<<"$out"
install v0.0.2 --ca-cert /etc/ssl/drydock/../drydock/missing.pem
check "a missing --ca-cert is refused" [ "$rc" != 0 ]
check "and it says so" grep -q "no such file" <<<"$out"
install v0.0.2 --ca-cert ca.pem
check "a relative --ca-cert is refused" [ "$rc" != 0 ]
check "and it says so" grep -q -- "--ca-cert must be an absolute path" <<<"$out"
check "the refusals left the setting alone" grep -qx "DRYDOCK_CA_CERT=" /etc/drydock/drydock.env
install v0.0.2 --ca-cert "$CA"
check "control: with --ca-cert the same install passes its check" [ "$rc" = 0 ] || printf '%s\n' "$out"
check "and keeps it" grep -qx "DRYDOCK_CA_CERT=$CA" /etc/drydock/drydock.env

# A trusted certificate in front of a broken chain must still fail the check,
# and must not be reported as only a trust problem. Caddy loses the drydock
# group, so it cannot reach the socket: a 502 behind good TLS.
printf '[Service]\nSupplementaryGroups=\n' >/etc/systemd/system/caddy.service.d/zz-test-break.conf
systemctl daemon-reload && systemctl restart caddy
check "control: caddy can no longer reach drydock" [ "$(status "https://$UI/api/auth/session")" = 502 ]
install v0.0.2
check "with --ca-cert and drydock unreachable, the check fails" [ "$rc" != 0 ]
check "as a failure, naming the 502" grep -q "answered '502' through Caddy" <<<"$out"
check "not as a trust problem" bash -c '! grep -q "installed and running" <<<"$1"' _ "$out"
install v0.0.2 --no-ca-cert
check "without --ca-cert and drydock unreachable, it fails" [ "$rc" != 0 ]
check "and is not reported as only a trust problem" bash -c '! grep -q "installed and running" <<<"$1"' _ "$out"
check "though it gives the TLS reason too" grep -q "unable to get local issuer certificate" <<<"$out"
rm -f /etc/systemd/system/caddy.service.d/zz-test-break.conf
systemctl daemon-reload && systemctl restart caddy
install v0.0.2 --ca-cert "$CA"
check "control: repaired, the same install passes" [ "$rc" = 0 ] || printf '%s\n' "$out"

section "a mixed-case --ui-host is lowercased"
upper=$(tr 'a-z' 'A-Z' <<<"$UI")
install v0.0.2 --ui-host "$upper"
check "it succeeds" [ "$rc" = 0 ] || printf '%s\n' "$out"
check "it says it lowercased it" grep -q -- "using --ui-host $UI" <<<"$out"
check "the setting is lowercase" grep -qx "DRYDOCK_UI_HOST=$UI" /etc/drydock/drydock.env
check "drydock was given the lowercase origin" bash -c "tr '\\0' ' ' </proc/$(mainpid drydock)/cmdline | grep -q -- '--ui-origin=https://$UI '"
# The browser's half: it sends Origin lowercased whatever was typed. This sign-in
# is what was refused as forbidden_origin when the origin kept its capitals.
code=$("${CURL[@]}" -o /dev/null -w '%{http_code}' -H "Origin: https://$UI" \
	-H 'Content-Type: application/json' -d "{\"password\":\"$PW\"}" "https://$UI/api/auth/session")
check "a sign-in with the browser's lowercase Origin works (204)" [ "$code" = 204 ]
sed -i "s/^DRYDOCK_UI_HOST=.*/DRYDOCK_UI_HOST=$upper/" /etc/drydock/drydock.env
install v0.0.2
check "a drydock.env from an older installer is repaired by a re-run" grep -qx "DRYDOCK_UI_HOST=$UI" /etc/drydock/drydock.env
check "and the re-run passes" [ "$rc" = 0 ] || printf '%s\n' "$out"
# Drydock itself refuses to start on one: the installer is not the only guard.
# (The control is the service above, running on the same binary in lowercase.)
srvout=$(timeout 5 runuser -u drydock -- drydock serve --db /tmp/never.db --ui-origin "https://$upper" --ui-host "$upper" \
	--api-socket /tmp/never-http.sock --preview-socket /tmp/never-preview.sock 2>&1)
check "drydock serve refuses a mixed-case origin at startup" grep -q "must be lowercase" <<<"$srvout"
check "before it opens a database" [ ! -e /tmp/never.db ]
check "control: the installed service is serving" [ "$(status "https://$UI/api/auth/session")" = 401 ]

section "a master key that is not a key is refused, never replaced"
cp -p "$SK" /root/secrets.key.good
head -c 31 /dev/urandom >"$SK"
damaged=$(sha256sum "$SK")
install v0.0.2
check "it fails" [ "$rc" != 0 ]
check "it says why, and what replacing it costs" grep -q "never replaced automatically" <<<"$out"
check "the file is untouched" [ "$(sha256sum "$SK")" = "$damaged" ]
cp -p /root/secrets.key.good "$SK"
install v0.0.2
check "control: with the key restored, the re-run succeeds" [ "$rc" = 0 ] || printf '%s\n' "$out"
check "and the secret still decrypts" grep -q '"rotated":false' <<<"$(put TEST_KEY install-test-value)"

section "a first install with the App key, from nothing"
# Every install above found /etc/drydock already there, made by the first
# install, which had no App key. v0.2.0 wrote the key into that directory
# before anything created it, so exactly this install failed.
forget_drydock
check "control: the host has no /etc/drydock" [ ! -e /etc/drydock ]
check "control: nor a drydock binary" [ ! -e /usr/local/bin/drydock ]
install v0.0.2 --ui-host "$UI" --cert /etc/ssl/drydock/ui.pem --key /etc/ssl/drydock/ui.key --ca-cert "$CA" \
	--github-app-id 5189455 --github-app-key /root/app.pem
check "it succeeds (and its own end-to-end 401 check passed)" [ "$rc" = 0 ]
[ "$rc" = 0 ] || printf '%s\n' "$out"
check "it reports a fresh install" grep -q "installed Drydock v0.0.2" <<<"$out"
check "/etc/drydock is root's, 0755" [ "$(stat -c '%U %G %a' /etc/drydock)" = "root root 755" ]
check "the App key is drydock's alone, mode 0400" [ "$(stat -c '%U %G %a' /etc/drydock/github-app.pem)" = "drydock drydock 400" ]
check "and is the key given" cmp -s /root/app.pem /etc/drydock/github-app.pem
check "the master key was created beside it" [ "$(stat -c '%U %G %a %s' "$SK")" = "drydock drydock 400 32" ]
check "no temporary file was left behind" bash -c '! compgen -G "/etc/drydock/.*.??????" >/dev/null'
check "drydock is given the key as a path" bash -c "tr '\\0' ' ' </proc/$(mainpid drydock)/cmdline | grep -q -- '--github-app-key=/etc/drydock/github-app.pem'"
printf '%s\n' "$PW" | runuser -u drydock -- drydock passwd --db /var/lib/drydock/drydock.db >/dev/null
jar=$(mktemp)
code=$("${CURL[@]}" -o /dev/null -w '%{http_code}' -c "$jar" -H "Origin: https://$UI" \
	-H 'Content-Type: application/json' -d "{\"password\":\"$PW\"}" "https://$UI/api/auth/session")
check "sign-in works on the fresh install (204)" [ "$code" = 204 ]
check "and the repo list is configured from the start (200)" [ "$(status -b "$jar" "https://$UI/api/repos")" = 200 ]
pid_d=$(mainpid drydock)
install v0.0.2 --github-app-id 5189455 --github-app-key /root/app.pem
check "a re-run with the same key succeeds" [ "$rc" = 0 ]
check "control: drydock was running before the re-run" [ "$pid_d" != 0 ]
check "and the re-run restarts nothing" [ "$(mainpid drydock)" = "$pid_d" ]
check "and leaves /etc/drydock as it was" [ "$(stat -c '%U %G %a' /etc/drydock)" = "root root 755" ]

section "the secrets master key from a file (--secrets-key)"
forget_drydock
SK1=/root/sk1.key SK2=/root/sk2.key
head -c 32 /dev/urandom >"$SK1" && head -c 32 /dev/urandom >"$SK2" && chmod 0600 "$SK1" "$SK2"
check "control: the two keys differ" bash -c "! cmp -s $SK1 $SK2"
FIRST=(--ui-host "$UI" --cert /etc/ssl/drydock/ui.pem --key /etc/ssl/drydock/ui.key --ca-cert "$CA")
# The refusals first, on a first install: each must stop before anything is
# installed, binary included.
head -c 31 /dev/urandom >/root/short.key
install v0.0.2 "${FIRST[@]}" --secrets-key /root/short.key
check "a 31-byte key is refused" [ "$rc" != 0 ]
check "it says the size, and how to make one" grep -q "is 31 bytes; a secrets master key is exactly 32 raw bytes" <<<"$out"
check "before anything was installed" bash -c "[ ! -e /usr/local/bin/drydock ] && [ ! -e $SK ]"
base64 -w0 "$SK1" >/root/b64.key
install v0.0.2 "${FIRST[@]}" --secrets-key /root/b64.key
check "a base64-encoded key is refused, and it says why" bash -c '[ "$1" != 0 ] && grep -q "is 44 bytes" <<<"$2" && grep -q "not base64" <<<"$2"' _ "$rc" "$out"
check "and nothing was installed" [ ! -e /usr/local/bin/drydock ]
install v0.0.2 "${FIRST[@]}" --secrets-key /root/missing.key
check "a missing file is refused" bash -c '[ "$1" != 0 ] && grep -q "no such file: /root/missing.key" <<<"$2"' _ "$rc" "$out"
mkdir -p /root/dir.key
install v0.0.2 "${FIRST[@]}" --secrets-key /root/dir.key
check "a directory is refused" bash -c '[ "$1" != 0 ] && grep -q "is not a regular file" <<<"$2"' _ "$rc" "$out"
check "and nothing was installed" bash -c "[ ! -e /usr/local/bin/drydock ] && [ ! -e $SK ]"

install v0.0.2 "${FIRST[@]}" --secrets-key "$SK1"
check "control: the same first install with a 32-byte key succeeds" [ "$rc" = 0 ] || printf '%s\n' "$out"
check "it installed the bytes given" cmp -s "$SK1" "$SK"
check "drydock's alone, mode 0400" [ "$(stat -c '%U %G %a %s' "$SK")" = "drydock drydock 400 32" ]
check "it says where it came from" grep -q "installed the secrets master key at $SK from $SK1" <<<"$out"
check "it generated nothing" bash -c '! grep -q "created the secrets master key" <<<"$1"' _ "$out"
check "no temporary file was left behind" bash -c '! compgen -G "/etc/drydock/.*.??????" >/dev/null'
check "the flag is not kept in drydock.env" bash -c "! grep -q -- '$SK1' /etc/drydock/drydock.env && ! grep -qi secrets /etc/drydock/drydock.env"
printf '%s\n' "$PW" | runuser -u drydock -- drydock passwd --db /var/lib/drydock/drydock.db >/dev/null
jar=$(mktemp)
code=$("${CURL[@]}" -o /dev/null -w '%{http_code}' -c "$jar" -H "Origin: https://$UI" \
	-H 'Content-Type: application/json' -d "{\"password\":\"$PW\"}" "https://$UI/api/auth/session")
check "sign-in works (204)" [ "$code" = 204 ]
check "count-secrets says none are stored" [ "$(runuser -u drydock -- drydock count-secrets --db /var/lib/drydock/drydock.db)" = 0 ]

pid_d=$(mainpid drydock)
install v0.0.2 --secrets-key "$SK1"
check "a re-run with the same key succeeds" [ "$rc" = 0 ] || printf '%s\n' "$out"
check "control: drydock was running before it" [ "$pid_d" != 0 ]
check "and restarts nothing" [ "$(mainpid drydock)" = "$pid_d" ]
check "and says nothing about the key" bash -c '! grep -q "secrets master key" <<<"$1"' _ "$out"
check "the key is unchanged" cmp -s "$SK1" "$SK"
install v0.0.2
check "a re-run without the flag succeeds" [ "$rc" = 0 ]
check "and keeps the supplied key, restarting nothing" bash -c "cmp -s $SK1 $SK && [ \$(systemctl show -p MainPID --value drydock) = $pid_d ]"

install v0.0.2 --secrets-key "$SK2"
check "a different key with no secret stored is accepted" [ "$rc" = 0 ] || printf '%s\n' "$out"
check "it is installed" cmp -s "$SK2" "$SK"
check "drydock's alone, mode 0400" [ "$(stat -c '%U %G %a %s' "$SK")" = "drydock drydock 400 32" ]
check "it says it replaced the key" grep -q "replaced the secrets master key at $SK with $SK2" <<<"$out"
check "it stopped drydock for the swap" grep -q "stopping drydock to replace the secrets master key" <<<"$out"
check "and drydock was restarted onto it" [ "$(mainpid drydock)" != "$pid_d" ]
check "and is running" systemctl is-active --quiet drydock
check "drydock is given it as a path" bash -c "tr '\\0' ' ' </proc/$(mainpid drydock)/cmdline | grep -q -- '--secrets-key=$SK'"
check "a secret can be stored under the new key" grep -q '"created":true' <<<"$(put TEST_KEY install-test-value)"
check "and read back: the same value is not a rotation" grep -q '"rotated":false' <<<"$(put TEST_KEY install-test-value)"
check "count-secrets says one is stored" [ "$(runuser -u drydock -- drydock count-secrets --db /var/lib/drydock/drydock.db)" = 1 ]

key_sum=$(sha256sum "$SK") pid_d=$(mainpid drydock)
install v0.0.2 --secrets-key "$SK1"
check "a different key with a secret stored is refused" [ "$rc" != 0 ]
check "it says how many secrets the swap would break" grep -q "1 stored secret(s) are sealed under the installed key" <<<"$out"
check "and what to do instead" grep -q "delete the stored secrets on Drydock's Secrets screen" <<<"$out"
check "the installed key is byte-identical" [ "$(sha256sum "$SK")" = "$key_sum" ]
check "and is still the one installed before" cmp -s "$SK2" "$SK"
check "drydock was neither stopped nor restarted" [ "$(mainpid drydock)" = "$pid_d" ]
check "it did not even stop it to look" bash -c '! grep -q "stopping drydock" <<<"$1"' _ "$out"
check "no temporary file was left behind" bash -c '! compgen -G "/etc/drydock/.*.??????" >/dev/null'
check "the secret is still served: the same value is not a rotation" grep -q '"rotated":false' <<<"$(put TEST_KEY install-test-value)"
check "and there is still one secret" [ "$(runuser -u drydock -- drydock count-secrets --db /var/lib/drydock/drydock.db)" = 1 ]

install v0.0.2 --secrets-key /root/short.key
check "a wrong-size key on a re-run is refused" bash -c '[ "$1" != 0 ] && grep -q "is 31 bytes" <<<"$2"' _ "$rc" "$out"
install v0.0.2 --secrets-key /root/missing.key
check "a missing file on a re-run is refused" bash -c '[ "$1" != 0 ] && grep -q "no such file" <<<"$2"' _ "$rc" "$out"
check "both left the key alone" cmp -s "$SK2" "$SK"
check "and drydock running, unrestarted" [ "$(mainpid drydock)" = "$pid_d" ]
install v0.0.2 --secrets-key "$SK2"
check "control: the installed key given again is accepted" [ "$rc" = 0 ]
check "and restarts nothing" [ "$(mainpid drydock)" = "$pid_d" ]

# The key's bytes are in no output, journal, argv, environment or setting.
# Searched as hex, raw bytes are found wherever they landed, whatever bytes
# surround them; the key's own hex and base64 are searched for as text too.
hex() { od -An -v -tx1 "$@" | tr -d ' \n'; }
absent() { # absent KEYFILE FILE...: no trace of KEYFILE's bytes in the FILEs
	local kh kb
	kh=$(hex "$1") kb=$(base64 -w0 "$1")
	shift
	! hex "$@" | grep -qF -- "$kh" && ! grep -qF -- "$kh" "$@" && ! grep -qF -- "$kb" "$@"
}
present() { ! absent "$@"; }
journalctl -o cat --no-pager >/root/journal.txt 2>/dev/null
pid=$(mainpid drydock)
for k in "$SK1" "$SK2" /root/short.key; do
	{ echo before; cat "$k"; echo after; } >/root/control.bin
	check "control: the sweep finds $(basename "$k") where it is" present "$k" /root/control.bin
	check "$(basename "$k") is in no installer output" absent "$k" /root/installer-output.log
	check "nor in the journal" absent "$k" /root/journal.txt
	check "nor in drydock's argv, environment or settings" absent "$k" "/proc/$pid/cmdline" "/proc/$pid/environ" /etc/drydock/drydock.env
done
check "control: the installer output was collected" grep -q "replaced the secrets master key" /root/installer-output.log
check "control: so was the journal" grep -q "serving on" /root/journal.txt

printf '\n%s\n' "$([ $fails = 0 ] && echo PASS || echo "$fails FAILED")"
[ $fails = 0 ]
