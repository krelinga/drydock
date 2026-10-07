#!/usr/bin/env bash
# Keeps docs/deploy/first-deployment-ansible.md honest: extracts every ```yaml
# block whose first line is "# file: <path>", assembles them (blocks naming the
# same path are concatenated in document order) into the example layout, and
# runs ansible-playbook --syntax-check and ansible-lint on the result, then
# runs §7.1's journal assertion on localhost against healthy and failing
# journals given as data.
#
#   test/ansible/check.sh            # syntax-check, and lint if ansible-lint is installed
#   test/ansible/check.sh OUTDIR     # also leave the assembled layout in OUTDIR
#
# Needs ansible-core (2.15 or later) on PATH; DRYDOCK_REQUIRE_ANSIBLE_LINT=1
# turns a missing ansible-lint into a failure instead of a skip.
set -euo pipefail
here=$(cd "$(dirname "$0")" && pwd)
root=$(cd "$here/../.." && pwd)
doc="$root/docs/deploy/first-deployment-ansible.md"

tmp_out=""
if [ $# -ge 1 ]; then
	out="$1"
	mkdir -p "$out"
else
	out=$(mktemp -d)
	tmp_out=$out
	trap 'rm -rf "$out"' EXIT
fi

# extract DOC OUTDIR: prints each path it writes.
python3 - "$doc" "$out" <<'EOF'
import os, re, sys
doc, out = sys.argv[1], sys.argv[2]
files, block, path = {}, None, None
for line in open(doc, encoding="utf-8"):
    line = line.rstrip("\n")
    if block is None:
        if line == "```yaml":
            block, path = [], None
        continue
    if line == "```":
        if path:
            files.setdefault(path, []).append("\n".join(block) + "\n")
        block = None
        continue
    if not block and path is None:
        m = re.fullmatch(r"# file: (\S+)", line)
        path = m.group(1) if m else ""
        if m:
            continue
    block.append(line)
if not files:
    sys.exit("no '# file:' blocks found in " + doc)
for p, parts in files.items():
    dest = os.path.join(out, p)
    os.makedirs(os.path.dirname(dest), exist_ok=True)
    with open(dest, "w", encoding="utf-8") as f:
        f.write("---\n" + "\n".join(parts))
    print(p)
EOF

cd "$out"
cat >inventory.ini <<'EOF'
[drydock]
drydock-server ansible_host=192.0.2.1
EOF
for f in drydock.crt drydock.key drydock-app.pem; do
	mkdir -p files && : >"files/$f"
done

export ANSIBLE_COLLECTIONS_PATH="$out/.collections" ANSIBLE_NOCOLOR=1
ansible-galaxy collection install -r requirements.yml -p "$out/.collections" >/dev/null </dev/null
echo "--- ansible-playbook --syntax-check"
ansible-playbook -i inventory.ini --syntax-check drydock.yml </dev/null

if command -v ansible-lint >/dev/null; then
	echo "--- ansible-lint"
	# Production is the strictest profile; the assembled files are the whole project.
	ansible-lint --profile production --offline drydock.yml roles group_vars requirements.yml </dev/null
elif [ "${DRYDOCK_REQUIRE_ANSIBLE_LINT:-0}" = 1 ]; then
	echo "ansible-lint is not installed" >&2
	exit 1
else
	echo "--- ansible-lint not installed: skipped"
fi

# The journal assertion (verify.yml, tag drydock_verify_journal) against
# journals given as data, on localhost: the only task the tag selects is the
# assertion, so drydock_journal is the extra var. It is an allowlist, so a
# failure line it was never told about fails it as surely as a listed one —
# #43 was a denylist that let `drydock: broker:` through. Each refusal sits
# beside the healthy journals passing.
echo "--- the journal assertion"
jc=$(mktemp -d)
trap 'rm -rf "$jc" ${tmp_out:+"$tmp_out"}' EXIT
printf -- '- name: The journal assertion alone\n  hosts: localhost\n  connection: local\n  gather_facts: false\n  tasks:\n    - name: Verify\n      ansible.builtin.import_tasks: %s/roles/drydock/tasks/verify.yml\n' "$out" >"$jc/play.yml"
serving='drydock: serving on /run/drydock/http.sock and /run/drydock/preview.sock'
journal_fails=0
journal() { # journal WANT(pass|fail) DESCRIPTION LINE...
	local want="$1" what="$2" got=pass
	shift 2
	python3 -c 'import json, sys; l = sys.argv[1:]; print(json.dumps({"drydock_journal": {"stdout": "\n".join(l), "stdout_lines": l}}))' "$@" >"$jc/vars.json"
	ansible-playbook -i localhost, "$jc/play.yml" --tags drydock_verify_journal -e "@$jc/vars.json" >"$jc/out.log" 2>&1 </dev/null || got=fail
	# A failure must be the assertion's, not the play's.
	if [ "$got" = fail ] && ! grep -q 'FAILED! => {"assertion"' "$jc/out.log"; then
		got="fail outside the assertion"
	fi
	if [ "$got" = "$want" ]; then
		echo "ok   $what: the assertion says $want"
	else
		echo "FAIL $what: the assertion says $got, want $want"
		tail -20 "$jc/out.log"
		journal_fails=$((journal_fails + 1))
	fi
}
journal pass "control: the serving line alone" "$serving"
journal pass "control: with the benign login sweep" "$serving" "drydock: login: removed 2 leftover login container(s)"
journal fail "no serving line" "drydock: login: removed 2 leftover login container(s)"
journal fail "a broker failure" "$serving" "drydock: broker: reopening the sockets: permission denied"
journal fail "a session-server failure" "$serving" "drydock: session servers: x"
journal fail "a catalog refresh failure" "$serving" "drydock: catalog refresh: github: GET /app/installations: 401"
journal fail "a line nobody has listed yet" "$serving" "drydock: something new: went wrong"
journal fail "a panic" "$serving" "panic: runtime error" "goroutine 1 [running]:"
journal fail "a failed login sweep" "$serving" "drydock: login: sweeping leftover login containers: docker: x"
[ "$journal_fails" = 0 ] || exit 1
echo PASS
