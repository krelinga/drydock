#!/bin/sh
# Spike 09 — seed-oauth.sh <config-dir> <oauthAccount.json>: merge one
# `oauthAccount` object into <config-dir>/.claude.json, leaving every other key
# as it was. This is the write the split layout would need Drydock to make in
# each container's own CLAUDE_CONFIG_DIR (the one gate the shared credential
# does not carry, Spike 02), so it follows the rule CLAUDE.md sets for any
# write into a .claude.json Claude Code also writes, the same way the
# Feature's drydock-claude-config does:
#
#   * take Claude Code's own lock, "<config-dir>/.claude.json.lock", a
#     directory made with mkdir(2), retrying once a second for up to 60 s, and
#     never steal it (Claude Code takes over a stale lock itself);
#   * under it, re-read the file, merge, write a temporary file beside it and
#     rename(2) it over, mode 0600;
#   * release the lock on every exit.
#
# <oauthAccount.json> holds the object alone (setup.sh extracts it from the
# source login's .claude.json with jq, reading only). A file that is not a JSON
# object is refused, not replaced.
set -eu
dir="${1:?usage: seed-oauth.sh <config-dir> <oauthAccount.json>}"
acct="${2:?usage: seed-oauth.sh <config-dir> <oauthAccount.json>}"
[ -d "$dir" ] || { echo "seed-oauth: $dir is not a directory" >&2; exit 1; }
jq -e 'type == "object" and (.organizationUuid // "") != ""' "$acct" >/dev/null 2>&1 \
  || { echo "seed-oauth: $acct is not an oauthAccount object with an organizationUuid" >&2; exit 1; }
file="$dir/.claude.json"; lock="$file.lock"; tmp="$dir/.claude.json.seed.$$"
umask 077
held=false
cleanup() { rm -f "$tmp"; if $held; then rmdir "$lock" 2>/dev/null || true; fi; }
trap cleanup EXIT
# While the lock is being taken a signal only sets a flag, checked once
# `held` says whether the lock is ours: no signal can land between a mkdir
# that succeeded and held=true, so an interrupted seed never leaves the lock
# behind (and never removes one it does not hold).
sig=
trap 'sig=1' HUP INT TERM
tries=0
while :; do
  if mkdir "$lock" 2>/dev/null; then held=true; break; fi
  if [ ! -d "$lock" ]; then
    # Not held, yet mkdir failed: once more at once (the holder may have
    # released between the two calls); if it still fails with no lock there,
    # mkdir cannot work in this dir, and retrying would never end.
    if mkdir "$lock" 2>/dev/null; then held=true; break; fi
    [ -d "$lock" ] || { echo "seed-oauth: cannot make $lock and no one holds it; refusing" >&2; exit 1; }
  fi
  [ -z "$sig" ] || exit 1
  tries=$((tries + 1))
  [ "$tries" -lt 60 ] || { echo "seed-oauth: $lock held for over a minute; not stealing it" >&2; exit 1; }
  sleep 1
done
[ -z "$sig" ] || exit 1
trap 'exit 1' HUP INT TERM
if [ -e "$file" ]; then
  jq -e 'type == "object"' "$file" >/dev/null 2>&1 || { echo "seed-oauth: $file is not a JSON object; refusing" >&2; exit 1; }
  jq --slurpfile a "$acct" '.oauthAccount = $a[0]' "$file" > "$tmp"
else
  jq -n --slurpfile a "$acct" '{oauthAccount: $a[0]}' > "$tmp"
fi
chmod 0600 "$tmp"
mv -f "$tmp" "$file"
