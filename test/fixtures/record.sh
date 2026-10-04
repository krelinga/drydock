#!/usr/bin/env bash
# Record the fixture corpus from the real Claude Code binary.
#
# Usage: ./record.sh [login|discovery|refusals|identity|credentials|all]
#
# This is the tool testing-plan §11.1 step 3 calls for. It exists because a
# corpus nobody can regenerate is worth very little: the whole point of
# recording raw bytes is that a Claude Code bump can be checked by re-recording
# and diffing, and that is only cheap if the recording is one command.
#
# Order matters and the ritual says so: **re-run the four spike harnesses
# first.** They drive the real binary and fail loudly on a behavioural change;
# this script only records whatever comes out. Re-recording first would quietly
# bake a regression into the fixtures and leave the parser tests green.
#
# Every fixture gets a sidecar `.meta` file rather than an inline header. The
# design document says "each with a header", but a header inside a file of raw
# PTY bytes corrupts the thing under test — so the bytes stay pristine and the
# provenance lives beside them.
set -uo pipefail

MODE="${1:-all}"
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
CLAUDE="${CLAUDE_BIN:-$(command -v claude)}"
VERSION="$("$CLAUDE" --version 2>&1 | awk '{print $1}')"
TDIR="$HERE/transcripts/claude-$VERSION"
WORK="$(mktemp -d -t drydock-record-XXXXXX)"
IMG=debian:bookworm-slim
BIN="$(readlink -f "$CLAUDE")"

trap 'tmux kill-session -t ddrec 2>/dev/null; docker rm -f ddrec-ct >/dev/null 2>&1' EXIT

mkdir -p "$TDIR" "$HERE/authstatus" "$HERE/credentials" "$HERE/devcontainer"

say() { printf '%s\n' "$*"; }

# meta <fixture> <command> <width-or-dash> <recorded|handwritten> <what it must yield>
meta() {
	cat > "$1.meta" <<-META
		claude_version: $VERSION
		recorded_at:    $(date -u +%Y-%m-%dT%H:%M:%SZ)
		command:        $2
		pty_width:      $3
		provenance:     $4
		must_yield:     $5
	META
}

# Run a command on a PTY of an exact width and capture the raw output stream.
#
# The width has to be forced in two steps. `new-session -x` is advisory: tmux
# clamps a window to the smallest attached client, so on a server with any
# client attached the requested width is silently ignored — which is how a
# width-dependent fixture ends up recorded at the wrong width and proving
# nothing.
pty_capture() {
	local out="$1" width="$2" cwd="$3"
	shift 3
	: > "$out"
	tmux kill-session -t ddrec 2>/dev/null
	# -c is not optional: without it the pane inherits whatever directory tmux
	# was invoked from, and `remote-control` serves THAT folder — which records
	# a fixture of the wrong repository, or a trust prompt for this one.
	tmux new-session -d -s ddrec -c "$cwd" -x "$width" -y 50 "$@"
	tmux set-option -t ddrec window-size manual >/dev/null
	tmux resize-window -t ddrec -x "$width" -y 50 >/dev/null
	tmux pipe-pane -o -t ddrec "cat >> '$out'"
	local actual
	actual=$(tmux display -t ddrec -p '#{pane_width}')
	[ "$actual" = "$width" ] || say "  WARNING: pane width is $actual, not $width"
}

# A config directory holding exactly the keys a case needs, so a refusal is
# caused by the one thing under test.
mkcfg() { # mkcfg <dir> <with-account:0|1> <trust-path-or-empty>
	local dir="$1" acct="$2" trust="$3"
	mkdir -p "$dir"
	chmod 777 "$dir"
	install -m 666 "$HOME/.claude/.credentials.json" "$dir/.credentials.json"
	local acctjson='{}'
	[ "$acct" = 1 ] && acctjson="$(jq -c '.oauthAccount // {}' "$HOME/.claude/.claude.json")"
	if [ -n "$trust" ]; then
		jq -n --argjson a "$acctjson" --arg p "$trust" \
			'{oauthAccount:$a, remoteDialogSeen:true, projects:{($p):{hasTrustDialogAccepted:true}}}' > "$dir/.claude.json"
	else
		jq -n --argjson a "$acctjson" '{oauthAccount:$a, remoteDialogSeen:true}' > "$dir/.claude.json"
	fi
	chmod 666 "$dir/.claude.json"
}

