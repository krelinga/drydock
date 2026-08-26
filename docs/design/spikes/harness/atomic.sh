#!/bin/bash
# Experiment 2: is the credential write atomic (temp+rename) or in-place (truncate)?
# Decisive signal: inode identity across a write. rename() => new inode => readers never see a torn file.
DIR="$1"; DUR_MS=$(( $2 * 1000 ))
CRED="$DIR/.credentials.json"
prev="init"; torn=0; reads=0
start=$(( $(date +%s%N) / 1000000 ))
while :; do
  now=$(( $(date +%s%N) / 1000000 )); el=$(( now - start ))
  [ $el -gt $DUR_MS ] && break
  line=$(stat -c '%i:%s' "$CRED" 2>/dev/null || echo "ABSENT")
  # hammer the file with reads and check every one parses as complete JSON
  if [ "$line" != "ABSENT" ]; then
    reads=$((reads+1))
    content=$(cat "$CRED" 2>/dev/null)
    case "$content" in
      *'}}'*) : ;;                       # complete
      "") torn=$((torn+1)); printf '%6d ms  EMPTY_READ\n' "$el" ;;
      *)   torn=$((torn+1)); printf '%6d ms  TORN_READ len=%s\n' "$el" "${#content}" ;;
    esac
  fi
  if [ "$line" != "$prev" ]; then
    [ "$prev" != "init" ] && printf '%6d ms  CRED_CHANGED %s -> %s\n' "$el" "$prev" "$line"
    prev="$line"
  fi
done
printf 'TOTALS reads=%s torn_or_empty=%s\n' "$reads" "$torn"
