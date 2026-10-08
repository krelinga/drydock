#!/usr/bin/env bash
# The Ansible companion's live run: the playbook assembled from
# docs/deploy/first-deployment-ansible.md (by check.sh), run against a bare
# Debian with systemd in a privileged container, installing a locally packaged
# release the way test/install/run.sh does. The prerequisites are installed
# for real, from Docker's, NodeSource's and Caddy's repositories, so this needs
# the internet and takes a few minutes. Not part of CI.
#
#   test/ansible/live.sh           # needs ansible-core, community.docker, Docker allowing --privileged
#   KEEP=1 test/ansible/live.sh    # leave the container up to poke at
#
# What it runs: a first install, a no-op re-run (changed=0), an upgrade (with
# the database backup), another no-op, an App key rotation, and a master-key
# backup that refuses to be overwritten by a different key, then the move to a
# vaulted secrets master key: the installed key vaulted (changed=0), a new one
# while no secret is stored (replaced), and another once one is (refused) —
# also on a run that moves the release, which must leave the old binary
# running — and last the run after an interrupted upgrade, which must back up
# the database and restart onto the installed binary though `drydock version`
# already reads the new release.
set -euo pipefail
here=$(cd "$(dirname "$0")" && pwd)
root=$(cd "$here/../.." && pwd)
work=$(mktemp -d)
name="drydock-ansible-test-$$"
UI=drydock.test
cleanup() {
	if [ "${KEEP:-0}" = 1 ]; then
		echo "container left running: docker exec -it $name bash; files in $work"
	else
		docker rm -f "$name" >/dev/null 2>&1 || true
		rm -rf "$work"
	fi
}
trap cleanup EXIT
if [ -z "${DOCKER_CONFIG:-}" ]; then # as test/install/lib.sh: the base image is public
	DOCKER_CONFIG="$work/docker"
	mkdir -p "$DOCKER_CONFIG" && echo '{}' >"$DOCKER_CONFIG/config.json"
	export DOCKER_CONFIG
fi

fails=0
check() { # check DESCRIPTION COMMAND...
	local d="$1"
	shift
	if "$@"; then printf 'ok   %s\n' "$d"; else printf 'FAIL %s\n' "$d"; fails=$((fails + 1)); fi
}
in_server() { docker exec "$name" "$@"; }

# The layout, exactly as the document's blocks assemble it (and linted).
play_dir="$work/play"
"$here/check.sh" "$play_dir" >/dev/null
export ANSIBLE_COLLECTIONS_PATH="$play_dir/.collections" ANSIBLE_NOCOLOR=1
ansible-galaxy collection install community.docker -p "$play_dir/.collections" >/dev/null </dev/null

# Four releases, served from inside the "server" as DRYDOCK_DOWNLOAD_BASE is in
# test/install: v0.0.3 and v0.0.4 for an upgrade a refused key must not
# start, and one an interrupted run left half done.
for v in v0.0.1 v0.0.2 v0.0.3 v0.0.4; do
	"$root/deploy/package.sh" "$v" "$work/releases/$v" amd64 >/dev/null
done

# Runbook step 1's files, on the controller: a private CA (option A), a
# certificate for the UI host, and an App key. The two keys are vault-encrypted.
cd "$play_dir/files"
openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:P-256 -nodes -days 2 \
	-subj /CN=test-ca -keyout ca.key -out drydock-ca.pem 2>/dev/null
openssl req -newkey ec -pkeyopt ec_paramgen_curve:P-256 -nodes -subj "/CN=$UI" \
	-keyout drydock.key -out ui.csr 2>/dev/null
openssl x509 -req -in ui.csr -CA drydock-ca.pem -CAkey ca.key -CAcreateserial -days 2 \
	-extfile <(printf 'subjectAltName=DNS:%s' "$UI") -out drydock.crt 2>/dev/null