scratch_repo() {
	local d="$1"
	mkdir -p "$d"
	git -C "$d" init -q .
	git -C "$d" config user.email rec@example.invalid
	git -C "$d" config user.name recorder
	echo recorded > "$d/README.md"
	git -C "$d" add -A
	git -C "$d" commit -qm init
}

# ---------------------------------------------------------------------------
record_login() {
	say "== login handshake (Spike 01) =="
	# Two widths. The assertion is that BOTH yield the complete URL from the
	# raw stream: Spike 04-era measurement showed Claude Code does not wrap
	# the URL itself, so a narrow PTY is a *regression guard*, not a
	# demonstration that de-wrapping is required.
	for w in 1000 80; do
		local name="$TDIR/login-url-${w}col"
		local cfg="$WORK/login-$w"
		mkdir -p "$cfg"
		chmod 777 "$cfg"
		pty_capture "$name" "$w" "$cfg" \
			"exec docker run --rm -it --name ddrec-ct -v '$BIN':/usr/local/bin/claude:ro \
			 -v '$cfg':/cfg -e CLAUDE_CONFIG_DIR=/cfg -e HOME=/root $IMG \
			 /usr/local/bin/claude auth login --claudeai"
		local i
		for i in $(seq 1 30); do grep -aq 'Paste code' "$name" && break; sleep 2; done
		meta "$name" "claude auth login --claudeai" "$w" recorded \
			"the complete authorize URL, matched per-line (no de-wrapping needed)"
		say "  login-url-${w}col: $(wc -c < "$name") bytes"

		# The invalid-code case rides the same session: submit a malformed
		# code and keep recording, which also captures that the process
		# stays at the prompt.
		if [ "$w" = 1000 ]; then
			cp "$name" "$TDIR/login-code-prompt"
			meta "$TDIR/login-code-prompt" "claude auth login --claudeai" "$w" recorded \
				"the at-paste-prompt state: 'Paste code here if prompted >'"
			tmux send-keys -t ddrec 'not-a-real-code-12345' Enter
			for i in $(seq 1 20); do grep -aq 'Invalid code' "$name" && break; sleep 2; done
			cp "$name" "$TDIR/login-invalid-code"
			meta "$TDIR/login-invalid-code" "claude auth login --claudeai, then a malformed code" "$w" recorded \
				"Invalid code, AND still at the prompt — a wrong code needs no teardown"
			say "  login-invalid-code: recorded"
		fi
		tmux kill-session -t ddrec 2>/dev/null
		docker rm -f ddrec-ct >/dev/null 2>&1
	done

	# The three success strings cannot be recorded without a human in a
	# browser, so they are written by hand and the meta says so. Spike 01
	# read them out of the binary; a hand-written fixture asserting a
	# hand-written expectation proves only that the regex matches itself.
	for v in plain:'Login successful' period:'Login successful.' press:'Login successful. Press any key to continue'; do
		local tag="${v%%:*}" text="${v#*:}"
		printf '%s\r\n' "$text" > "$TDIR/login-success-$tag"
		meta "$TDIR/login-success-$tag" "HAND-WRITTEN — run harness-01-login/run.sh login to record for real" - handwritten \
			"a prefix match on 'Login successful' — never a whole-line match"
	done
	say "  login-success-{plain,period,press}: HAND-WRITTEN, still owed a real recording"
}

