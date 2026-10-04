# A disposable "server" for the installer test: Debian with systemd as PID 1,
# Caddy from its official apt repository, Docker, and the devcontainer CLI —
# the setup the installer's instructions point a real operator at.
#
# Docker is Debian's docker.io, which runs its daemon under this container's
# systemd (the container is privileged, so a nested daemon works). Node comes
# from nodejs.org because Debian's is 18 and the devcontainer CLI needs 20 or
# later; npm puts the CLI in /usr/local/bin, on the service's default PATH.
FROM debian:bookworm
ARG NODE_VERSION=22.23.3
ARG DEVCONTAINER_CLI_VERSION=0.89.0
RUN apt-get update && apt-get install -y --no-install-recommends \
      systemd systemd-sysv ca-certificates curl gnupg openssl python3 procps xz-utils \
      docker.io util-linux \
    && curl -fsSL https://dl.cloudsmith.io/public/caddy/stable/gpg.key \
      | gpg --dearmor -o /usr/share/keyrings/caddy-stable-archive-keyring.gpg \
    && curl -fsSL https://dl.cloudsmith.io/public/caddy/stable/debian.deb.txt \
      > /etc/apt/sources.list.d/caddy-stable.list \
    && apt-get update && apt-get install -y --no-install-recommends caddy \
    && rm -rf /var/lib/apt/lists/*
RUN curl -fsSL "https://nodejs.org/dist/v${NODE_VERSION}/node-v${NODE_VERSION}-linux-x64.tar.xz" \
      | tar -xJ -C /usr/local --strip-components=1 \
        --exclude='*/CHANGELOG.md' --exclude='*/LICENSE' --exclude='*/README.md' \
    && npm install -g --no-fund --no-audit "@devcontainers/cli@${DEVCONTAINER_CLI_VERSION}" \
    && devcontainer --version
STOPSIGNAL SIGRTMIN+3
CMD ["/lib/systemd/systemd"]
