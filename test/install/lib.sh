# Sourced by run.sh and live.sh: the throwaway "server" both test against.

# The VS Code credential helper dies with its window; the base image is public.
if [ -z "${DOCKER_CONFIG:-}" ]; then
	DOCKER_CONFIG=$(mktemp -d)
	export DOCKER_CONFIG
	echo '{}' >"$DOCKER_CONFIG/config.json"
fi

# start_server NAME: a privileged container with systemd as PID 1 and Caddy
# from its official package, with the test CA and certificates from certs.sh
# under /etc/ssl/drydock. Returns once systemd is up.
start_server() {
	local name="$1" here state
	here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
	docker build -q -t drydock-install-test \
		--build-arg "CADDY_VERSION=$(sed -n 's/^CADDY_VERSION=//p' "$here/../../.github/actions/go-suite/install-caddy.sh")" "$here" >/dev/null
	docker run -d --name "$name" --privileged --cgroupns=host \
		-v /sys/fs/cgroup:/sys/fs/cgroup:rw --tmpfs /run --tmpfs /run/lock \
		drydock-install-test >/dev/null
	for _ in $(seq 1 100); do
		state=$(docker exec "$name" systemctl is-system-running 2>/dev/null || true)
		case "$state" in running | degraded) break ;; esac
		sleep 0.2
	done
	docker cp "$here/certs.sh" "$name:/certs.sh"
	docker exec "$name" bash /certs.sh
}

stop_server() {
	if [ "${KEEP:-0}" = 1 ]; then
		echo "container left running: docker exec -it $1 bash"
	else
		docker rm -f "$1" >/dev/null 2>&1 || true
	fi
}
