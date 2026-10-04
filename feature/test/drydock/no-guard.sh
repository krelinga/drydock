#!/bin/bash
# branchPrefix "" turns the guard off — and the hooks still pass through.
source dev-container-features-test-lib
source "$(dirname "$0")/_common.sh"

check "with no prefix, a push to main goes through" bash -c \
	"$(declare -f scratch); scratch && git push -q origin HEAD:main"

reportResults
