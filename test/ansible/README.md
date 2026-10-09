# test/ansible — checks on the Ansible companion

`docs/deploy/first-deployment-ansible.md` is the installer's Ansible companion. Neither script is
in CI.

- `check.sh` extracts the document's `# file:` YAML blocks, assembles them, and runs
  `ansible-playbook --syntax-check` and `ansible-lint`, then runs §7.1's journal assertion on
  localhost against journals given as data (an allowlist: a line it was never told about fails
  it).
- `live.sh` runs the assembled play against a bare Debian systemd container, installing a locally
  packaged release (needs the internet): first install, a `changed=0` re-run, an upgrade with its
  database backup, an App key rotation, a master-key backup that refuses a different key, and the
  move to a vaulted master key (the installed key vaulted: `changed=0`; a new one with no secret
  stored: replaced; another with one stored: refused, the key unchanged; the same refusal on a
  release change, leaving the old binary running; and the run after an interrupted upgrade,
  backed up by the running process's inode).
