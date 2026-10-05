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

# A scratch repository with a bare "remote", for the hook checks.
scratch() {
	rm -rf /tmp/dd && mkdir -p /tmp/dd && cd /tmp/dd &&
		git init -q --bare remote.git && git clone -q remote.git work 2>/dev/null && cd work &&
		git -c user.name=t -c user.email=t@t commit -q --allow-empty -m init
}
