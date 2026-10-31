# Spike 09 — a stand-in workspace container. Not the Feature: only what the
# spike needs to run the pinned claude binary (bind-mounted read-only from the
# host, so the version under test is exactly the host's) as uid 1000. The two
# mount points exist, owned by vscode, so an empty named volume mounted over
# one takes that owner (h2.sh's volumes; run.sh bind-mounts host dirs).
FROM debian:12
RUN apt-get update \
 && apt-get install -y --no-install-recommends ca-certificates git jq procps tmux \
 && rm -rf /var/lib/apt/lists/* \
 && useradd -m -u 1000 -s /bin/bash vscode \
 && install -d -o vscode -g vscode -m 700 /home/vscode/.claude /home/vscode/.claude-credentials
