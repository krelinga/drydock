# Shared checks for every scenario: the clients are installed where every
# shell finds them first, git is wired to the helper, and with no broker
# socket each client says "GitHub access unavailable" rather than failing
# some other way.

check "the gh found first is the shim" bash -c '[ "$(readlink -f "$(command -v gh)")" = /usr/local/drydock/bin/gh ]'
# Debian's /etc/profile replaces PATH in login shells, dropping containerEnv's
# prefix; the link in /usr/local/bin is what still finds the shim there.
check "and in a login shell" bash -lc '[ "$(readlink -f "$(command -v gh)")" = /usr/local/drydock/bin/gh ]'
check "the gh at /usr/local/bin is the shim" bash -c '[ "$(readlink /usr/local/bin/gh)" = /usr/local/drydock/bin/gh ]'
check "git uses the broker for github.com" bash -c '[ "$(git config --system credential.https://github.com.helper)" = /usr/local/drydock/bin/drydock-credential ]'
check "a broker client is installed" bash -c 'command -v socat || command -v nc'
check "the broker socket path is in the environment" bash -c '[ "$DRYDOCK_BROKER_SOCK" = /run/drydock/broker.sock ]'

check "no socket: the helper says so" bash -c \
	'out=$(printf "host=github.com\n\n" | drydock-credential get 2>&1); [ $? -ne 0 ] && echo "$out" | grep -q "GitHub access unavailable"'
check "no socket: the gh shim says so" bash -c \
	'out=$(gh api user 2>&1); [ $? -ne 0 ] && echo "$out" | grep -q "GitHub access unavailable"'
check "no socket, not required: the probe warns and passes" bash -c \
	'out=$(drydock-probe 2>&1) && echo "$out" | grep -q "broker not required"'

# Secrets (design §10.3): CLAUDE_ENV_FILE names the Feature's one-line script,
# and with no broker that line ends the command shell with 69 — the command
# after it never runs — and says why on one line. requireBroker does not
# soften this: a command without its secrets must not run.
check "CLAUDE_ENV_FILE is the one constant line" bash -c \
	'[ "$CLAUDE_ENV_FILE" = /usr/local/drydock/etc/claude-env.sh ] && [ "$(cat "$CLAUDE_ENV_FILE")" = "eval \"\$(drydock-secrets export || echo exit 69)\"" ] && [ "$(wc -l <"$CLAUDE_ENV_FILE")" = 1 ]'
check "drydock-secrets is found first, in a login shell too" bash -lc \
	'[ "$(readlink -f "$(command -v drydock-secrets)")" = /usr/local/drydock/bin/drydock-secrets ]'
check "no socket: the prelude aborts the command, on one line" bash -c \
	'out=$(bash -c "$(cat "$CLAUDE_ENV_FILE") && echo ran" 2>&1); rc=$?; [ $rc = 69 ] && ! echo "$out" | grep -q ran && [ "$(echo "$out" | wc -l)" = 1 ] && echo "$out" | grep -q "secrets unavailable"'

# --- Claude Code: pinned, on the shared volume, keys written (design §11) ---
# Every scenario mounts a named volume at /home/vscode/.claude, as Drydock
# does, because the Feature's postCreateCommand refuses a container without
# one; the expected-failure builds in feature/expect-fail/ cover what happens
# when that and the other preflight checks do not hold.

check "Claude Code is the pinned version, found first" bash -c \
	'[ "$(claude --version)" = "2.1.289 (Claude Code)" ] && [ "$(readlink -f "$(command -v claude)")" = /usr/local/drydock/claude/2.1.289/claude ]'
check "and in a login shell" bash -lc \
	'[ "$(claude --version)" = "2.1.289 (Claude Code)" ] && [ "$(readlink -f "$(command -v claude)")" = /usr/local/drydock/claude/2.1.289/claude ]'
check "the autoupdater is off, in a login shell too" bash -lc '[ "$DISABLE_AUTOUPDATER" = 1 ]'
check "the install recorded the version" grep -qx CLAUDE_CODE_VERSION=2.1.289 /usr/local/drydock/etc/feature.env
check "CLAUDE_CONFIG_DIR is the shared volume's mount point, the remote user's, 0700" bash -c \
	'[ "$CLAUDE_CONFIG_DIR" = /home/vscode/.claude ] && [ "$(awk "\$5 == \"/home/vscode/.claude\"" /proc/self/mountinfo | wc -l)" = 1 ] && [ "$(stat -c %u:%a "$CLAUDE_CONFIG_DIR")" = "$(id -u):700" ]'
