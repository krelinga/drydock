#!/bin/bash
source dev-container-features-test-lib
source "$(dirname "$0")/_common.sh"

check "commits carry the bot identity" bash -c \
	'[ "$(git config user.email)" = "337849865+krelinga-drydock-dev[bot]@users.noreply.github.com" ] && [ "$(git config user.name)" = "krelinga-drydock-dev[bot]" ]'
check "a push outside drydock/ is refused, and says why" bash -c \
	"$(declare -f scratch); scratch && out=\$(git push -q origin HEAD:main 2>&1); [ \$? -ne 0 ] && echo \"\$out\" | grep -q 'pushes go under refs/heads/drydock/'"
check "a push under drydock/ goes through" bash -c \
	"$(declare -f scratch); scratch && git push -q origin HEAD:drydock/work && git -C /tmp/dd/remote.git rev-parse --verify -q refs/heads/drydock/work"
check "deleting a branch outside drydock/ is refused too" bash -c \
	"$(declare -f scratch); scratch && git --git-dir=/tmp/dd/remote.git branch -q other HEAD 2>/dev/null; ! git push -q origin --delete other 2>/dev/null"
check "the repository's own hooks still run" bash -c \
	"$(declare -f scratch); scratch && printf '#!/bin/sh\ntouch /tmp/dd/own-hook-ran\n' > .git/hooks/pre-commit && chmod +x .git/hooks/pre-commit && git -c user.name=t -c user.email=t@t commit -q --allow-empty -m x && [ -e /tmp/dd/own-hook-ran ]"
check "and its own pre-push sees the refs" bash -c \
	"$(declare -f scratch); scratch && printf '#!/bin/sh\ncat > /tmp/dd/own-pre-push\n' > .git/hooks/pre-push && chmod +x .git/hooks/pre-push && git push -q origin HEAD:drydock/x && grep -q refs/heads/drydock/x /tmp/dd/own-pre-push"

reportResults