# ---------------------------------------------------------------------------
record_discovery() {
	say "== discovery tail (Spike 02) =="
	local cfg="$WORK/disc-cfg" repo="$WORK/disc-repo"
	scratch_repo "$repo"
	mkcfg "$cfg" 1 "$repo"
	local out="$TDIR/env-status-block"
	pty_capture "$out" 200 "$repo" \
		"exec env CLAUDE_CONFIG_DIR='$cfg' '$CLAUDE' remote-control --verbose \
		 --spawn worktree --capacity 4 --remote-control-session-name-prefix drydockrec"
	local i
	for i in $(seq 1 40); do
		sed 's/\x1b\[[0-9;]*[A-Za-z]//g' "$out" | grep -aqE 'Capacity: [1-9]' && break
		sleep 2
	done
	meta "$out" "claude remote-control --verbose --spawn worktree --capacity 4" 200 recorded \
		"the environment id, and Capacity: N/4 as the session count"

	# The same stream holds the OSC 8 session URL and the in-place repaints,
	# so they are copies rather than separate runs — the point of each is a
	# different assertion over the same bytes.
	cp "$out" "$TDIR/session-url-osc8"
	meta "$TDIR/session-url-osc8" "as env-status-block" 200 recorded \
		"the session id matched as session_[A-Za-z0-9]+, never parsed out of the URL"
	cp "$out" "$TDIR/status-block-repainted"
	meta "$TDIR/status-block-repainted" "as env-status-block" 200 recorded \
		"ONE row from N in-place reprints — the tail must upsert by id, not append"

	local pid
	pid=$(tmux list-panes -t ddrec -F '#{pane_pid}' 2>/dev/null | head -1)
	[ -n "$pid" ] && kill -TERM "$pid" 2>/dev/null
	sleep 3
	tmux kill-session -t ddrec 2>/dev/null
	say "  env-status-block: $(wc -c < "$out") bytes, $(grep -ac 'Capacity:' "$out") repaints"

	# A negative fixture: a session_… id that the *model* printed is not a
	# server announcement. Synthetic on purpose — provoking an agent to say
	# its own id is more fragile than writing the line.
	printf 'I am running in session_01SYNTHETICMODELOUTPUT00 right now.\r\n' \
		> "$TDIR/session-id-in-model-output"
	meta "$TDIR/session-id-in-model-output" "SYNTHETIC — model prose containing an id" - synthetic \
		"NO session row: an id the model printed is not a server announcement"
	say "  session-id-in-model-output: synthetic negative"
}