# postCreateCommand ran in the workspace folder, which is where this runs.
check "postCreate wrote both Remote Control keys for the workspace folder" bash -c \
	'jq -e --arg ws "$PWD" ".remoteDialogSeen == true and .projects[\$ws].hasTrustDialogAccepted == true" "$CLAUDE_CONFIG_DIR/.claude.json" && [ "$(stat -c %a "$CLAUDE_CONFIG_DIR/.claude.json")" = 600 ] && [ ! -e "$CLAUDE_CONFIG_DIR/.claude.json.lock" ]'

# drydock-preflight, run directly. The control first: in this container, as
# built, it passes and says nothing. Then each variable of §2.1, one at a
# time, must fail it by name — empty counts as set — while the other four
# are named nowhere in its output.
check "preflight passes, silently, in the container as built" bash -c \
	'out=$(drydock-preflight 2>&1) && [ -z "$out" ]'
for v in ANTHROPIC_BASE_URL DISABLE_TELEMETRY DO_NOT_TRACK CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC DISABLE_GROWTHBOOK; do
	check "preflight fails naming $v, and only $v" bash -c '
		for val in 1 ""; do
			[ $0 = ANTHROPIC_BASE_URL ] && [ -n "$val" ] && val=https://gateway.example.com
			out=$(env "$0=$val" drydock-preflight 2>&1) && { echo "passed with $0=$val"; exit 1; }
			echo "$out" | grep -q "^drydock feature: $0 is set" || { echo "$out"; exit 1; }
			[ "$(echo "$out" | grep -cE "(ANTHROPIC_BASE_URL|DISABLE_TELEMETRY|DO_NOT_TRACK|CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC|DISABLE_GROWTHBOOK) is set")" = 1 ] || { echo "$out"; exit 1; }
		done' "$v"
done
check "ANTHROPIC_BASE_URL pointing at api.anthropic.com is allowed" bash -c \
	'env ANTHROPIC_BASE_URL=https://api.anthropic.com drydock-preflight && env ANTHROPIC_BASE_URL=https://api.anthropic.com/ drydock-preflight'
# A project's settings.local.json env block reaches every session the same
# way containerEnv does. The control is the same project with a harmless env.
check "a settings env block that disables Remote Control fails preflight; a harmless one does not" bash -c '
	rm -rf /tmp/proj && mkdir -p /tmp/proj/.claude && cd /tmp/proj &&
	echo "{\"env\":{\"FOO\":\"1\"}}" >.claude/settings.local.json && drydock-preflight &&
	echo "{\"env\":{\"FOO\":\"1\",\"DISABLE_GROWTHBOOK\":\"1\"}}" >.claude/settings.local.json &&
	out=$(drydock-preflight 2>&1); [ $? -ne 0 ] && echo "$out" | grep -q "DISABLE_GROWTHBOOK is set in the env block of /tmp/proj/.claude/settings.local.json"'
check "CLAUDE_CONFIG_DIR moved elsewhere fails preflight, naming it" bash -c \
	'mkdir -p /tmp/elsewhere && out=$(CLAUDE_CONFIG_DIR=/tmp/elsewhere drydock-preflight 2>&1); [ $? -ne 0 ] && echo "$out" | grep -q "CLAUDE_CONFIG_DIR is ./tmp/elsewhere."'

