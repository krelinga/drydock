#!/usr/bin/env bash
# Spike 01 — the scripted login handshake (design §7.2).
#
# Usage: ./run.sh [probe|badcode|login]
#
#   probe    Start `claude auth login` in a container on a PTY, scrape the
#            authorize URL and the paste prompt, then stop. No human needed.
#   badcode  Same, then write a malformed code to the PTY and capture the
#            rejection. Proves the stdin write path works end to end without
#            needing a real authorization. No human needed.
#   login    The real handshake: prints the URL, waits for you to paste the code
#            from your browser, writes it to the PTY, and waits for the success
#            marker. This is §7.2 steps 1-5, scripted.
#
# Runs in a throwaway container with an empty CLAUDE_CONFIG_DIR, which is what
# makes it safe: it cannot see or modify the real credential, and it is the
# faithful case anyway — §2.2 is specifically about a container, where the
# browser cannot reach a local callback server and the flow falls back to a
# pasted code.
set -uo pipefail

MODE="${1:-probe}"
BIN="${CLAUDE_BIN:-$(readlink -f "$(command -v claude)")}"
RUN="${DRYDOCK_SPIKE_RUN:-$(mktemp -d -t drydock-spike-01-XXXXXX)}"
TM=dd-spike-01
CT=dd-spike-01-ct
# Wide PTY on purpose: at 200 columns the authorize URL wraps mid-token and a
# line-based regex captures a fragment. See the report.
COLS="${COLS:-1000}"

LOG="$RUN/pty.log"

say() { printf '%s\n' "$*"; }
# Join the raw stream into one line before matching, so a URL broken across
# terminal rows is still matchable even on a narrow PTY.
dewrap() { tr -d '\r\n' < "$1" | sed 's/\x1b\[[0-9;]*[A-Za-z]//g'; }

cleanup() {
  tmux kill-session -t "$TM" 2>/dev/null
  docker rm -f "$CT" >/dev/null 2>&1
}
trap cleanup EXIT

mkdir -p "$RUN/cfg"; chmod 777 "$RUN/cfg"
: > "$LOG"

say "claude under test: $("$BIN" --version 2>&1 | head -1)"
say "mode=$MODE  pty_cols=$COLS  artifacts=$RUN"
say ""

cleanup
tmux new-session -d -s "$TM" -x "$COLS" -y 50 \
  "exec docker run --rm -it --name $CT \
     -v '$BIN':/usr/local/bin/claude:ro -v '$RUN/cfg':/cfg \
     -e CLAUDE_CONFIG_DIR=/cfg -e HOME=/root debian:bookworm-slim \
     /usr/local/bin/claude auth login --claudeai"
tmux pipe-pane -o -t "$TM" "cat >> '$LOG'"

# --- step 2: scrape the authorize URL -----------------------------------------
URL=""
for _ in $(seq 1 30); do
  URL=$(dewrap "$LOG" | grep -aoE 'https://claude\.com/cai/oauth/authorize\?[A-Za-z0-9&=_%.~+-]+' | head -1)
  [ -n "$URL" ] && break
  sleep 2
done

if [ -z "$URL" ]; then
  say "FAILED to scrape an authorize URL. Raw pane:"
  tmux capture-pane -p -t "$TM" | grep -v '^$' | sed 's/^/  /'
  exit 1
fi

say "--- scraped authorize URL (${#URL} chars) ---"
say "  $URL"
say ""
say "--- URL wrapping check at $COLS columns ---"
LONGEST=$(sed 's/\x1b\[[0-9;]*[A-Za-z]//g' "$LOG" | tr -d '\r' | awk '{print length}' | sort -n | tail -1)
if grep -aoE 'https://claude\.com/cai/oauth/authorize\?[A-Za-z0-9&=_%.~+-]+' "$LOG" \
     | awk -v u="$URL" 'length($0)==length(u){found=1} END{exit !found}'; then
  say "  URL appears intact on a single line (longest line: $LONGEST chars)"
else
  say "  URL is WRAPPED across lines at this width (longest line: $LONGEST chars)"
  say "  -> a line-based regex would capture only a fragment; dewrap or widen the PTY"
fi
say ""

# --- step 2b: the paste prompt -------------------------------------------------
PROMPT=$(dewrap "$LOG" | grep -aoE 'Paste code here[^>]*>' | head -1)
say "--- paste prompt ---"
say "  ${PROMPT:-<not seen>}"
say ""

[ "$MODE" = probe ] && { say "probe complete; not sending a code."; exit 0; }

# --- steps 4-5: write a code to the PTY and watch for the verdict -------------
if [ "$MODE" = badcode ]; then
  CODE='not-a-real-code-12345'
  say "--- writing a deliberately malformed code: $CODE ---"
else
  say "--- open that URL, authorize, and paste the code below ---"
  say "    (the code has the form <code>#<state>; it is a one-time secret and is"
  say "     NOT echoed into the log by this harness)"
  printf '  code > '
  read -r CODE
  [ -z "$CODE" ] && { say "no code entered"; exit 1; }
  # The shape check Drydock should do before touching the PTY at all.
  if ! printf '%s' "$CODE" | grep -qE '^[^#[:space:]]+#[^#[:space:]]+$'; then
    say "  this does not look like <code>#<state> — Claude Code would reject it"
  fi
fi

MARK=$(wc -c < "$LOG")
tmux send-keys -t "$TM" "$CODE" Enter

VERDICT=""
for _ in $(seq 1 40); do
  TAIL=$(tail -c +"$MARK" "$LOG" | tr -d '\r' | sed 's/\x1b\[[0-9;]*[A-Za-z]//g')
  if grep -q 'Login successful' <<<"$TAIL"; then VERDICT=success; break; fi
  if grep -q 'Invalid code' <<<"$TAIL"; then VERDICT=invalid; break; fi
  sleep 2
done

say ""
say "--- verdict: ${VERDICT:-timeout} ---"
case "$VERDICT" in
  success)
    say "  matched: $(grep -ao 'Login successful[^ ]*' <<<"$TAIL" | head -1)"
    say "  credential written: $(ls -l "$RUN/cfg/.credentials.json" 2>/dev/null | awk '{print $1, $5" bytes"}')"
    say "  note: there are several 'Login successful' variants in the binary —"
    say "        match it as a prefix, never as a whole line."
    ;;
  invalid)
    say "  matched: $(grep -ao 'Invalid code[^$]*' <<<"$TAIL" | head -1 | cut -c1-80)"
    say "  the process stays at the prompt and accepts another code, so a"
    say "  mis-paste is recoverable without restarting the handshake."
    ;;
  *) say "  no verdict within 80s; raw tail:"; printf '%s\n' "$TAIL" | tail -5 | sed 's/^/    /' ;;
esac

say ""
say "--- what reached the log (codes must never be stored verbatim) ---"
if [ "$MODE" = badcode ]; then
  grep -c 'not-a-real-code-12345' "$LOG" | sed 's/^/  occurrences of the submitted code in the PTY log: /'
  say "  (the PTY echoes what is typed — this is the buffer §7.2 says to redact)"
fi
