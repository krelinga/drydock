#!/bin/bash
# A base image with no git, no socat and no gh: the Feature brings them.
source dev-container-features-test-lib
source "$(dirname "$0")/_common.sh"

check "git was installed" git --version
check "socat was installed" socat -V
check "no identity was set without the options" bash -c '[ -z "$(git config --system user.email)" ]'

reportResults