# drydock-claude-config, against a scratch CLAUDE_CONFIG_DIR seeded the way a
# signed-in volume looks: the account record, another project, settings.
seed() {
	rm -rf /tmp/cc && mkdir -p /tmp/cc && chmod 700 /tmp/cc
	cat >/tmp/cc/.claude.json <<'JSON'
{
  "numStartups": 12,
  "userID": "a1b2c3",
  "oauthAccount": { "accountUuid": "acct-1", "emailAddress": "op@example.com", "organizationUuid": "org-1" },
  "remoteControlMachineId": "m-1",
  "projects": { "/workspaces/other": { "allowedTools": ["Bash"], "hasTrustDialogAccepted": true } }
}
JSON
	chmod 600 /tmp/cc/.claude.json
}
check "the keys are merged in, and everything else survives" bash -c "$(declare -f seed)"'
	seed && before=$(jq -S "del(.remoteDialogSeen, .projects[\"/workspaces/w\"])" /tmp/cc/.claude.json) &&
	CLAUDE_CONFIG_DIR=/tmp/cc drydock-claude-config /workspaces/w &&
	jq -e ".remoteDialogSeen == true and .projects[\"/workspaces/w\"].hasTrustDialogAccepted == true" /tmp/cc/.claude.json >/dev/null &&
	[ "$(jq -S "del(.remoteDialogSeen, .projects[\"/workspaces/w\"])" /tmp/cc/.claude.json)" = "$before" ] &&
	jq -e ".oauthAccount.organizationUuid == \"org-1\"" /tmp/cc/.claude.json >/dev/null &&
	[ "$(stat -c %a /tmp/cc/.claude.json)" = 600 ] && [ ! -e /tmp/cc/.claude.json.lock ] && [ -z "$(ls -A /tmp/cc | grep -v "^\.claude\.json$")" ]'
check "with no .claude.json yet, it is created holding just the keys" bash -c \
	'rm -rf /tmp/cc && mkdir /tmp/cc && CLAUDE_CONFIG_DIR=/tmp/cc drydock-claude-config /w && [ "$(jq -c . /tmp/cc/.claude.json)" = "{\"remoteDialogSeen\":true,\"projects\":{\"/w\":{\"hasTrustDialogAccepted\":true}}}" ]'
check "a second run leaves the file alone" bash -c "$(declare -f seed)"'
	seed && CLAUDE_CONFIG_DIR=/tmp/cc drydock-claude-config /w && i=$(stat -c %i /tmp/cc/.claude.json) &&
	CLAUDE_CONFIG_DIR=/tmp/cc drydock-claude-config /w && [ "$(stat -c %i /tmp/cc/.claude.json)" = "$i" ]'
check "a file that is not a JSON object is refused, not replaced" bash -c '
	for body in "" "[]" "{\"oauthAccount\": "; do
		rm -rf /tmp/cc && mkdir /tmp/cc && printf %s "$body" >/tmp/cc/.claude.json
		out=$(CLAUDE_CONFIG_DIR=/tmp/cc drydock-claude-config /w 2>&1) && exit 1
		echo "$out" | grep -q "is not a JSON object" && [ "$(cat /tmp/cc/.claude.json)" = "$body" ] && [ ! -e /tmp/cc/.claude.json.lock ] || exit 1
	done'
# Claude Code's lock: a writer that finds it held waits, and writes only once
# it is released — the control for the merge above, which never contended.
check "it waits for Claude Code's .claude.json.lock, then writes" bash -c "$(declare -f seed)"'
	seed && mkdir /tmp/cc/.claude.json.lock &&
	{ CLAUDE_CONFIG_DIR=/tmp/cc drydock-claude-config /w & } && pid=$! && sleep 3 &&
	! jq -e ".remoteDialogSeen" /tmp/cc/.claude.json >/dev/null && kill -0 $pid &&
	rmdir /tmp/cc/.claude.json.lock && wait $pid &&
	jq -e ".remoteDialogSeen == true and .oauthAccount.accountUuid == \"acct-1\"" /tmp/cc/.claude.json >/dev/null'
check "a lock never released fails it, by name, with the file untouched and the lock left" bash -c "$(declare -f seed)"'
	seed && mkdir /tmp/cc/.claude.json.lock && sum=$(sha256sum </tmp/cc/.claude.json) &&
	out=$(DRYDOCK_CLAUDE_LOCK_WAIT=2 CLAUDE_CONFIG_DIR=/tmp/cc drydock-claude-config /w 2>&1); [ $? -ne 0 ] &&
	echo "$out" | grep -q "/tmp/cc/.claude.json.lock has been held" && [ "$(sha256sum </tmp/cc/.claude.json)" = "$sum" ] && [ -d /tmp/cc/.claude.json.lock ]'
# Twenty containers' worth of writers at once, each trusting its own folder:
# with the lock, no write is lost.
check "concurrent writers lose no update" bash -c "$(declare -f seed)"'
	seed && for i in $(seq 20); do CLAUDE_CONFIG_DIR=/tmp/cc drydock-claude-config /workspaces/w$i & done; wait &&
	[ "$(jq "[.projects | to_entries[] | select(.key | startswith(\"/workspaces/w\")) | select(.value.hasTrustDialogAccepted)] | length" /tmp/cc/.claude.json)" = 20 ] &&
	jq -e ".oauthAccount.organizationUuid == \"org-1\" and .projects[\"/workspaces/other\"].allowedTools == [\"Bash\"]" /tmp/cc/.claude.json >/dev/null'

# A stand-in broker, so the clients get past the token fetch to what they do
# with it. Without one, a gh shim that execs itself forever still "fails
# fast" at the fetch, and the loop check below would be vacuous. It answers
# GET-SECRETS with two secrets, one of them hostile: a value that would run
# a command if export did not quote it.
cat >/tmp/fake-broker.sh <<'BROKER'
#!/bin/sh
read -r l
case $l in
GET-SECRETS)
	printf '%s\n' 'OK count=2' 'TEST_DATABASE_URL postgres://u:p@db:5432/test' \
		"TRICKY it's \$(touch /tmp/pwned); \`touch /tmp/pwned\`; '; touch /tmp/pwned; '" 'END'
	;;
*) echo "OK token=ghs_FakeTokenForFeatureTests expires_at=2030-01-01T00:00:00Z" ;;
esac
BROKER
chmod +x /tmp/fake-broker.sh
socat UNIX-LISTEN:/tmp/fake-broker.sock,fork,mode=666 EXEC:/tmp/fake-broker.sh &
for _ in $(seq 50); do [ -S /tmp/fake-broker.sock ] && break; sleep 0.1; done
export DRYDOCK_BROKER_SOCK=/tmp/fake-broker.sock

check "with a broker, the prelude is silent and the command sees the secrets" bash -c \
	'out=$(bash -c "$(cat "$CLAUDE_ENV_FILE") && printf %s \"\$TEST_DATABASE_URL\"" 2>&1) && [ "$out" = postgres://u:p@db:5432/test ]'
# A helper that cannot run at all prints nothing, and `eval` of an empty
# substitution succeeds; the env file's `|| echo exit 69` is what stops the
# command then. Asserted on a marker the command would write, not a status.
# The control comes first, in the same check: with the helper on PATH and the
# broker answering, the same text runs the command, which sees its secret.
# "missing" is a PATH without the Feature's directories; "noexec" puts a
# non-executable copy first on it. Both shells, since sh is dash here.
mkdir -p /tmp/noexec && cp /usr/local/drydock/bin/drydock-secrets /tmp/noexec/ && chmod 0644 /tmp/noexec/drydock-secrets
check "a helper that cannot run stops the command; a working one does not" bash -c '
	pre=$(cat "$CLAUDE_ENV_FILE")
	for sh in sh bash; do
		rm -f /tmp/ran; $sh -c "$pre && printf %s \"\$TEST_DATABASE_URL\" >/tmp/ran"
		[ "$(cat /tmp/ran 2>/dev/null)" = postgres://u:p@db:5432/test ] || { echo "control failed in $sh"; exit 1; }
		for p in /usr/bin:/bin /tmp/noexec:/usr/bin:/bin; do
			rm -f /tmp/ran; env PATH=$p $sh -c "$pre && touch /tmp/ran" 2>/dev/null
			[ ! -e /tmp/ran ] || { echo "ran without the helper: $sh, PATH=$p"; exit 1; }
		done
	done'
check "a hostile value is held verbatim and runs nothing" bash -c \
	'v=$(bash -c "$(cat "$CLAUDE_ENV_FILE") && printf %s \"\$TRICKY\"") && [ "$v" = "it'"'"'s \$(touch /tmp/pwned); \`touch /tmp/pwned\`; '"'"'; touch /tmp/pwned; '"'"'" ] && [ ! -e /tmp/pwned ]'

check "with a broker, the helper gives git the token" bash -c \
	'printf "protocol=https\nhost=github.com\n\n" | git credential fill | grep -qx "password=ghs_FakeTokenForFeatureTests"'
check "with a broker, the shim reaches the real gh with the token, by either path" bash -c \
	'for p in /usr/local/bin/gh /usr/local/drydock/bin/gh; do [ "$(timeout 10 $p auth token)" = ghs_FakeTokenForFeatureTests ] || exit 1; done'
check "with a broker, the probe passes" drydock-probe

# A broker that refuses: the reason the operator can fix gets its sentence,
# and one this copy does not know (a newer server's) is named as it came,
# still exit 69. The control is the working broker above, same clients.
cat >/tmp/refusing-broker.sh <<'BROKER'
#!/bin/sh
read -r l
case $l in
"GET-TOKEN scope=gh") echo "ERR reason=app_permission_missing" ;;
*) echo "ERR reason=some_future_reason" ;;
esac
BROKER
chmod +x /tmp/refusing-broker.sh
socat UNIX-LISTEN:/tmp/refusing-broker.sock,fork,mode=666 EXEC:/tmp/refusing-broker.sh &
for _ in $(seq 50); do [ -S /tmp/refusing-broker.sock ] && break; sleep 0.1; done
check "a missing App permission: the shim says what to check" bash -c \
	'out=$(DRYDOCK_BROKER_SOCK=/tmp/refusing-broker.sock gh api user 2>&1); [ $? -ne 0 ] && [ "$out" = "drydock: GitHub access unavailable (the GitHub App lacks a permission; see the workspace'"'"'s events in Drydock)" ]'
check "an unknown reason: named as it came, exit 69" bash -c \
	'DRYDOCK_BROKER_SOCK=/tmp/refusing-broker.sock drydock-broker GET-TOKEN scope=git 2>/tmp/err; [ $? = 69 ] && [ "$(cat /tmp/err)" = "drydock: GitHub access unavailable (some_future_reason)" ]'

# A scratch repository with a bare "remote", for the hook checks.
scratch() {
	rm -rf /tmp/dd && mkdir -p /tmp/dd && cd /tmp/dd &&
		git init -q --bare remote.git && git clone -q remote.git work 2>/dev/null && cd work &&
		git -c user.name=t -c user.email=t@t commit -q --allow-empty -m init
}