openssl genrsa -traditional -out drydock-app.pem 2048 2>/dev/null
app_sum=$(sha256sum <drydock-app.pem | cut -d' ' -f1)
rm -f ca.key ui.csr drydock-ca.srl
echo "test-vault-password" >"$work/vault-pass"
VAULT=(--vault-password-file "$work/vault-pass")
ansible-vault encrypt "${VAULT[@]}" drydock-app.pem drydock.key ../group_vars/drydock/vault.yml >/dev/null </dev/null
cd "$play_dir"

docker build -q -t drydock-ansible-test "$here" >/dev/null
docker run -d --name "$name" --privileged --cgroupns=host \
	-v /sys/fs/cgroup:/sys/fs/cgroup:rw --tmpfs /run --tmpfs /run/lock drydock-ansible-test >/dev/null
for _ in $(seq 1 100); do
	case "$(in_server systemctl is-system-running 2>/dev/null || true)" in running | degraded) break ;; esac
	sleep 0.2
done
docker cp "$work/releases" "$name:/releases"
in_server sh -c 'cd /releases && nohup python3 -m http.server 8000 --bind 127.0.0.1 >/dev/null 2>&1 &'

cat >ansible.cfg <<'EOF'
[defaults]
inventory = inventory.ini

[ssh_connection]
pipelining = True
EOF
printf '[drydock]\n%s ansible_connection=community.docker.docker ansible_user=root\n' "$name" >inventory.ini
cat >test-vars.yml <<EOF
drydock_ui_host: $UI
drydock_version: v0.0.1
drydock_release_base_url: "http://127.0.0.1:8000/{{ drydock_version }}"
drydock_ca_cert_src: "{{ playbook_dir }}/files/drydock-ca.pem"
drydock_require_clock_sync: false
drydock_secrets_key_backup: $work/backup/secrets.key
EOF
PW=$(ansible-vault view "${VAULT[@]}" group_vars/drydock/vault.yml </dev/null | sed -n 's/^vault_drydock_operator_password: "\(.*\)"$/\1/p')

# play ARGS...: one run. The App key is a fake, so the first catalog refresh
# is GitHub's 401 in the journal: that one assertion is skipped here, and the
# journal is checked below instead.
play() {
	local n=$((${run:-0} + 1))
	run=$n
	rc=0
	ansible-playbook -v drydock.yml "${VAULT[@]}" -e @test-vars.yml --skip-tags drydock_verify_journal "$@" \
		>"$work/run$n.log" 2>&1 </dev/null || rc=$?
	changed=$(sed -n "s/^$name *: .*changed=\([0-9]*\).*/\1/p" "$work/run$n.log")
	printf -- '--- run %s: rc=%s changed=%s\n' "$n" "$rc" "$changed"
	[ "$rc" = 0 ] || tail -40 "$work/run$n.log"
}
signin() {
	in_server curl -s -o /dev/null -w '%{http_code}' --cacert /etc/caddy/certs/drydock-ca.pem \
		--resolve "$UI:443:127.0.0.1" -H "Origin: https://$UI" -H 'Content-Type: application/json' \
		-d "{\"password\":\"$PW\"}" "https://$UI/api/auth/session"
}

