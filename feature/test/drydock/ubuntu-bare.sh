#!/bin/bash
# A base image with no git, no socat and no gh: the Feature brings them.
source dev-container-features-test-lib
source "$(dirname "$0")/_common.sh"

check "git was installed" git --version
check "socat was installed" socat -V
check "no identity was set without the options" bash -c '[ -z "$(git config --system user.email)" ]'

# A remote user that is not vscode, with the CLI's uid update on: it is moved
# to the runner's uid (where that is not 1000), its home with it, while the
# Feature's /home/vscode/.claude keeps ubuntu's build-time uid. The shared
# volume is prepared as Drydock's step 4 prepares it
# (feature/prepare-test-volumes.sh), so it is the runner's uid all the same,
# and the preflight that refused this container before (design §7.1) passes.
check "the remote user is ubuntu, and owns the shared volume" bash -c \
	'[ "$(id -un)" = ubuntu ] && [ "$(stat -c %u "$CLAUDE_CONFIG_DIR")" = "$(id -u)" ] && [ -d "$CLAUDE_CONFIG_DIR/.drydock-volume" ]'

# gh comes from the Feature's own install.sh, from GitHub's apt repository —
# not from a dependsOn Feature, whose lockfile entry would be written into
# every repository's devcontainer-lock.json (design §6, §11). Had any other
# Feature installed gh first, the install would have found it and recorded
# "image".
check "gh was installed by the Feature itself" bash -c \
	'grep -qx GH_INSTALLED_BY=drydock /usr/local/drydock/etc/feature.env && [ "$(dpkg-query -W -f "\${Status}" gh)" = "install ok installed" ] && grep -q "https://cli.github.com/packages" /etc/apt/sources.list.d/github-cli.list'
check "the real gh runs" bash -c '/usr/bin/gh --version | grep -q "^gh version"'

reportResults