# ---------------------------------------------------------------------------
record_refusals() {
	say "== startup refusals (Spike 02) — all four exit 1 =="
	local repo="$WORK/ref-repo"
	scratch_repo "$repo"

	# 1. Workspace not trusted: account present, no trust record.
	mkcfg "$WORK/ref-untrusted" 1 ""
	# 2. No organization: credential present, oauthAccount absent.
	mkcfg "$WORK/ref-noorg" 0 "$repo"
	# 3. Bad command line: -c cannot be combined with --spawn.
	mkcfg "$WORK/ref-badcl" 1 "$repo"

	local specs=(
		"refusal-not-trusted|$WORK/ref-untrusted|--verbose --spawn worktree --capacity 4|Workspace not trusted -> config error, do NOT retry"
		"refusal-no-organization|$WORK/ref-noorg|--verbose --spawn worktree --capacity 4|Unable to determine your organization -> awaiting_login, do NOT retry"
		"refusal-bad-commandline|$WORK/ref-badcl|--verbose -c --spawn worktree --capacity 4|cannot be used with --spawn -> a Drydock bug, do NOT retry"
	)
	local s name cfg args yield
	for s in "${specs[@]}"; do
		IFS='|' read -r name cfg args yield <<< "$s"
		local out="$TDIR/$name"
		( cd "$repo" && CLAUDE_CONFIG_DIR="$cfg" timeout 60 "$CLAUDE" remote-control $args ) \
			> "$out" 2>&1
		printf 'exit_code:      %s\n' "$?" >> "$out.exit"
		meta "$out" "claude remote-control $args" - recorded "$yield"
		say "  $name: exit $(grep -o '[0-9]*$' "$out.exit"), $(wc -c < "$out") bytes"
	done

	# 4. The 409. Needs a session-less server killed hard: with a live
	# session a restart reconnects immediately, so --no-create-session-in-dir
	# is what exposes the window.
	mkcfg "$WORK/ref-409" 1 "$repo"
	local srv="$WORK/409-server.log"
	pty_capture "$srv" 200 "$repo" \
		"exec env CLAUDE_CONFIG_DIR='$WORK/ref-409' '$CLAUDE' remote-control --verbose \
		 --spawn worktree --capacity 4 --no-create-session-in-dir"
	local i
	for i in $(seq 1 30); do grep -aq 'Environment ID' "$srv" && break; sleep 2; done
	local pid
	pid=$(tmux list-panes -t ddrec -F '#{pane_pid}' 2>/dev/null | head -1)
	[ -n "$pid" ] && kill -9 "$pid" 2>/dev/null
	tmux kill-session -t ddrec 2>/dev/null
	sleep 3
	local out="$TDIR/refusal-409"
	( cd "$repo" && CLAUDE_CONFIG_DIR="$WORK/ref-409" timeout 60 "$CLAUDE" remote-control \
		--verbose --spawn worktree --capacity 4 --no-create-session-in-dir ) > "$out" 2>&1
	printf 'exit_code:      %s\n' "$?" >> "$out.exit"
	meta "$out" "claude remote-control after SIGKILL of a session-less server" - recorded \
		"409 / already served -> a WAIT, retry patiently, must not spend the restart budget"
	say "  refusal-409: exit $(grep -o '[0-9]*$' "$out.exit"), $(grep -ac 409 "$out") lines mentioning 409"
}

# ---------------------------------------------------------------------------
# Gates that HANG rather than fail. These are the dangerous ones: a suite with
# no hang fixture passes happily against the exact bug, because there is no
# error string to assert on -- only an absence. Each test here asserts a
# TIMEOUT.
record_hangs() {
	say "== gates that hang on a TTY (2.1.289) =="
	local repo="$WORK/hang-repo"
	scratch_repo "$repo"
	# Account present, consent set, NO trust record. Redirected this exits 1
	# with "Workspace not trusted"; on a PTY it asks "Trust <dir>? [y/N]" and
	# waits. Drydock's supervisor owns a PTY, so the supervisor gets the hang.
	mkcfg "$WORK/hang-untrusted" 1 ""
	local out="$TDIR/hang-untrusted-tty"
	pty_capture "$out" 200 "$repo" \
		"exec env CLAUDE_CONFIG_DIR='$WORK/hang-untrusted' '$CLAUDE' remote-control \
		 --verbose --spawn worktree --capacity 4; echo RC_EXITED=\$?; sleep 300"
	local i
	for i in $(seq 1 20); do grep -aq 'Trust ' "$out" && break; sleep 2; done
	sleep 10
	local verdict=hung
	grep -aq RC_EXITED "$out" && verdict="exited: $(grep -ao 'RC_EXITED=[0-9]*' "$out" | head -1)"
	tmux kill-session -t ddrec 2>/dev/null
	meta "$out" "claude remote-control on a PTY, workspace not trusted" 200 recorded \
		"a TIMEOUT, not a message -- asserts the hang; redirected the same case exits 1"
	say "  hang-untrusted-tty: $verdict, $(wc -c < "$out") bytes"
}

