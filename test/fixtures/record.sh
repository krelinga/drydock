#!/usr/bin/env bash
# Record the fixture corpus from the real Claude Code binary.
#
# Usage: ./record.sh [login|discovery|refusals|hangs|devcontainer|identity|credentials|all]
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
	# Two widths, and the assertion is that BOTH yield a complete URL matched
	# per line: Claude Code writes the URL unbroken at every width (Spike 01,
	# re-measured on 2.1.289), so the narrow capture is a regression guard.
	# Never join lines before matching -- the URL's line is followed by the
	# paste prompt, and joining them corrupts `state` undetectably.
	#
	# The PTY streams into a SCRATCH file, and each named fixture is a copy
	# taken at the moment it is meant to represent. An earlier version
	# captured straight into login-url-1000col and kept recording into it
	# after sending the bad code, so that "URL" fixture silently became a copy
	# of login-invalid-code.
	for w in 1000 80; do
		local raw="$WORK/login-$w.raw"
		local cfg="$WORK/login-$w"
		mkdir -p "$cfg"
		chmod 777 "$cfg"
		pty_capture "$raw" "$w" "$cfg" \
			"exec docker run --rm -it --name ddrec-ct -v '$BIN':/usr/local/bin/claude:ro \
			 -v '$cfg':/cfg -e CLAUDE_CONFIG_DIR=/cfg -e HOME=/root $IMG \
			 /usr/local/bin/claude auth login --claudeai"
		local i
		for i in $(seq 1 30); do grep -aq 'Paste code' "$raw" && break; sleep 2; done
		sleep 1 # let the prompt line finish arriving before the snapshot

		cp "$raw" "$TDIR/login-url-${w}col"
		meta "$TDIR/login-url-${w}col" "claude auth login --claudeai (snapshot at the paste prompt)" "$w" recorded \
			"a complete authorize URL matched per line, and LoginAwaitingCode"
		say "  login-url-${w}col: $(wc -c < "$TDIR/login-url-${w}col") bytes"

		if [ "$w" = 1000 ]; then
			cp "$raw" "$TDIR/login-code-prompt"
			meta "$TDIR/login-code-prompt" "claude auth login --claudeai (snapshot at the paste prompt)" "$w" recorded \
				"the at-paste-prompt state: 'Paste code here if prompted >'"
			# The invalid-code case rides the same session, so it also shows
			# the process staying at the prompt afterwards.
			tmux send-keys -t ddrec 'not-a-real-code-12345' Enter
			for i in $(seq 1 20); do grep -aq 'Invalid code' "$raw" && break; sleep 2; done
			sleep 1
			cp "$raw" "$TDIR/login-invalid-code"
			meta "$TDIR/login-invalid-code" "claude auth login --claudeai, then a malformed code" "$w" recorded \
				"Invalid code, AND still at the prompt -- a wrong code needs no teardown"
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
		meta "$TDIR/login-success-$tag" "HAND-WRITTEN -- run harness-01-login/run.sh login to record for real" - handwritten \
			"a prefix match on 'Login successful' -- never a whole-line match"
	done
	say "  login-success-{plain,period,press}: HAND-WRITTEN, still owed a real recording"
}