play
check "first install succeeds" [ "$rc" = 0 ]
check "the installer reported a fresh install" grep -q "installed Drydock v0.0.1" "$work/run1.log"
check "the password from the vault signs in through Caddy (204)" [ "$(signin)" = 204 ]
check "control: a wrong password does not" [ "$(PW=wrong-password-123 signin)" = 401 ]
check "the password never reached the log" bash -c "! grep -qF -- \"\$1\" \"\$2\"" _ "$PW" "$work/run1.log"
check "the App key is installed, drydock's alone, 0400" [ "$(in_server stat -c '%U %a' /etc/drydock/github-app.pem)" = "drydock 400" ]
check "and is the controller's key" [ "$(in_server sha256sum /etc/drydock/github-app.pem | cut -d' ' -f1)" = "$app_sum" ]
check "the staged copy is gone" in_server test ! -e /root/drydock-app.pem
check "the key is 0640 root:caddy" [ "$(in_server stat -c '%U %G %a' /etc/caddy/certs/drydock.key)" = "root caddy 640" ]
check "the journal says it is serving" bash -c "docker exec $name journalctl -u drydock -o cat | grep -q 'drydock: serving on /run/drydock/http.sock'"
check "and its only complaint is the fake App key" bash -c "! docker exec $name journalctl -u drydock -o cat | grep -E '^drydock(: reconcile:| serve:)'"
check "the master key was backed up, 0400" [ "$(stat -c %a "$work/backup/secrets.key")" = 400 ]
check "and is the server's key" [ "$(sha256sum <"$work/backup/secrets.key")" = "$(in_server cat /etc/drydock/secrets.key | sha256sum)" ]
check "in a 0700 directory" [ "$(stat -c %a "$work/backup")" = 700 ]

play
check "a re-run succeeds" [ "$rc" = 0 ]
check "and changes nothing" [ "$changed" = 0 ]
check "the installer said current" grep -q "v0.0.1 is installed and current" "$work/run2.log"

play -e drydock_version=v0.0.2
check "an upgrade succeeds" [ "$rc" = 0 ]
check "the installer reported the move" grep -q "upgraded Drydock v0.0.1 -> v0.0.2" "$work/run3.log"
check "the binary is v0.0.2" [ "$(in_server drydock version)" = v0.0.2 ]
check "the database was backed up first, root 0600" bash -c "[ \"\$(docker exec $name sh -c 'stat -c \"%U %a\" /root/drydock.db.*')\" = 'root 600' ]"
# 200, not 204: the first sign-in since the wrong password above reports it (design §12).
check "the password was not reset" [ "$(signin)" = 200 ]

play -e drydock_version=v0.0.2
check "a re-run after the upgrade succeeds" [ "$rc" = 0 ]
check "and changes nothing" [ "$changed" = 0 ]

openssl genrsa -traditional -out files/drydock-app.pem 2048 2>/dev/null
app_sum=$(sha256sum <files/drydock-app.pem | cut -d' ' -f1)
ansible-vault encrypt "${VAULT[@]}" files/drydock-app.pem >/dev/null </dev/null
play -e drydock_version=v0.0.2
check "rotating the App key succeeds" [ "$rc" = 0 ]
check "the installer installed the new key" grep -q "installed the GitHub App key" "$work/run5.log"
check "the server has the new key" [ "$(in_server sha256sum /etc/drydock/github-app.pem | cut -d' ' -f1)" = "$app_sum" ]
check "the staged copy is gone again" in_server test ! -e /root/drydock-app.pem

chmod 0600 "$work/backup/secrets.key" && head -c 32 /dev/urandom >"$work/backup/secrets.key"
play -e drydock_version=v0.0.2
check "a backup of a different key fails the play" [ "$rc" != 0 ]
check "at the refusal" grep -q "holds a different key" "$work/run6.log"
check "control: the server's key is unchanged and still drydock's" [ "$(in_server stat -c '%U %a %s' /etc/drydock/secrets.key)" = "drydock 400 32" ]

# Option A, from an install that generated its key: the document's "Move to a
# vaulted key". First the installed key itself, vaulted. The backup above is
# still a different key, so a run that reached the backup tasks would fail.
server_sum() { in_server sha256sum /etc/drydock/secrets.key | cut -d' ' -f1; }
in_server cat /etc/drydock/secrets.key >files/drydock-secrets.key
check "control: the fetched key is the server's" [ "$(sha256sum <files/drydock-secrets.key | cut -d' ' -f1)" = "$(server_sum)" ]
ansible-vault encrypt "${VAULT[@]}" files/drydock-secrets.key >/dev/null </dev/null
cp files/drydock-secrets.key "$work/key1.vault"
KEYSRC=(-e drydock_version=v0.0.2 -e '{"drydock_secrets_key_src": "{{ playbook_dir }}/files/drydock-secrets.key"}')
sum1=$(server_sum) pid=$(in_server systemctl show -p MainPID --value drydock)
play "${KEYSRC[@]}"
check "vaulting the installed key: the run succeeds, skipping the backup" [ "$rc" = 0 ]
check "and changes nothing" [ "$changed" = 0 ]
check "the server's key is the same" [ "$(server_sum)" = "$sum1" ]
check "drydock was not restarted" [ "$(in_server systemctl show -p MainPID --value drydock)" = "$pid" ]
check "nothing is left staged" in_server test ! -e /root/drydock-secrets.key

