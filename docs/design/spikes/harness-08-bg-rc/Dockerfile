# Spike 08 — a stand-in workspace container. Not the Feature: only what the
# spike needs to run the pinned claude binary (bind-mounted read-only from the
# host, so the version under test is exactly the host's) as uid 1000.
FROM debian:12
RUN apt-get update \
 && apt-get install -y --no-install-recommends ca-certificates git jq procps tmux \
 && rm -rf /var/lib/apt/lists/* \
 && useradd -m -u 1000 -s /bin/bash vscode
