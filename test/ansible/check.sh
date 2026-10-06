#!/usr/bin/env bash
# Keeps docs/deploy/first-deployment-ansible.md honest: extracts every ```yaml
# block whose first line is "# file: <path>", assembles them (blocks naming the
# same path are concatenated in document order) into the example layout, and
# runs ansible-playbook --syntax-check and ansible-lint on the result.
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

if [ $# -ge 1 ]; then
	out="$1"
	mkdir -p "$out"
else
	out=$(mktemp -d)
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
echo PASS