# Then a new key, with no secret stored: replaced.
head -c 32 /dev/urandom >files/drydock-secrets.key
sum2=$(sha256sum <files/drydock-secrets.key | cut -d' ' -f1)
ansible-vault encrypt "${VAULT[@]}" files/drydock-secrets.key >/dev/null </dev/null
play "${KEYSRC[@]}"
n=$run
check "a new vaulted key with no secret stored is installed" [ "$rc" = 0 ]
check "the installer replaced the key" grep -q "replaced the secrets master key" "$work/run$n.log"
check "the server holds the new key, drydock's, 0400" bash -c "[ \"\$(docker exec $name sha256sum /etc/drydock/secrets.key | cut -d' ' -f1)\" = $sum2 ] && [ \"\$(docker exec $name stat -c '%U %a %s' /etc/drydock/secrets.key)\" = 'drydock 400 32' ]"
check "the run reported a change" [ "$changed" != 0 ]
check "the staged copy is gone" in_server test ! -e /root/drydock-secrets.key
check "sign-in still works" [ "$(signin)" = 204 ]
play "${KEYSRC[@]}"
check "a re-run with it succeeds" [ "$rc" = 0 ]
check "and changes nothing" [ "$changed" = 0 ]

# A secret stored, then a third key: refused, nothing changed.
in_server curl -s -o /dev/null -c /root/jar --cacert /etc/caddy/certs/drydock-ca.pem \
	--resolve "$UI:443:127.0.0.1" -H "Origin: https://$UI" -H 'Content-Type: application/json' \
	-d "{\"password\":\"$PW\"}" "https://$UI/api/auth/session"
stored=$(in_server curl -s -b /root/jar -X PUT --cacert /etc/caddy/certs/drydock-ca.pem \
	--resolve "$UI:443:127.0.0.1" -H "Origin: https://$UI" -H 'Content-Type: application/json' \
	-d '{"value":"ansible-test-value","reach":"reads a scratch bucket"}' "https://$UI/api/secrets/TEST_KEY")
check "control: a secret is stored" grep -q '"created":true' <<<"$stored"
cp files/drydock-secrets.key "$work/key2.vault"
head -c 32 /dev/urandom >files/drydock-secrets.key
ansible-vault encrypt "${VAULT[@]}" files/drydock-secrets.key >/dev/null </dev/null
cp files/drydock-secrets.key "$work/key3.vault"
pid=$(in_server systemctl show -p MainPID --value drydock)
play "${KEYSRC[@]}"
n=$run
check "a different vaulted key with a secret stored fails the play" [ "$rc" != 0 ]
check "with the installer's refusal" grep -q "1 stored secret(s) are sealed under the installed key" "$work/run$n.log"
check "the server's key is unchanged" [ "$(server_sum)" = "$sum2" ]
check "drydock was not restarted" [ "$(in_server systemctl show -p MainPID --value drydock)" = "$pid" ]
check "the staged copy is gone, even so" in_server test ! -e /root/drydock-secrets.key