# ---------------------------------------------------------------------------
record_identity() {
	say "== auth status --json (Spike 01) =="
	# A SYNTHETIC account, never the operator's. `auth status` validates
	# nothing against the server, so plausible fake values produce the same
	# output shape -- and whatever goes in here comes back out in the fixture
	# as email/orgId/orgName, in a repository that may be public. An earlier
	# version of this script copied the real record and published it.
	local acct='{"accountUuid":"00000000-0000-0000-0000-000000000000","emailAddress":"fixture@example.invalid","organizationUuid":"11111111-1111-1111-1111-111111111111","organizationName":"fixture-org","subscriptionType":"max"}'
	local future past
	future=$(( ($(date +%s) + 86400 * 30) * 1000 ))
	past=$(( ($(date +%s) - 3600) * 1000 ))

	local cases=(
		"absent||"
		"valid|$future|sk-ant-oat01-FIXTURE-FAKE"
		"expired|$past|sk-ant-oat01-FIXTURE-FAKE"
		"blanked|0|"
	)
	local c name exp tok
	for c in "${cases[@]}"; do
		IFS='|' read -r name exp tok <<< "$c"
		local dir="$WORK/id-$name"
		mkdir -p "$dir"
		chmod 777 "$dir"
		if [ "$name" != absent ]; then
			jq -n --arg t "$tok" --argjson e "$exp" \
				'{claudeAiOauth:{accessToken:$t, refreshToken:$t, expiresAt:$e,
				  scopes:["user:inference","user:profile"], subscriptionType:"max"}}' \
				> "$dir/.credentials.json"
			chmod 666 "$dir/.credentials.json"
		fi
		jq -n --argjson a "$acct" '{oauthAccount:$a}' > "$dir/.claude.json"
		chmod 666 "$dir/.claude.json"

		docker run --rm -v "$BIN":/usr/local/bin/claude:ro -v "$dir":/cfg \
			-e CLAUDE_CONFIG_DIR=/cfg -e HOME=/root "$IMG" \
			/usr/local/bin/claude auth status --json 2>/dev/null \
			> "$HERE/authstatus/$name.json"
		meta "$HERE/authstatus/$name.json" "claude auth status --json" - recorded \
			"$(case $name in
				absent)  echo 'loggedIn:false — indistinguishable from blanked; the FILE separates them';;
				valid)   echo 'loggedIn:true';;
				expired) echo 'loggedIn:TRUE — auth status ignores expiry; the countdown comes from the file';;
				blanked) echo 'loggedIn:false — every workspace just died; different words from absent';;
			esac)"
		say "  authstatus/$name.json: $(jq -c '{loggedIn,authMethod}' "$HERE/authstatus/$name.json" 2>/dev/null)"
	done
}

# ---------------------------------------------------------------------------
record_credentials() {
	say "== .credentials.json shapes (synthetic by nature — they are inputs) =="
	local now future soon past
	now=$(date +%s)
	future=$(( (now + 86400 * 30) * 1000 ))
	soon=$(( (now + 86400 * 2) * 1000 ))
	past=$(( (now - 3600) * 1000 ))
	local tok=sk-ant-oat01-FIXTURE-FAKE
	local rtok=sk-ant-ort01-FIXTURE-FAKE

	emit() { # emit <name> <json> <yield>
		# Pretty-print when it parses; write raw when it does not, because the
		# corrupt fixture exists precisely to be unparseable.
		if printf '%s' "$2" | jq . > "$HERE/credentials/$1.json" 2>/dev/null; then :; else
			printf '%s' "$2" > "$HERE/credentials/$1.json"
		fi
		meta "$HERE/credentials/$1.json" "SYNTHETIC — a classifier input, not a recording" - synthetic "$3"
		say "  credentials/$1.json"
	}
	emit ok "$(jq -nc --arg t "$tok" --arg r "$rtok" --argjson e "$future" \
		'{claudeAiOauth:{accessToken:$t,refreshToken:$r,expiresAt:$e,scopes:["user:inference"],subscriptionType:"max"}}')" \
		"ok"
	emit expiring "$(jq -nc --arg t "$tok" --arg r "$rtok" --argjson e "$soon" \
		'{claudeAiOauth:{accessToken:$t,refreshToken:$r,expiresAt:$e,scopes:["user:inference"],subscriptionType:"max"}}')" \
		"expiring — inside the three-day window, still a countdown the operator may ignore"
	emit expired "$(jq -nc --arg t "$tok" --arg r "$rtok" --argjson e "$past" \
		'{claudeAiOauth:{accessToken:$t,refreshToken:$r,expiresAt:$e,scopes:["user:inference"],subscriptionType:"max"}}')" \
		"expired — and note auth status still says loggedIn:true for this one"
	emit blanked "$(jq -nc \
		'{claudeAiOauth:{accessToken:"",refreshToken:"",expiresAt:0,scopes:["user:inference"],subscriptionType:"max"}}')" \
		"blanked — the Spike 00 tombstone; EVERY container on the volume just lost auth"
	emit corrupt '{"claudeAiOauth":{"accessToken":' \
		"a parse error, surfaced as such — never silently treated as absent"
	# "absent" is the absence of a file, so it is recorded as a marker rather
	# than a document; a classifier takes nil for this case.
	printf 'This case is the FILE NOT EXISTING. Pass nil to ClassifyIdentity.\n' \
		> "$HERE/credentials/absent.txt"
	meta "$HERE/credentials/absent.txt" "SYNTHETIC — marker for the no-file case" - synthetic \
		"absent — nobody has ever signed in; the expected first-run state"
	say "  credentials/absent.txt (marker)"
}

