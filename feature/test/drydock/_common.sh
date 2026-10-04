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

# A stand-in broker, so the clients get past the token fetch to what they do
# with it. Without one, a gh shim that execs itself forever still "fails
# fast" at the fetch, and the loop check below would be vacuous.
socat UNIX-LISTEN:/tmp/fake-broker.sock,fork,mode=666 \
	SYSTEM:'read l; echo "OK token=ghs_FakeTokenForFeatureTests expires_at=2030-01-01T00:00:00Z"' &
for _ in $(seq 50); do [ -S /tmp/fake-broker.sock ] && break; sleep 0.1; done
export DRYDOCK_BROKER_SOCK=/tmp/fake-broker.sock

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
