# A bare Debian "server" for the Ansible companion's live run (live.sh):
# systemd as PID 1, and only what Ansible itself needs (python3, sudo). Unlike
# test/install/Dockerfile it has no Docker, Node, devcontainer CLI or Caddy:
# installing those is the playbook's job (runbook §3).
FROM debian:bookworm
RUN apt-get update && apt-get install -y --no-install-recommends \
      systemd systemd-sysv dbus python3 sudo \
    && rm -rf /var/lib/apt/lists/*
STOPSIGNAL SIGRTMIN+3
CMD ["/lib/systemd/systemd"]
