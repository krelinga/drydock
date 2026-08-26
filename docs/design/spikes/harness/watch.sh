#!/bin/bash
DIR="$1"; DUR_MS=$(( $2 * 1000 ))
LOCK="$DIR/.oauth_refresh.lock"; CRED="$DIR/.credentials.json"
prev_ino="init"; prev_sz="init"
start=$(( $(date +%s%N) / 1000000 ))
while :; do
  now=$(( $(date +%s%N) / 1000000 )); el=$(( now - start ))
  [ $el -gt $DUR_MS ] && break
  if [ -e "$LOCK" ]; then
    ino=$(stat -c '%i' "$LOCK" 2>/dev/null)
    if [ -d "$LOCK" ]; then typ=dir; else typ=file; fi
  else ino=""; typ=""; fi
  if [ "$ino" != "$prev_ino" ]; then
    if [ -n "$ino" ]; then printf '%6d ms  LOCK_ACQUIRED  ino=%s type=%s\n' "$el" "$ino" "$typ"
    elif [ "$prev_ino" != "init" ]; then printf '%6d ms  LOCK_RELEASED\n' "$el"; fi
    prev_ino="$ino"
  fi
  sz=$(stat -c '%s' "$CRED" 2>/dev/null || echo -1)
  if [ "$sz" != "$prev_sz" ]; then
    [ "$prev_sz" != "init" ] && printf '%6d ms  CRED_REWRITTEN size=%s\n' "$el" "$sz"
    prev_sz="$sz"
  fi
  sleep 0.01
done
printf '%6d ms  (watch ended)\n' "$el"