# The same refused key on a run that also moves the release: still nothing
# changed, the binary included (#46), and the run after it, with the server's
# key, backs up and upgrades rather than finding the new binary "current".
runs_installed() { in_server sh -c 'pid=$(systemctl show -p MainPID --value drydock); [ "$pid" != 0 ] && [ /proc/$pid/exe -ef /usr/local/bin/drydock ]'; }
not_runs_installed() { ! runs_installed; }
backups() { in_server sh -c 'ls /root/drydock.db.* 2>/dev/null | wc -l'; }
KEYSRC3=(-e drydock_version=v0.0.3 -e '{"drydock_secrets_key_src": "{{ playbook_dir }}/files/drydock-secrets.key"}')
play "${KEYSRC3[@]}"
n=$run
check "a different vaulted key on an upgrade fails the play" [ "$rc" != 0 ]
check "with the installer's refusal" grep -q "1 stored secret(s) are sealed under the installed key" "$work/run$n.log"
check "the installed binary is still v0.0.2" [ "$(in_server drydock version)" = v0.0.2 ]
check "and drydock runs it, not a deleted file" runs_installed
check "the server's key is unchanged" [ "$(server_sum)" = "$sum2" ]
cp "$work/key2.vault" files/drydock-secrets.key
b=$(backups)
play "${KEYSRC3[@]}"
n=$run
check "the same upgrade with the server's key succeeds" [ "$rc" = 0 ]
check "it backed up the database first" [ "$(backups)" = $((b + 1)) ]
check "and upgraded" grep -q "upgraded Drydock v0.0.2 -> v0.0.3" "$work/run$n.log"
check "drydock runs the installed file" runs_installed

# What an interrupted run leaves: the next release's binary in place, the old
# one still running, deleted. drydock version reads the file, so only the
# running process's inode says an upgrade is still to come.
in_server sh -c 'mkdir -p /root/v4 && tar -xzf /releases/v0.0.4/drydock_linux_amd64.tar.gz -C /root/v4 &&
	install -m 0755 /root/v4/drydock/drydock /usr/local/bin/drydock.new && mv -f /usr/local/bin/drydock.new /usr/local/bin/drydock'
check "control: the file reads v0.0.4" [ "$(in_server drydock version)" = v0.0.4 ]
check "control: and drydock runs a deleted file" not_runs_installed
KEYSRC4=(-e drydock_version=v0.0.4 -e '{"drydock_secrets_key_src": "{{ playbook_dir }}/files/drydock-secrets.key"}')
b=$(backups)
play "${KEYSRC4[@]}"
n=$run
check "the run after an interrupted upgrade succeeds" [ "$rc" = 0 ]
check "it backed up the database, though the file already read v0.0.4" [ "$(backups)" = $((b + 1)) ]
check "the backup's restart moved drydock onto v0.0.4, so the installer found it current" grep -q "Drydock v0.0.4 is installed and current" "$work/run$n.log"
check "drydock runs the installed file" runs_installed
b=$(backups)
play "${KEYSRC4[@]}"
check "control: the next run changes nothing" [ "$changed" = 0 ]
check "and backs up nothing" [ "$(backups)" = "$b" ]

# No key's bytes in any run's output, raw (searched as hex) or encoded.
cat "$work"/run*.log >"$work/all-runs.log"
hex() { od -An -v -tx1 "$@" | tr -d ' \n'; }
for k in key1 key2 key3; do
	ansible-vault decrypt "${VAULT[@]}" --output "$work/$k.raw" "$work/$k.vault" >/dev/null </dev/null
	kh=$(hex "$work/$k.raw") kb=$(base64 -w0 "$work/$k.raw")
	{ echo x; cat "$work/$k.raw"; echo y; } >"$work/control.bin"
	check "control: the sweep finds $k where it is" bash -c "od -An -v -tx1 $work/control.bin | tr -d ' \\n' | grep -qF $kh"
	check "$k is in no run's output" bash -c "! od -An -v -tx1 $work/all-runs.log | tr -d ' \\n' | grep -qF $kh && ! grep -qF -- '$kh' $work/all-runs.log && ! grep -qF -- '$kb' $work/all-runs.log"
done

printf '\n%s\n' "$([ $fails = 0 ] && echo PASS || echo "$fails FAILED")"
[ $fails = 0 ]