# ---------------------------------------------------------------------------
record_discovery() {
	say "== discovery tail (Spike 02) =="
	local cfg="$WORK/disc-cfg" repo="$WORK/disc-repo" raw="$WORK/discovery.raw"
	scratch_repo "$repo"
	mkcfg "$cfg" 1 "$repo"
	pty_capture "$raw" 200 "$repo" \
		"exec env CLAUDE_CONFIG_DIR='$cfg' '$CLAUDE' remote-control --verbose \
		 --spawn worktree --capacity 4 --remote-control-session-name-prefix drydockrec"
	local i
	for i in $(seq 1 40); do
		sed 's/\x1b\[[0-9;]*[A-Za-z]//g' "$raw" | grep -aqE 'Capacity: [1-9]' && break
		sleep 2
	done

	# Stop it cleanly and let the shutdown output land BEFORE copying. An
	# earlier version copied two of these fixtures while the server was still
	# running, so they came out as a 4480-byte pre-shutdown prefix of the
	# third rather than the same bytes, which their .meta claimed.
	local pid
	pid=$(tmux list-panes -t ddrec -F '#{pane_pid}' 2>/dev/null | head -1)
	[ -n "$pid" ] && kill -TERM "$pid" 2>/dev/null
	for i in $(seq 1 15); do kill -0 "$pid" 2>/dev/null || break; sleep 1; done
	sleep 1
	tmux kill-session -t ddrec 2>/dev/null

	# Three identical copies, each named for a different assertion over the
	# same bytes.
	cp "$raw" "$TDIR/env-status-block"
	meta "$TDIR/env-status-block" "claude remote-control --verbose --spawn worktree --capacity 4, then SIGTERM" 200 recorded \
		"the environment id from the 'Environment ID:' header, and Capacity: 1/4 (the last one wins)"
	cp "$raw" "$TDIR/session-url-osc8"
	meta "$TDIR/session-url-osc8" "identical to env-status-block" 200 recorded \
		"the session id, taken only from an OSC 8 hyperlink target -- never from visible text"
	cp "$raw" "$TDIR/status-block-repainted"
	meta "$TDIR/status-block-repainted" "identical to env-status-block" 200 recorded \
		"ONE session from N in-place reprints -- the tail must upsert by id, not append"
	say "  env-status-block (and two identical copies): $(wc -c < "$raw") bytes, $(grep -ac 'Capacity:' "$raw") repaints"

	# A negative fixture: a session_… id that the *model* printed is not a
	# server announcement. Synthetic on purpose -- provoking an agent to say
	# its own id is more fragile than writing the line.
	printf 'I am running in session_01SYNTHETICMODELOUTPUT00 right now.\r\n' \
		> "$TDIR/session-id-in-model-output"
	meta "$TDIR/session-id-in-model-output" "SYNTHETIC -- model prose containing an id" - synthetic \
		"NO session: an id the model printed is not a server announcement"
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
		"already served by a terminal -> a WAIT; retry patiently; must not spend the restart budget. (The file name is historical: 2.1.289 dropped the 409 token, so never match on it.)"
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
# The devcontainer CLI's result (design §6). Not a Claude Code recording, so
# it lives outside the versioned transcript directory and its .meta names the
# CLI and Docker versions instead.
#
# Two rules this step depends on, both learned by getting them wrong:
#   - there is NO `--json` flag. The result is always one JSON object on
#     stdout; logs go to stderr. Passing --json fails, and one fixture keeps
#     that failure as evidence.
#   - every fixture gets its OWN id-label value. The label, not the workspace
#     folder, decides which container `up` returns: reusing a value reattaches
#     to the previous fixture's container and records a false success.
# The label prefix is drydock.test.<run>, never the production key (testing
# §5.4), and every container this creates is removed before the step ends.
record_devcontainer() {
	say "== devcontainer up results (design §6) =="
	need_cmd devcontainer
	local run dir="$HERE/devcontainer"
	run=$(head -c4 /dev/urandom | od -An -tx1 | tr -d ' \n')
	local dcv dkv
	dcv=$(devcontainer --version 2>/dev/null)
	dkv=$(docker info --format '{{.ServerVersion}}' 2>/dev/null)

	# rec <fixture> <label-value> <config-json> <must-yield> [extra up args]
	rec() {
		local name="$1" label="$2" cfg="$3" yield="$4"
		shift 4
		local ws
		ws=$(mktemp -d -t ddfix-XXXXXX)
		mkdir "$ws/.devcontainer"
		printf '%s\n' "$cfg" > "$ws/.devcontainer/devcontainer.json"
		devcontainer up --workspace-folder "$ws" \
			--id-label "drydock.test.$run.workspace=$label" "$@" \
			> "$dir/$name" 2> "$WORK/$name.stderr"
		local rc=$?
		cat > "$dir/$name.meta" <<-META
			devcontainer_version: $dcv
			docker_version:       $dkv (the devcontainer's inner DinD daemon)
			recorded_at:          $(date -u +%Y-%m-%dT%H:%M:%SZ)
			command:              devcontainer up --workspace-folder <mktemp dir> --id-label drydock.test.$run.workspace=$label $*   (stdout only; exit $rc)
			config:               $cfg
			provenance:           recorded
			must_yield:           $yield
		META
		rm -rf "$ws"
		say "  $name: exit $rc, $(wc -c < "$dir/$name") bytes"
	}

	rec up-ok.json ok \
		'{"name":"drydock-fixture","image":"debian:bookworm-slim"}' \
		"ContainerRunning with a containerId and remoteUser"
	rec up-error-image-pull.json imagepull \
		'{"name":"drydock-fixture","image":"drydock-fixture-no-such-image:does-not-exist"}' \
		"ContainerFailed; the description is generic, so the step must come from Drydock"
	rec up-error-config.json config \
		'{"name":"drydock-fixture", "image": ' \
		"ContainerFailed; a truncated config is reported as a missing image, not a parse error"
	rec up-error-postcreate.json postcreate \
		'{"name":"drydock-fixture","image":"debian:bookworm-slim","postCreateCommand":"echo drydock-fixture-postcreate; exit 7"}' \
		"ContainerFailed WITH a containerId -- the container was created and left running"
	# Evidence rather than classifier input: the flag the design used to pass.
	rec up-unknown-argument-json.stdout unknownarg \
		'{"name":"drydock-fixture","image":"debian:bookworm-slim"}' \
		"empty -- there is no --json flag; the matching .stderr ends 'Unknown argument: json'" \
		--json
	cp "$WORK/up-unknown-argument-json.stdout.stderr" "$dir/up-unknown-argument-json.stderr"
	sed -e 's/^must_yield:.*/must_yield:           nothing -- evidence, not classifier input; ends "Unknown argument: json"/' \
		"$dir/up-unknown-argument-json.stdout.meta" > "$dir/up-unknown-argument-json.stderr.meta"

	local left
	docker rm -f $(docker ps -aq --filter "label=drydock.test.$run.workspace") >/dev/null 2>&1
	left=$(docker ps -aq --filter "label=drydock.test.$run.workspace" | wc -l)
	say "  containers left with this run's label: $left"
	[ "$left" = 0 ] || { say "refusing to finish: containers were left behind"; exit 4; }
}
need_cmd() { command -v "$1" >/dev/null || { say "missing: $1"; exit 1; }; }

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
devcontainer) record_devcontainer ;;
identity) record_identity ;;
credentials) record_credentials ;;
all)
	record_credentials
	record_identity
	record_login
	record_discovery
	record_refusals
	record_hangs
	record_devcontainer
	;;
*)
	say "unknown mode: $MODE"
	exit 1
	;;
esac

say ""
say "corpus now holds $(find "$TDIR" "$HERE/authstatus" "$HERE/credentials" "$HERE/devcontainer" -type f ! -name '*.meta' ! -name '*.exit' 2>/dev/null | wc -l) fixtures"
say "still owed a real recording: login-success-{plain,period,press} (needs a human in a browser),"
say "and login-timeout (needs the 5-minute deadline to elapse)"

leak_guard