# ---------------------------------------------------------------------------
# Leak guard. The PTY recordings need a REAL login (remote-control checks
# eligibility server-side), so real credential and account values sit in this
# run's work directory -- and anything Claude Code echoes could carry one into a
# fixture. This repository is public. So before declaring success, scan the
# whole corpus for every identifying value in the operator's own config and
# refuse to finish if one is found. An earlier run published an email and org
# id this way; the guard is what makes that a build failure instead of a commit.
leak_guard() {
	local cfg="$HOME/.claude" v found=0
	local -a values=()
	while IFS= read -r v; do [ ${#v} -ge 8 ] && values+=("$v"); done < <(
		jq -r '.claudeAiOauth | .accessToken, .refreshToken' "$cfg/.credentials.json" 2>/dev/null
		jq -r '.oauthAccount | .emailAddress, .organizationUuid, .accountUuid, .organizationName' \
			"$cfg/.claude.json" 2>/dev/null
		jq -r '.userID // empty' "$cfg/.claude.json" 2>/dev/null
	)
	for v in "${values[@]}"; do
		if grep -rlF --exclude=record.sh -- "$v" "$HERE" >/dev/null 2>&1; then
			say "LEAK: an identifying value from $cfg appears in:"
			grep -rlF --exclude=record.sh -- "$v" "$HERE" | sed 's/^/  /'
			found=1
		fi
	done
	if [ "$found" = 1 ]; then
		say "refusing to finish: scrub or re-record the files above before committing"
		exit 3
	fi
	say "leak guard: no identifying value from $cfg in the corpus (${#values[@]} checked)"
}

# ---------------------------------------------------------------------------
say "claude under test: $VERSION"
say "corpus: $TDIR"
say "work:   $WORK"
say ""

case "$MODE" in
login) record_login ;;
discovery) record_discovery ;;
refusals) record_refusals ;;
hangs) record_hangs ;;
identity) record_identity ;;
credentials) record_credentials ;;
all)
	record_credentials
	record_identity
	record_login
	record_discovery
	record_refusals
	record_hangs
	;;
*)
	say "unknown mode: $MODE"
	exit 1
	;;
esac

say ""
say "recorded $(find "$TDIR" "$HERE/authstatus" "$HERE/credentials" -type f ! -name '*.meta' ! -name '*.exit' 2>/dev/null | wc -l) fixtures"
say "still owed a real recording: login-success-{plain,period,press} (needs a human in a browser),"
say "login-timeout (needs the 5-minute deadline to elapse), devcontainer/ (needs a real build)"

leak_guard
