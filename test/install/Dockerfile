# A disposable "server" for the installer test: Debian with systemd as PID 1,
# Caddy from its official .deb (GitHub release), Docker, and the devcontainer CLI —
# the setup the installer's instructions point a real operator at.
#
# Docker is Debian's docker.io, which runs its daemon under this container's
# systemd (the container is privileged, so a nested daemon works). Node comes
# from nodejs.org because Debian's is 18 and the devcontainer CLI needs 20 or
# later; npm puts the CLI in /usr/local/bin, on the service's default PATH.
FROM debian:bookworm
ARG NODE_VERSION=22.23.3
ARG DEVCONTAINER_CLI_VERSION=0.89.0
# No default: lib.sh passes the one pin, from .github/actions/go-suite/install-caddy.sh.
ARG CADDY_VERSION
RUN apt-get update && apt-get install -y --no-install-recommends \
      systemd systemd-sysv ca-certificates curl gnupg openssl python3 procps xz-utils \
      docker.io util-linux \
    && test -n "$CADDY_VERSION" \
    && base="https://github.com/caddyserver/caddy/releases/download/v${CADDY_VERSION}" \
    && deb="caddy_${CADDY_VERSION}_linux_amd64.deb" \
    && (cd /tmp \
        && curl -fsSLO "$base/$deb" -fsSLO "$base/caddy_${CADDY_VERSION}_checksums.txt" \
        && grep " $deb\$" "caddy_${CADDY_VERSION}_checksums.txt" | sha512sum -c - \
        && apt-get install -y --no-install-recommends "./$deb" \
        && rm -f "$deb" "caddy_${CADDY_VERSION}_checksums.txt") \
    && rm -rf /var/lib/apt/lists/*
RUN curl -fsSL "https://nodejs.org/dist/v${NODE_VERSION}/node-v${NODE_VERSION}-linux-x64.tar.xz" \
      | tar -xJ -C /usr/local --strip-components=1 \
        --exclude='*/CHANGELOG.md' --exclude='*/LICENSE' --exclude='*/README.md' \
    && npm install -g --no-fund --no-audit "@devcontainers/cli@${DEVCONTAINER_CLI_VERSION}" \
    && devcontainer --version
STOPSIGNAL SIGRTMIN+3
CMD ["/lib/systemd/systemd"]
