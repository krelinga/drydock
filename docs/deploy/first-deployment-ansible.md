# First deployment with Ansible

The Ansible companion to [the first-deployment runbook](first-deployment.md). It does the runbook's
**server-side** steps as copy-pastable tasks, numbered the same way, and links back to the runbook
for the *why* and for everything that cannot be automated: choosing a certificate, DNS, the GitHub
App's settings, and the checks you do from a laptop and a phone.

Read the runbook's [Known issues](first-deployment.md#0-known-issues--read-these-first) and
[step 1](first-deployment.md#1-before-you-start) first. This document assumes you have done
step 1 by hand and have the files it produces on your **controller** (the machine you run
`ansible-playbook` on).

> [!IMPORTANT]
> **The installer, `deploy/install.sh`, is the source of truth.** These tasks prepare the host the
> way the runbook does, run the installer with the flags the runbook gives it, and then check what
> the runbook checks. They never write a file the installer owns (the unit, the Caddyfile,
> `/etc/drydock/*`), so an upgrade of the installer cannot be undone by a stale task here. When the
> installer changes, this document and the runbook change with it.

Every YAML block below whose first line is `# file: <path>` is one file of the example layout, or
a part of one. `test/ansible/check.sh` extracts exactly those blocks, assembles them, and runs
`ansible-playbook --syntax-check` and `ansible-lint` (production profile) on the result, so the
blocks are the tested copy. `test/ansible/live.sh` runs that assembled play against a bare Debian
container with systemd: a first install, a re-run that reports `changed=0`, an upgrade, an App key
rotation, and the move to a vaulted secrets master key. Paste the blocks as they are, then change
the variables.

- [Requirements and layout](#requirements-and-layout)
- [Variables](#variables)
- [3. Prerequisites on the server](#3-prerequisites-on-the-server)
- [4. Get the secrets onto the server without leaking them](#4-get-the-secrets-onto-the-server-without-leaking-them)
- [5. Install](#5-install)
- [6. The secrets master key: supply it, or back it up](#6-the-secrets-master-key-supply-it-or-back-it-up)
- [7. Verify](#7-verify)
- [8–9. First workspace, and what does not work yet](#89-first-workspace-and-what-does-not-work-yet)
- [10. Upgrade, roll back, uninstall](#10-upgrade-roll-back-uninstall)
- [What this does not automate](#what-this-does-not-automate)

---

## Requirements and layout

- **ansible-core 2.15 or later** on the controller (`ansible.builtin.deb822_repository` and
  `ansible.builtin.systemd_service` arrived in 2.15). Everything is `ansible.builtin.*` except the
  optional firewall task, which uses **`community.general.ufw`**.
- **The server** is what [runbook §1.1](first-deployment.md#11-the-server) says: Debian 12/13 or
  Ubuntu 22.04/24.04 on amd64, with systemd. Ansible reaches it over SSH as a user that can `sudo`.
  Unlike the runbook, nothing here needs a terminal: the password is set without one
  ([step 5](#5-install)).

```text
drydock-ansible/
├── ansible.cfg
├── inventory.ini
├── requirements.yml
├── drydock.yml                      # the playbook
├── group_vars/drydock/
│   ├── vars.yml                     # settings: plain text
│   └── vault.yml                    # the operator password: ansible-vault encrypted
├── files/                           # from runbook step 1; keep this directory out of git
│   ├── drydock-app.pem              # the GitHub App key: ansible-vault encrypted
│   ├── drydock.crt                  # the certificate, full chain
│   ├── drydock.key                  # its private key: ansible-vault encrypted
│   ├── drydock-ca.pem               # option A only: the CA's certificate, never its key
│   └── drydock-secrets.key          # optional: the secrets master key, ansible-vault encrypted
└── roles/drydock/
    ├── defaults/main.yml
    ├── handlers/main.yml
    └── tasks/
        ├── main.yml
        ├── prerequisites.yml        # §3
        ├── firewall.yml             # §3.4, optional
        ├── tls.yml                  # §4.2
        ├── install.yml              # §4.1 + §4.3 + §5 (+ the §10.1 database backup)
        ├── backup_master_key.yml    # §6, only without drydock_secrets_key_src
        └── verify.yml               # §7.1
```

`pipelining` matters here, not only for speed: without it Ansible writes each module's arguments to
a temporary file on the server before running it, and one task's arguments carry the operator
password (as `stdin`, [step 5](#5-install)). With it they go over the SSH channel only.

```ini
# ansible.cfg
[defaults]
inventory = inventory.ini

[ssh_connection]
pipelining = True
```

```ini
# inventory.ini
[drydock]
drydock-server ansible_host=192.168.1.20 ansible_user=you
```

```yaml
# file: requirements.yml
# Only for the optional firewall task (§3.4):
#   ansible-galaxy collection install -r requirements.yml
collections:
  - name: community.general
```

```yaml
# file: drydock.yml
- name: Deploy Drydock (docs/deploy/first-deployment-ansible.md)
  hosts: drydock
  become: true
  roles:
    - drydock
```

Run it with:

```sh
ansible-galaxy collection install -r requirements.yml   # once, for the firewall task
ansible-playbook drydock.yml --ask-become-pass --ask-vault-pass
```

```yaml
# file: roles/drydock/tasks/main.yml
- name: Prerequisites (runbook §3)
  ansible.builtin.import_tasks: prerequisites.yml
  tags: [drydock_prerequisites]

- name: Firewall (runbook §3.4, optional)
  ansible.builtin.import_tasks: firewall.yml
  when: drydock_firewall_lan_cidr | length > 0
  tags: [drydock_firewall]

- name: TLS certificate and key (runbook §4.2)
  ansible.builtin.import_tasks: tls.yml
  tags: [drydock_tls]

- name: Install or upgrade (runbook §4.1, §5, §10.1)
  ansible.builtin.import_tasks: install.yml
  tags: [drydock_install]

# A generated key only: with drydock_secrets_key_src, the vault already holds it.
- name: Back up the secrets master key (runbook §6.2)
  ansible.builtin.import_tasks: backup_master_key.yml
  when: drydock_secrets_key_src | length == 0
  tags: [drydock_backup]

- name: Verify (runbook §7.1)
  ansible.builtin.import_tasks: verify.yml
  tags: [drydock_verify]
```

```yaml
# file: roles/drydock/handlers/main.yml
# Caddy reads the certificate when it loads its config and does not watch the
# files (runbook §1.3): a renewed certificate takes a reload.
- name: Reload caddy
  ansible.builtin.systemd_service:
    name: caddy
    state: reloaded
```

---

## Variables

Set these in `group_vars/drydock/vars.yml`. **`drydock_version` is pinned**, not "latest", so a
re-run installs the same release until you change it. Use a release that is *published*
([runbook step 2](first-deployment.md#2-cut-a-release)): a draft's assets do not download, and the
download task fails rather than installing something else. The pin below is v0.10.0, the first
release whose installer takes `--vscode-ssh-host` and `--no-vscode-ssh-host`, one of which the play
always passes (an older installer stops at either with `unknown option`); if
`gh release view v0.10.0 --repo krelinga/drydock` says `release not found`, it has not been cut yet
([runbook step 2](first-deployment.md#2-cut-a-release)). `drydock_secrets_key_src` needs
**v0.3.0 or later**: an older installer stops at `--secrets-key` with `unknown option`. Use
**v0.4.2 or later** with it on an upgrade: an earlier installer that refuses the key has already
replaced the binary ([below](#5-install)).

```yaml
# file: group_vars/drydock/vars.yml
drydock_version: v0.10.0
# Lowercase, fully qualified, with a dot (runbook §1.2).
drydock_ui_host: drydock.example.com

# Controller paths of the files from runbook §1.3 and §1.4.
drydock_cert_src: "{{ playbook_dir }}/files/drydock.crt"
drydock_key_src: "{{ playbook_dir }}/files/drydock.key"
# Option A (a private CA) only: the CA's certificate. Leave empty for options B and C.
drydock_ca_cert_src: ""
# drydock_ca_cert_src: "{{ playbook_dir }}/files/drydock-ca.pem"

# The production App, krelinga-drydock. Never the dev App (runbook §1.4, Known issue 3).
drydock_app_id: 5189455
drydock_app_key_src: "{{ playbook_dir }}/files/drydock-app.pem"

# From the vault (below). Only ever used when no password is set yet.
drydock_operator_password: "{{ vault_drydock_operator_password }}"

# The secrets master key (runbook §4.3, §6). Your own key: a controller path
# to a vault-encrypted file of exactly 32 raw bytes, which the installer puts
# in place, and no backup step is needed. Empty: the installer generates the
# key, and step 6 fetches a backup of it to the path below.
drydock_secrets_key_src: ""
# drydock_secrets_key_src: "{{ playbook_dir }}/files/drydock-secrets.key"
drydock_secrets_key_backup: "{{ lookup('ansible.builtin.env', 'HOME') }}/drydock-backup/secrets.key"

# Optional: allow 443 from this network with ufw (§3.4). Empty: leave the firewall alone.
drydock_firewall_lan_cidr: ""

# Optional: previews (runbook §8.7, §8.8). A second registrable
# domain, lowercase, with a LAN wildcard record pointing at this server, and
# controller paths of its wildcard certificate (full chain) and key. All three
# or none. Empty: no preview site, and the installer is told --no-preview.
drydock_preview_domain: ""
# drydock_preview_domain: drydock-preview.example
# drydock_preview_cert_src: "{{ playbook_dir }}/files/preview.crt"
# drydock_preview_key_src: "{{ playbook_dir }}/files/preview.key"

# Optional: the Open in VS Code link (runbook §8.9). The address your VS Code's
# Remote-SSH reaches this server at, [user@]host[:port] or an ssh config alias;
# that user needs Docker access on the server. Empty: no link, and the
# installer is told --no-vscode-ssh-host.
drydock_vscode_ssh_host: ""
# drydock_vscode_ssh_host: owner@devbox.lan
```

**The operator password and the private keys go in ansible-vault.** The password is a
variable; the keys are files, and `ansible.builtin.copy` decrypts a vault-encrypted `src` on the
way, so they never sit on the controller in plain text:

```sh
ansible-vault create group_vars/drydock/vault.yml          # holds the variable below
ansible-vault encrypt files/drydock-app.pem files/drydock.key
ansible-vault encrypt files/preview.key                     # only with previews (§4.2)
```

The secrets master key, if you supply it ([§4.3](#43-optional-your-own-secrets-master-key)), is
encrypted the same way. It is 32 **raw** bytes, not text, and that is fine: `ansible-vault encrypt`
and `decrypt --output`, `copy`, and the checksum the play takes all keep it byte for byte (checked
on ansible-core 2.15 and 2.21). **Never `ansible-vault view` or `edit` it**: `view` replaces every
byte that is not UTF-8 with `?` on the way to the terminal, and an editor would rewrite it.

```yaml
# file: group_vars/drydock/vault.yml
# Encrypt this file: ansible-vault encrypt group_vars/drydock/vault.yml
# At least 12 characters, one line (runbook §1.5).
vault_drydock_operator_password: "replace me: twelve characters or more"
```

Everything else has a default. You should not need to change these: the paths are the runbook's,
and the versions are the ones CI pins.

```yaml
# file: roles/drydock/defaults/main.yml
drydock_ca_cert_src: ""
drydock_firewall_lan_cidr: ""
drydock_take_over_caddy: false   # true only for runbook §3.3's "Caddy already serves other sites"
drydock_require_clock_sync: true

# Where releases come from. The tarball and SHA256SUMS are fetched from here.
drydock_release_base_url: "https://github.com/krelinga/drydock/releases/download/{{ drydock_version }}"
drydock_release_dir: "/var/cache/drydock-release/{{ drydock_version }}"

# The service's PATH, exactly as the installer writes it into the unit (runbook §3.2).
drydock_service_path: /usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin
drydock_devcontainer_cli_version: "0.89.0"
drydock_node_major: 22
# Caddy comes from its GitHub release, as CI installs it (see §3); keep in step with
# CADDY_VERSION in .github/actions/go-suite/install-caddy.sh.
drydock_caddy_version: "2.11.7"
# Docker's repository is per distribution. On a derivative (Mint, Pop!_OS),
# set these to the parent distribution's name and codename (runbook §3.1).
drydock_docker_distro: "{{ ansible_facts['distribution'] | lower }}"
drydock_docker_codename: "{{ ansible_facts['distribution_release'] }}"

# Server paths: the runbook's convention (§4.2).
drydock_cert_dir: /etc/caddy/certs
drydock_cert_path: /etc/caddy/certs/drydock.crt
drydock_key_path: /etc/caddy/certs/drydock.key
drydock_ca_cert_path: /etc/caddy/certs/drydock-ca.pem
# Where the App key waits for the installer, for one task (§4.1).
drydock_app_key_stage: /root/drydock-app.pem
# Your own secrets master key, if any, and where it waits for the installer (§4.3).
drydock_secrets_key_src: ""
drydock_secrets_key_stage: /root/drydock-secrets.key
# Previews, off unless drydock_preview_domain is set (runbook §8.7).
drydock_preview_domain: ""
drydock_preview_cert_src: ""
drydock_preview_key_src: ""
drydock_preview_cert_path: /etc/caddy/certs/preview.crt
drydock_preview_key_path: /etc/caddy/certs/preview.key
# The Open in VS Code link, off unless drydock_vscode_ssh_host is set (runbook §8.9).
drydock_vscode_ssh_host: ""
```

---

## 3. Prerequisites on the server

[Runbook §3](first-deployment.md#3-prerequisites-on-the-server). The installer checks for all of
these and installs none of them. The same sources as the runbook, as deb822 `.sources` files with
their signing keys, rather than the runbook's `.list` files and `apt_key`-era keyrings.

> [!NOTE]
> **If you already followed the runbook by hand on this server,** its `docker.list` and
> `caddy-stable.list` describe the same repositories with a different `Signed-By`, and apt refuses
> the pair (`Conflicting values set for option Signed-By`). The task that removes the runbook's
> hand-written source lists deletes them (Caddy's goes even earlier, before the first cache refresh). A host
> with **Debian's `docker.io`** installed (which the installer also accepts) gets it replaced by
> Docker's packages; remove `docker-ce*` from the list if you would rather keep `docker.io`.

```yaml
# file: roles/drydock/tasks/prerequisites.yml
- name: Check the host is one the release and these tasks support (runbook §1.1)
  ansible.builtin.assert:
    that:
      - ansible_facts['architecture'] == 'x86_64'
      - ansible_facts['os_family'] == 'Debian'
      - ansible_facts['service_mgr'] == 'systemd'
    fail_msg: Releases are amd64 only, the installer needs systemd, and these tasks use apt.

- name: Check the clock is synchronized (GitHub refuses a JWT from a drifted clock, runbook §1.1)
  ansible.builtin.command: timedatectl show --property=NTPSynchronized --value
  register: drydock_clock
  changed_when: false
  failed_when: drydock_require_clock_sync | bool and drydock_clock.stdout != 'yes'

# Caddy's apt repository on dl.cloudsmith.io answers 402 Payment Required, so Caddy is installed
# from its GitHub release instead (below). A file the repository task of an earlier version of
# this play left behind would fail the cache refresh in the next task, so it goes first. This uses
# `file`, not `deb822_repository`, which needs the python3-debian that the next task installs.
- name: Remove Caddy's apt repository, which answers 402
  ansible.builtin.file:
    path: "{{ item }}"
    state: absent
  loop:
    - /etc/apt/sources.list.d/caddy-stable.sources
    - /etc/apt/sources.list.d/caddy-stable.list
    - /etc/apt/keyrings/caddy-stable.asc

- name: Install the base packages (runbook §3)
  ansible.builtin.apt:
    name: [ca-certificates, curl, gnupg, openssl, iproute2, python3-debian]
    update_cache: true
    cache_valid_time: 3600

- name: Remove the runbook's hand-written source lists, which would conflict with the files below
  ansible.builtin.file:
    path: "/etc/apt/sources.list.d/{{ item }}"
    state: absent
  loop: [docker.list, caddy-stable.list, nodesource.list]

- name: Add Docker's apt repository (runbook §3.1)
  ansible.builtin.deb822_repository:
    name: docker
    types: deb
    uris: "https://download.docker.com/linux/{{ drydock_docker_distro }}"
    suites: "{{ drydock_docker_codename }}"
    components: stable
    architectures: amd64
    signed_by: "https://download.docker.com/linux/{{ drydock_docker_distro }}/gpg"
  register: drydock_repo_docker

- name: Add NodeSource's apt repository (runbook §3.2; the distributions' nodejs is too old)
  ansible.builtin.deb822_repository:
    name: nodesource
    types: deb
    uris: "https://deb.nodesource.com/node_{{ drydock_node_major }}.x"
    suites: nodistro
    components: main
    architectures: amd64
    signed_by: https://deb.nodesource.com/gpgkey/nodesource-repo.gpg.key
  register: drydock_repo_nodesource

- name: Prefer NodeSource's nodejs over the distribution's, as its setup script does
  ansible.builtin.copy:
    dest: /etc/apt/preferences.d/nodejs
    content: |
      Package: nodejs
      Pin: origin deb.nodesource.com
      Pin-Priority: 600
    owner: root
    group: root
    mode: "0644"

- name: Refresh the package lists when a repository was added
  ansible.builtin.apt:
    update_cache: true
  when: drydock_repo_docker is changed or drydock_repo_nodesource is changed

- name: Install Docker Engine and Node.js (runbook §3.1–3.2)
  ansible.builtin.apt:
    name: [docker-ce, docker-ce-cli, containerd.io, docker-buildx-plugin, nodejs]
    state: present

# The official package (systemd unit, caddy user, /etc/caddy) from Caddy's GitHub release, checked
# against that release's checksums: the same package CI installs (test/install/Dockerfile).
# Reinstalls only when the installed version differs; to upgrade, change drydock_caddy_version.
- name: Install Caddy from its GitHub release (runbook §3.3)
  ansible.builtin.shell:
    cmd: |
      set -euo pipefail
      if [ "$(dpkg-query -W -f='${Status} ${Version}' caddy 2>/dev/null)" = "install ok installed {{ drydock_caddy_version }}" ]; then
        echo unchanged; exit 0
      fi
      dir=$(mktemp -d); trap 'rm -rf "$dir"' EXIT
      cd "$dir"
      base=https://github.com/caddyserver/caddy/releases/download/v{{ drydock_caddy_version }}
      deb=caddy_{{ drydock_caddy_version }}_linux_amd64.deb
      curl -fsSLO "$base/$deb" -fsSLO "$base/caddy_{{ drydock_caddy_version }}_checksums.txt"
      grep " $deb\$" "caddy_{{ drydock_caddy_version }}_checksums.txt" | sha512sum -c -
      apt-get install -y "./$deb"
    executable: /bin/bash
  register: drydock_caddy_deb
  changed_when: "'unchanged' not in drydock_caddy_deb.stdout"

- name: Start Docker and Caddy, now and at boot
  ansible.builtin.systemd_service:
    name: "{{ item }}"
    enabled: true
    state: started
  loop: [docker, caddy]

- name: Check the docker group and the caddy user exist (the installer needs both)
  ansible.builtin.getent:
    database: "{{ item.db }}"
    key: "{{ item.key }}"
  loop:
    - {db: group, key: docker}
    - {db: passwd, key: caddy}

- name: Read the devcontainer CLI's version, on the service's PATH
  ansible.builtin.command: devcontainer --version
  environment:
    PATH: "{{ drydock_service_path }}"
  register: drydock_devcontainer_installed
  changed_when: false
  failed_when: false

- name: Install the pinned devcontainer CLI globally, where the service's PATH finds it (runbook §3.2)
  ansible.builtin.command: npm install -g --no-fund --no-audit @devcontainers/cli@{{ drydock_devcontainer_cli_version }}
  environment:
    PATH: "{{ drydock_service_path }}"
  when: drydock_devcontainer_installed.stdout != drydock_devcontainer_cli_version
  changed_when: true

# The installer runs `devcontainer --version` as the drydock user on the
# service's PATH. That user does not exist yet, so check as nobody: a CLI under
# nvm, fnm, volta, snap or a home directory fails here, before the install.
- name: Check node and the CLI run for an unprivileged user on the service's PATH (runbook §3.2)
  ansible.builtin.command:
    argv: [runuser, -u, nobody, --, env, "PATH={{ drydock_service_path }}", HOME=/nonexistent, "{{ item.cmd }}", --version]
  loop:
    - {cmd: node, want: "v{{ drydock_node_major }}."}
    - {cmd: devcontainer, want: "{{ drydock_devcontainer_cli_version }}"}
  register: drydock_tool_check
  changed_when: false
  failed_when: drydock_tool_check.rc != 0 or not drydock_tool_check.stdout.startswith(item.want)
```

Not automated from §3: the runbook's `sudo docker run --rm hello-world` (it pulls from Docker Hub
on every run) and §3.3's decision about **a Caddy that already serves other sites**. That one is
yours: the installer refuses a foreign `/etc/caddy/Caddyfile`, and `drydock_take_over_caddy: true`
passes `--take-over-caddy`, which backs it up and replaces it. Leave it `false` unless you have read
[§3.3](first-deployment.md#33-caddy-from-its-official-package).

### 3.4 Firewall (optional)

[Runbook §3.4](first-deployment.md#34-firewall). Only if the host runs ufw, and only when
`drydock_firewall_lan_cidr` is set. It adds one rule and does **not** enable ufw. Port 80 only
redirects to 443, so it is left out; Drydock listens on no TCP port, so **open nothing else**.
Needs `community.general` ([requirements.yml](#requirements-and-layout)).

```yaml
# file: roles/drydock/tasks/firewall.yml
- name: Allow HTTPS to Caddy from the LAN (runbook §3.4)
  community.general.ufw:
    rule: allow
    port: "443"
    proto: tcp
    from_ip: "{{ drydock_firewall_lan_cidr }}"
```

---

## 4. Get the secrets onto the server without leaking them

[Runbook §4](first-deployment.md#4-get-the-secrets-onto-the-server-without-leaking-them). The
runbook's two rules hold here as Ansible rules:

- **Files are copied with `ansible.builtin.copy`, never passed as variables, `content:`,
  environment variables or command-line arguments.** The App key must never enter the environment
  or a command line ([§13.5](../design/overall/drydock-design.md#135--non-negotiables)), and a
  `--github-app-key` or `--secrets-key` flag carries only a path.
- **Every task that handles key material has `no_log: true` and `diff: false`.** `no_log` keeps it
  out of the output and any callback or log file; `diff: false` matters as much, because
  `ansible-playbook --diff` prints a copied file's contents otherwise.

### 4.1 The GitHub App private key

This is in [step 5's block](#5-install), because it has to be: the installer copies the key to
`/etc/drydock/github-app.pem` (mode `0400`, owner `drydock`), and the `drydock` user does not exist
until the installer creates it. So the same block stages the key at `/root/drydock-app.pem` (root,
`0400`), passes `--github-app-key /root/drydock-app.pem`, and then always deletes it with
`shred -u`, as the runbook does by hand.

It stages the key **only when it differs from the installed one**, comparing SHA-256 checksums, so a
re-run stages nothing and passes only `--github-app-id`, which the installer accepts once a key is
in place. To rotate the key, replace `files/drydock-app.pem` (vault-encrypted) and re-run; then
delete the old key on GitHub ([runbook §10.1](first-deployment.md#101-upgrade)).

### 4.2 The TLS certificate and key

Same directory, owners and modes as the runbook: `/etc/caddy/certs` `0750 root:caddy`, the
certificate `0644`, the key `0640`, both group `caddy`. **Not `/etc/ssl/private`**, which `caddy`
cannot traverse. A changed certificate (a renewal you copy in) reloads Caddy through the handler.

```yaml
# file: roles/drydock/tasks/tls.yml
- name: Create the certificate directory, readable by root and Caddy only (runbook §4.2)
  ansible.builtin.file:
    path: "{{ drydock_cert_dir }}"
    state: directory
    owner: root
    group: caddy
    mode: "0750"

- name: Install the TLS certificate, full chain, leaf first
  ansible.builtin.copy:
    src: "{{ drydock_cert_src }}"
    dest: "{{ drydock_cert_path }}"
    owner: root
    group: caddy
    mode: "0644"
  notify: Reload caddy

- name: Install the TLS private key
  ansible.builtin.copy:
    src: "{{ drydock_key_src }}"
    dest: "{{ drydock_key_path }}"
    owner: root
    group: caddy
    mode: "0640"
  diff: false
  no_log: true
  notify: Reload caddy

- name: Install the private CA's certificate, for the installer's final check (option A only)
  ansible.builtin.copy:
    src: "{{ drydock_ca_cert_src }}"
    dest: "{{ drydock_ca_cert_path }}"
    owner: root
    group: caddy
    mode: "0644"
  when: drydock_ca_cert_src | length > 0

- name: Check the caddy user can read the certificate and key, as the installer will
  ansible.builtin.command:
    argv: [runuser, -u, caddy, --, test, -r, "{{ item }}"]
  loop: ["{{ drydock_cert_path }}", "{{ drydock_key_path }}"]
  changed_when: false

# Runbook §1.3's two checks. Both print public data only.
- name: Read the certificate's public key and names
  ansible.builtin.command:
    argv: [openssl, x509, -noout, -pubkey, -ext, subjectAltName, -in, "{{ drydock_cert_path }}"]
  register: drydock_cert_info
  changed_when: false

- name: Read the private key's public half
  ansible.builtin.command:
    argv: [openssl, pkey, -pubout, -in, "{{ drydock_key_path }}"]
  register: drydock_key_pub
  changed_when: false

- name: Check the certificate matches the key and names the UI host (runbook §1.3)
  ansible.builtin.assert:
    that:
      - drydock_key_pub.stdout in drydock_cert_info.stdout
      - >-
        ('DNS:' ~ drydock_ui_host) in drydock_cert_info.stdout or
        ('DNS:*.' ~ drydock_ui_host.split('.', 1)[1]) in drydock_cert_info.stdout
    fail_msg: >-
      The certificate does not match the key, or its subjectAltName does not
      name {{ drydock_ui_host }}. See runbook §1.3.
```

**With previews** (`drydock_preview_domain` set; [runbook §8.7](first-deployment.md#87-optional-enable-previews)),
the wildcard pair goes beside the UI's with the same owners and modes, and gets the same two
checks, with the SAN required to carry the wildcard itself. Without previews these tasks are
skipped.

```yaml
# file: roles/drydock/tasks/tls.yml
- name: Install the preview wildcard certificate, full chain, leaf first (runbook §8.7)
  ansible.builtin.copy:
    src: "{{ drydock_preview_cert_src }}"
    dest: "{{ drydock_preview_cert_path }}"
    owner: root
    group: caddy
    mode: "0644"
  when: drydock_preview_domain | length > 0
  notify: Reload caddy

- name: Install the preview wildcard private key
  ansible.builtin.copy:
    src: "{{ drydock_preview_key_src }}"
    dest: "{{ drydock_preview_key_path }}"
    owner: root
    group: caddy
    mode: "0640"
  diff: false
  no_log: true
  when: drydock_preview_domain | length > 0
  notify: Reload caddy

- name: Check the caddy user can read the preview certificate and key
  ansible.builtin.command:
    argv: [runuser, -u, caddy, --, test, -r, "{{ item }}"]
  loop: ["{{ drydock_preview_cert_path }}", "{{ drydock_preview_key_path }}"]
  changed_when: false
  when: drydock_preview_domain | length > 0

- name: Read the preview certificate's public key and names
  ansible.builtin.command:
    argv: [openssl, x509, -noout, -pubkey, -ext, subjectAltName, -in, "{{ drydock_preview_cert_path }}"]
  register: drydock_preview_cert_info
  changed_when: false
  when: drydock_preview_domain | length > 0

- name: Read the preview private key's public half
  ansible.builtin.command:
    argv: [openssl, pkey, -pubout, -in, "{{ drydock_preview_key_path }}"]
  register: drydock_preview_key_pub
  changed_when: false
  when: drydock_preview_domain | length > 0

- name: Check the preview certificate matches its key and is the wildcard (runbook §8.7)
  ansible.builtin.assert:
    that:
      - drydock_preview_key_pub.stdout in drydock_preview_cert_info.stdout
      - ('DNS:*.' ~ drydock_preview_domain) in drydock_preview_cert_info.stdout
    fail_msg: >-
      The preview certificate does not match its key, or its subjectAltName does
      not carry DNS:*.{{ drydock_preview_domain }}. See runbook §8.7.
  when: drydock_preview_domain | length > 0
```

### 4.3 Optional: your own secrets master key

[Runbook §4.3](first-deployment.md#43-optional-your-own-secrets-master-key) and
[§6](first-deployment.md#6-the-secrets-master-key-supply-it-or-back-it-up). Set
`drydock_secrets_key_src` to a vault-encrypted file of exactly 32 raw bytes (your own key), or
leave it empty and let the installer generate the key, which
[step 6](#6-the-secrets-master-key-supply-it-or-back-it-up) backs up. To make a new one, on the controller:

```sh
(umask 077 && head -c 32 /dev/urandom > files/drydock-secrets.key)   # 32 raw bytes: not base64, no newline
ansible-vault encrypt files/drydock-secrets.key
```

Like the App key, it is staged by [step 5's block](#5-install), because the installer installs it
as `drydock`'s and that user may not exist yet: only when its SHA-256 differs from the installed
`/etc/drydock/secrets.key`, at `/root/drydock-secrets.key` (root, `0400`), passed as
`--secrets-key`, and always deleted with `shred -u`. A re-run stages nothing and passes no flag, so
it changes nothing.

A different key replaces the installed one **only while no secret is stored**. With a secret
stored, the installer refuses, changes nothing, and the play fails on the installer task with its
message (`… N stored secret(s) are sealed under the installed key …`). What to do then is
[runbook §6.3](first-deployment.md#63-switch-an-installed-key-to-one-you-supply).

---

## 5. Install

[Runbook §5](first-deployment.md#5-install).

**How the installer is fetched.** Not the `curl … | sudo bash` one-liner, and not the stamped
standalone `install.sh` either: the release's `SHA256SUMS` covers only the tarball, so the
standalone script is the one file nothing verifies. Instead, `get_url` downloads
`drydock_linux_amd64.tar.gz` for `drydock_version` and checks it against that release's
`SHA256SUMS` (the same check the standalone script does), and the play runs the `install.sh`
**inside** the tarball. That is the exact file the one-liner ends up running: the standalone
script's only job is to download, verify and re-run it. The version is pinned by the URL, so
`--version` is not passed (the installer ignores it for an extracted release and says so). As the
installer's own comment says, a checksum from the same place as the tarball catches a corrupt
download, not a compromised release.

**The flags** are the runbook's: `--ui-host`, `--cert`, `--key`, `--github-app-id`, plus
`--github-app-key` when a new key is staged ([§4.1](#41-the-github-app-private-key)),
`--secrets-key` when a new master key is staged ([§4.3](#43-optional-your-own-secrets-master-key)),
and `--ca-cert` for option A. Without a CA certificate the task passes `--no-ca-cert`, so the variables
stay the whole truth: the installer otherwise keeps a `--ca-cert` from an earlier run. Likewise
`--preview-domain`, `--preview-cert` and `--preview-key` when `drydock_preview_domain` is set, and
`--no-preview` when it is not, which removes a preview site an earlier run installed. And
`--vscode-ssh-host` when `drydock_vscode_ssh_host` is set, `--no-vscode-ssh-host` when it is not, for
the same reason: emptying the variable turns the link off rather than leaving an earlier run's
address in `drydock.env`. Either way an unchanged value rewrites nothing and restarts nothing, so
the task stays unchanged on a re-run. The installer refuses a value that is not
`[user@]host[:port]` before it changes anything (`--vscode-ssh-host refused: …`). An installer from
before v0.10.0 has neither flag and stops at either with `unknown option`, which is why
`drydock_version` must be v0.10.0 or later. A port is
previewed from the workspace's page in the UI, never from the playbook
([runbook §8.7](first-deployment.md#87-optional-enable-previews)). The runbook's real-Safari check
([§8.8](first-deployment.md#88-preview-a-port-and-the-real-safari-check)) is done by hand on an
iPhone and a Mac, and has no task here: it is a browser's behaviour that is being checked, and
nothing Ansible can drive.

**The password.** The installer asks for it on `/dev/tty`, and Ansible has no terminal to answer
on. Worse, with `ssh -tt` it may *have* one, and then the installer waits on a prompt nobody sees.
So the task passes **`--no-password`**, and the next task sets it the installer's own way:
`drydock passwd --if-unset`, which reads one line from stdin when stdin is not a terminal. The
password goes in through the `command` module's `stdin:`, never `argv` (where `ps` shows it), with
`no_log: true`. `--if-unset` makes it a no-op once a password exists, and it then never reads
stdin, so a re-run signs no one out. **Changing `vault_drydock_operator_password` later changes
nothing**: to change the password, run the runbook's
`sudo -u drydock drydock passwd --db /var/lib/drydock/drydock.db` by hand, which signs out every
device.

**`changed`.** The installer is idempotent and says what it did: every action prints a `==> ` line
(`==> starting drydock`, `==> reloading caddy`, `==> installed the GitHub App key …`, …), and a run
that did nothing prints only `==> Drydock vX is installed and current` and `==> open https://…`.
So the task is `changed` exactly when there is any other `==> ` line. The version line alone would
be wrong: a new certificate path restarts services and still reports "installed and current".

**Failure.** The installer stops at the first failure, changes nothing below it, and exits
non-zero, and so does the task. A refusal of a setting or of a new master key comes before it
replaces anything, the binary included, so the release that was running still is. That includes its final check: an unauthenticated
`https://<ui-host>/api/auth/session` through Caddy must answer `401` over a verified certificate —
and with previews on, `https://drydock-check.<preview-domain>/` must answer `302` to its own
`/.drydock/denied` the same way.
When only the certificate could not be verified, the message starts `Drydock is installed and
running, and answers through Caddy, but this host could not verify the certificate …`; it is still
a failure (usually a missing `drydock_ca_cert_src` for option A). The installer's output names no
secret, so the task shows it; the fixes are in [runbook §11](first-deployment.md#11-troubleshooting).

**Before an upgrade** the block backs up the database
([runbook §10.1](first-deployment.md#101-upgrade), [Known issue 2](first-deployment.md#0-known-issues--read-these-first)):
when an installed `drydock version` differs from `drydock_version`, it stops `drydock`, copies
`/var/lib/drydock/drydock.db` to `/root/drydock.db.<timestamp>` (root, `0600`), and starts it again.
It does the same when the running `drydock` is not the installed binary (the file it executes has
another inode): an earlier run was cut off after it replaced the binary, or an installer before
v0.4.2 refused a `--secrets-key` after doing so. Restarting that process onto the
installed binary is the upgrade, so it gets an upgrade's backup first; the backup's own stop and start do the restart (the installer would otherwise).

```yaml
# file: roles/drydock/tasks/install.yml
- name: Check the settings the installer would refuse
  ansible.builtin.assert:
    that:
      - drydock_version is match('^v[0-9]+[.][0-9]+[.][0-9]+$')
      - drydock_ui_host == drydock_ui_host | lower
      - drydock_ui_host is match('^[a-z0-9]([a-z0-9.-]*[a-z0-9])?$')
      - "'.' in drydock_ui_host"
      - drydock_app_id | string is match('^[1-9][0-9]*$')
    fail_msg: >-
      drydock_version must be a release tag (vMAJOR.MINOR.PATCH), drydock_ui_host must be a lowercase
      fully qualified name, and drydock_app_id the numeric App ID (not the Iv… Client ID).

# The installer refuses a preview domain on the UI host's registrable domain
# itself (runbook §8.7), before it changes anything; this checks the shape.
- name: Check the preview settings, when previews are on
  ansible.builtin.assert:
    that:
      - drydock_preview_domain == drydock_preview_domain | lower
      - "'.' in drydock_preview_domain"
      - drydock_preview_cert_src | length > 0
      - drydock_preview_key_src | length > 0
    fail_msg: >-
      drydock_preview_domain must be a lowercase domain with a dot, and needs
      drydock_preview_cert_src and drydock_preview_key_src beside it (runbook §8.7).
  when: drydock_preview_domain | length > 0

# Checked without printing it: neither the condition nor the message carries the value.
- name: Check the operator password is long enough (drydock passwd refuses under 12)
  ansible.builtin.fail:
    msg: drydock_operator_password must be at least 12 characters, on one line (runbook §1.5).
  when: drydock_operator_password | length < 12 or '\n' in drydock_operator_password

- name: Create the release cache directory
  ansible.builtin.file:
    path: "{{ drydock_release_dir }}"
    state: directory
    owner: root
    group: root
    mode: "0755"

- name: Download the release tarball, verified against the release's SHA256SUMS
  ansible.builtin.get_url:
    url: "{{ drydock_release_base_url }}/drydock_linux_amd64.tar.gz"
    dest: "{{ drydock_release_dir }}/drydock_linux_amd64.tar.gz"
    checksum: "sha256:{{ drydock_release_base_url }}/SHA256SUMS"
    owner: root
    group: root
    mode: "0644"

- name: Unpack it (drydock/drydock, drydock/install.sh, the Caddy files, VERSION)
  ansible.builtin.unarchive:
    src: "{{ drydock_release_dir }}/drydock_linux_amd64.tar.gz"
    dest: "{{ drydock_release_dir }}"
    remote_src: true
    creates: "{{ drydock_release_dir }}/drydock/install.sh"

- name: Look for an installed binary
  ansible.builtin.stat:
    path: /usr/local/bin/drydock
    get_checksum: false
  register: drydock_installed_binary

- name: Read the installed release
  ansible.builtin.command:
    argv: [/usr/local/bin/drydock, version]
  register: drydock_installed_version
  changed_when: false
  when: drydock_installed_binary.stat.exists

- name: Read drydock's main PID
  ansible.builtin.command:
    argv: [systemctl, show, --property=MainPID, --value, drydock]
  register: drydock_running_pid
  changed_when: false
  when: drydock_installed_binary.stat.exists

# Followed to the file the process executes, deleted or not: an inode other
# than the installed binary's is a release the installer will restart away from.
- name: Look at the binary the running drydock executes
  ansible.builtin.stat:
    path: "/proc/{{ drydock_running_pid.stdout }}/exe"
    follow: true
    get_checksum: false
    get_mime: false
    get_attributes: false
  register: drydock_running_binary
  when:
    - drydock_running_pid is not skipped
    - drydock_running_pid.stdout not in ['', '0']

- name: Back up the database before changing release (runbook §10.1, Known issue 2)
  when:
    - drydock_installed_version is not skipped
    - >-
      drydock_installed_version.stdout != drydock_version or (
      drydock_running_binary is not skipped and drydock_running_binary.stat.exists and
      [drydock_running_binary.stat.dev, drydock_running_binary.stat.inode] !=
      [drydock_installed_binary.stat.dev, drydock_installed_binary.stat.inode])
  block:
    - name: Stop drydock, for a clean, checkpointed copy
      ansible.builtin.systemd_service:
        name: drydock
        state: stopped

    - name: Copy the database to /root, root-only
      ansible.builtin.copy:
        src: /var/lib/drydock/drydock.db
        dest: "/root/drydock.db.{{ now(fmt='%Y%m%d%H%M%S') }}"
        remote_src: true
        owner: root
        group: root
        mode: "0600"
  always:
    - name: Start drydock again
      ansible.builtin.systemd_service:
        name: drydock
        state: started

- name: Checksum the installed App key (not its contents)
  ansible.builtin.stat:
    path: /etc/drydock/github-app.pem
    checksum_algorithm: sha256
  register: drydock_app_key_installed

- name: Checksum the installed secrets master key (not its contents)
  ansible.builtin.stat:
    path: /etc/drydock/secrets.key
    checksum_algorithm: sha256
  register: drydock_secrets_key_installed

- name: Stage the keys, run the installer, and always remove the staged keys
  block:
    - name: Stage the App key for the installer, only when it is new or changed (runbook §4.1)
      ansible.builtin.copy:
        src: "{{ drydock_app_key_src }}"
        dest: "{{ drydock_app_key_stage }}"
        owner: root
        group: root
        mode: "0400"
      diff: false
      no_log: true
      when: >-
        not drydock_app_key_installed.stat.exists or
        drydock_app_key_installed.stat.checksum !=
        (lookup('ansible.builtin.file', drydock_app_key_src, rstrip=false) | hash('sha256'))
      register: drydock_app_key_staged

    - name: Stage your secrets master key, only when it is new or changed (runbook §4.3)
      ansible.builtin.copy:
        src: "{{ drydock_secrets_key_src }}"
        dest: "{{ drydock_secrets_key_stage }}"
        owner: root
        group: root
        mode: "0400"
      diff: false
      no_log: true
      when: >-
        drydock_secrets_key_src | length > 0 and (
        not drydock_secrets_key_installed.stat.exists or
        drydock_secrets_key_installed.stat.checksum !=
        (lookup('ansible.builtin.file', drydock_secrets_key_src, rstrip=false) | hash('sha256')))
      register: drydock_secrets_key_staged

    - name: Run the installer from the release (runbook §5)
      ansible.builtin.command:
        argv: >-
          {{
            ['bash', drydock_release_dir ~ '/drydock/install.sh',
             '--ui-host', drydock_ui_host,
             '--cert', drydock_cert_path,
             '--key', drydock_key_path,
             '--github-app-id', drydock_app_id | string,
             '--no-password']
            + (['--ca-cert', drydock_ca_cert_path] if drydock_ca_cert_src | length > 0 else ['--no-ca-cert'])
            + (['--github-app-key', drydock_app_key_stage] if drydock_app_key_staged is not skipped else [])
            + (['--secrets-key', drydock_secrets_key_stage] if drydock_secrets_key_staged is not skipped else [])
            + (['--preview-domain', drydock_preview_domain,
                '--preview-cert', drydock_preview_cert_path,
                '--preview-key', drydock_preview_key_path]
               if drydock_preview_domain | length > 0 else ['--no-preview'])
            + (['--vscode-ssh-host', drydock_vscode_ssh_host] if drydock_vscode_ssh_host | length > 0
               else ['--no-vscode-ssh-host'])
            + (['--take-over-caddy'] if drydock_take_over_caddy | bool else [])
          }}
      register: drydock_installer
      changed_when: >-
        drydock_installer.stdout_lines
        | select('match', '^==> ')
        | reject('match', '^==> (Drydock [^ ]+ is installed and current|open https://)')
        | list | length > 0
  always:
    - name: Delete the staged App key (runbook §5)
      ansible.builtin.command:
        argv: [shred, -u, "{{ drydock_app_key_stage }}"]
        removes: "{{ drydock_app_key_stage }}"

    - name: Delete the staged secrets master key (runbook §5)
      ansible.builtin.command:
        argv: [shred, -u, "{{ drydock_secrets_key_stage }}"]
        removes: "{{ drydock_secrets_key_stage }}"

- name: Set the operator password, only if none is set yet (runbook §5 step 14)
  ansible.builtin.command:
    argv: [runuser, -u, drydock, --, /usr/local/bin/drydock, passwd, --db, /var/lib/drydock/drydock.db, --if-unset]
    stdin: "{{ drydock_operator_password }}"
  register: drydock_passwd
  changed_when: "'Password set.' in drydock_passwd.stdout"
  failed_when: false
  no_log: true

# drydock passwd never echoes the password, so its stderr is safe to show; the
# task above hides everything, including why it failed.
- name: Report why the password could not be set
  ansible.builtin.fail:
    msg: "drydock passwd failed: {{ drydock_passwd.stderr }}"
  when: drydock_passwd.rc != 0
```

What the installer creates, and where, is [runbook §5's table](first-deployment.md#5-install).
[Known issue 3](first-deployment.md#0-known-issues--read-these-first) applies unchanged: there is no
flag for the bot identity, the container cap or the label prefix, and **do not template or
`lineinfile` `drydock.service`** to add them. The next installer run overwrites the unit, and its
rollback restores `drydock.service.previous`, not your edit.

---

## 6. The secrets master key: supply it, or back it up

[Runbook §6](first-deployment.md#6-the-secrets-master-key-supply-it-or-back-it-up). Two options:

- **Your own key, `drydock_secrets_key_src` set:** the key is already in your vault, and the installer
  installed that file ([§4.3](#43-optional-your-own-secrets-master-key)). There is no backup step:
  the tasks below are skipped, and [§7.1](#71-on-the-server) checks that the server holds the
  vaulted key.
- **A generated key, `drydock_secrets_key_src` empty:** the installer generated the key, and the tasks
  below fetch `/etc/drydock/secrets.key` to `drydock_secrets_key_backup` on the controller, in a
  `0700` directory, as a `0400` file.

> [!WARNING]
> **With a generated key, move that file into your vault, then delete it from the controller**
> ([below](#move-to-a-vaulted-key)). Every repository secret is encrypted under this key:
> **lose it and every stored secret is unreadable**, with no recovery but typing each value in
> again. And keep it **apart from any database backup**, because the two together are every secret
> in plaintext-equivalent form. Do not commit it unencrypted, and do not leave it loose in the
> Ansible directory.

It never overwrites a backup silently. If a file is already at that path, the task compares
checksums: the same key passes untouched, and a **different** one fails the play. That means the
server's key is not the one you backed up, which needs a person, not a playbook: either the backup
path is reused from another server, or the server's key was replaced and every secret stored under
the old one is unreadable. Once you have moved the backup into your vault and deleted the local
file, set `drydock_secrets_key_src` to the vaulted copy, and later runs skip the backup.

```yaml
# file: roles/drydock/tasks/backup_master_key.yml
- name: Checksum the server's secrets master key (not its contents)
  ansible.builtin.stat:
    path: /etc/drydock/secrets.key
    checksum_algorithm: sha256
  register: drydock_master_key

- name: Look for an existing backup on the controller
  ansible.builtin.stat:
    path: "{{ drydock_secrets_key_backup }}"
    checksum_algorithm: sha256
  delegate_to: localhost
  become: false
  register: drydock_master_key_backup

- name: Refuse to overwrite a backup of a different key
  ansible.builtin.fail:
    msg: >-
      {{ drydock_secrets_key_backup }} holds a different key from the server's
      /etc/drydock/secrets.key. Nothing was overwritten. Find out which key your
      secrets were stored under before doing anything else (runbook §6).
  when:
    - drydock_master_key_backup.stat.exists
    - drydock_master_key_backup.stat.checksum != drydock_master_key.stat.checksum

- name: Create a private backup directory on the controller
  ansible.builtin.file:
    path: "{{ drydock_secrets_key_backup | dirname }}"
    state: directory
    mode: "0700"
  delegate_to: localhost
  become: false

- name: Fetch the secrets master key (runbook §6)
  ansible.builtin.fetch:
    src: /etc/drydock/secrets.key
    dest: "{{ drydock_secrets_key_backup }}"
    flat: true
    fail_on_missing: true
  no_log: true
  when: not drydock_master_key_backup.stat.exists

- name: Make the backup readable by you alone
  ansible.builtin.file:
    path: "{{ drydock_secrets_key_backup }}"
    mode: "0400"
  delegate_to: localhost
  become: false
```

### Move to a vaulted key

For a server already installed with a generated key, such as a first run with
`drydock_secrets_key_src` empty. Either way ends with the key in your vault and
`drydock_secrets_key_src` pointing at it.

**Vault the installed key.** This keeps the key, so it works whether or not secrets are stored,
and the next run changes nothing. With the backup the play fetched:

```sh
install -m 0600 ~/drydock-backup/secrets.key files/drydock-secrets.key
ansible-vault encrypt files/drydock-secrets.key
shred -u ~/drydock-backup/secrets.key && rmdir ~/drydock-backup
```

(If the play never fetched one, fetch it once instead:
`ansible drydock -b -m ansible.builtin.fetch -a "src=/etc/drydock/secrets.key dest=files/drydock-secrets.key flat=true"`,
then `chmod 0600 files/drydock-secrets.key` and encrypt it as above.) Then set
`drydock_secrets_key_src: "{{ playbook_dir }}/files/drydock-secrets.key"` and re-run: the
checksums match, so nothing is staged and the run reports `changed=0`.

**Or install a new key**, only while no secret is stored. Make and encrypt one as in
[§4.3](#43-optional-your-own-secrets-master-key), set `drydock_secrets_key_src`, and re-run. The
installer prints `==> stopping drydock to replace the secrets master key` and
`==> replaced the secrets master key at /etc/drydock/secrets.key with /root/drydock-secrets.key …`,
and starts Drydock on the new key. Then shred the old backup, which no longer matches the server:
`shred -u ~/drydock-backup/secrets.key`. With a secret stored, the installer refuses and the play
fails with nothing changed ([runbook §6.3](first-deployment.md#63-switch-an-installed-key-to-one-you-supply)),
even when the same run also changed `drydock_version`: the refusal comes before the new binary is
installed, so the next run without the new key still sees the old release, backs up, and upgrades.
To check first:
`ansible drydock -b -m ansible.builtin.command -a "runuser -u drydock -- drydock count-secrets --db /var/lib/drydock/drydock.db"`
prints `0`.

To restore the key onto a rebuilt server, set `drydock_secrets_key_src` to the vaulted copy before
the first run: the installer installs it in place of generating one.

---

## 7. Verify

### 7.1 On the server

[Runbook §7.1](first-deployment.md#71-on-the-server), as assertions. The `401` check is the
runbook's own `curl` line, run on the server with the UI host resolved to `127.0.0.1` (`--resolve`),
because `ansible.builtin.uri` has no way to pin a name to an address while still sending that name
for SNI and `Host`. It verifies the certificate: against `drydock-ca.pem` for option A, against the
system store otherwise.

The journal check reads only the **running** process's lines (`_PID=` its main PID), so a failure
from an earlier start in the same boot does not fail it. It is an allowlist: every line must be the
serving line, and any other line fails it, whether or not [runbook §7.1](first-deployment.md#71-on-the-server) lists it. The first catalog refresh logs only when it
fails, and asynchronously, so a wrong App key can show up seconds after this check has passed: look
at `journalctl -u drydock -o cat` once more after signing in. That assertion carries its own tag,
`drydock_verify_journal`, so `--skip-tags drydock_verify_journal` leaves it out while you fix
something it already told you about.

```yaml
# file: roles/drydock/tasks/verify.yml
- name: Check both services are active
  ansible.builtin.command:
    argv: [systemctl, is-active, drydock, caddy]
  register: drydock_active
  changed_when: false
  failed_when: drydock_active.stdout_lines != ['active', 'active']

- name: Read drydock's main PID
  ansible.builtin.command:
    argv: [systemctl, show, --property=MainPID, --value, drydock]
  register: drydock_mainpid
  changed_when: false

- name: Check drydock runs as the drydock user
  ansible.builtin.command:
    argv: [ps, -o, user=, -p, "{{ drydock_mainpid.stdout }}"]
  register: drydock_ps_user
  changed_when: false
  failed_when: drydock_ps_user.stdout | trim != 'drydock'

- name: Check an unauthenticated API call through Caddy is 401, over a verified certificate
  ansible.builtin.command:
    argv: >-
      {{
        ['curl', '-sS', '-o', '/dev/null', '-w', '%{http_code}', '--max-time', '10',
         '--resolve', drydock_ui_host ~ ':443:127.0.0.1']
        + (['--cacert', drydock_ca_cert_path] if drydock_ca_cert_src | length > 0 else [])
        + ['https://' ~ drydock_ui_host ~ '/api/auth/session']
      }}
  register: drydock_gate
  changed_when: false
  failed_when: drydock_gate.stdout != '401'

- name: List the listening TCP sockets
  ansible.builtin.command:
    argv: [ss, -ltnpH]
  register: drydock_listening
  changed_when: false

- name: Check Caddy's admin API is off port 2019, and drydock listens on no TCP port
  ansible.builtin.assert:
    that:
      - drydock_listening.stdout is not search(':2019[^0-9]')
      - drydock_listening.stdout is not search('"drydock"')
    fail_msg: "{{ drydock_listening.stdout }}"
    quiet: true

- name: Read the running drydock's journal
  ansible.builtin.command:
    argv: [journalctl, "_PID={{ drydock_mainpid.stdout }}", --boot, --no-pager, --output=cat]
  register: drydock_journal
  changed_when: false

- name: Check the journal says it is serving, and nothing else (runbook §7.1)
  tags: [drydock_verify_journal]
  ansible.builtin.assert:
    that:
      - "'drydock: serving on /run/drydock/http.sock and /run/drydock/preview.sock' in drydock_journal.stdout_lines"
      # An allowlist, not a list of known failures: every line is the serving
      # line, so a failure line added later fails this too.
      - >-
        drydock_journal.stdout_lines
        | reject('equalto', 'drydock: serving on /run/drydock/http.sock and /run/drydock/preview.sock')
        | reject('equalto', '')
        | list | length == 0
    fail_msg: "{{ drydock_journal.stdout }}"
    quiet: true

- name: Check the secrets master key's owner, mode and size (never its contents)
  ansible.builtin.stat:
    path: /etc/drydock/secrets.key
    get_checksum: false
  register: drydock_secrets_key
  failed_when: >-
    not drydock_secrets_key.stat.exists or
    [drydock_secrets_key.stat.pw_name, drydock_secrets_key.stat.mode, drydock_secrets_key.stat.size]
    != ['drydock', '0400', 32]

- name: Check the server holds your vaulted master key (checksums, not contents)
  ansible.builtin.stat:
    path: /etc/drydock/secrets.key
    checksum_algorithm: sha256
  register: drydock_secrets_key_sum
  failed_when: >-
    drydock_secrets_key_sum.stat.checksum !=
    (lookup('ansible.builtin.file', drydock_secrets_key_src, rstrip=false) | hash('sha256'))
  when: drydock_secrets_key_src | length > 0
```

### 7.2 and 7.3 From a laptop and a phone

Not automated, on purpose: the point of [runbook §7.2](first-deployment.md#72-from-a-laptop) and
[§7.3](first-deployment.md#73-from-a-phone) is that *those devices* resolve the name and trust the
certificate. Do them by hand. Signing in from the phone is Phase 1's acceptance test.

---

## 8–9. First workspace, and what does not work yet

[Runbook §8](first-deployment.md#8-first-workspace) and [§9](first-deployment.md#9-what-does-not-work-yet)
are UI work and checks inside a running container. Nothing to automate; do them as written.

### Opening a workspace in VS Code

[Runbook §8.9](first-deployment.md#89-optional-opening-a-workspace-in-vs-code). Set
`drydock_vscode_ssh_host` to the address your VS Code's Remote-SSH uses for this server —
`[user@]host[:port]`, or the `Host` alias from your `~/.ssh/config` — and run the play: the
installer keeps it in `/etc/drydock/drydock.env` as `DRYDOCK_VSCODE_SSH_HOST` and restarts Drydock
with `--vscode-ssh-host`. Each running workspace's card and page then carry **Open in VS Code**;
until it is set, the workspace page says how to turn it on. The rest is the runbook's, and none of
it is this play's to do:

- The SSH user needs Docker access on the server (the `docker` group: root on this host by
  another name). The play does not add your account to it; that is your decision.
- VS Code needs the *Remote - SSH* and *Dev Containers* extensions, and asks to confirm opening the
  link.
- Never use VS Code's own *Rebuild Container* on a workspace: it would replace Drydock's container
  with one Drydock does not know. Use Drydock's **Rebuild**.
- A container an earlier release made gets the `devcontainer.local_folder` and
  `devcontainer.config_file` labels — what *Reopen in Container* on the clone finds it by, instead
  of building a second container — only at its next **Rebuild**, which also changes its
  `${devcontainerId}`: a volume a configuration names with it starts empty.

---

## 10. Upgrade, roll back, uninstall

### 10.1 Upgrade

[Runbook §10.1](first-deployment.md#101-upgrade). Wait until the release is published
([runbook step 2](first-deployment.md#2-cut-a-release); a *draft* cannot be downloaded, so the
download task fails rather than installing anything), then change one line and re-run:

```yaml
drydock_version: vX.Y.Z   # the new release's tag
```

The play backs up the database first (the [step 5](#5-install) block above), then the installer
upgrades in place. It ends with `==> upgraded Drydock vA -> vB`. A re-run without a version change
reports `ok` on every install task: the installer prints `is installed and current`, and nothing is
restarted. The App key and the master key are kept, and the password is not asked for again.
With `drydock_secrets_key_src` set, the installed key matches it, so nothing is staged.
Changing a certificate, the CA, or the App key works the same way: change the file or the variable
and re-run.

**Upgrading from v0.4.1 or earlier, Rebuild each workspace once**, from the UI. Those releases
mounted each workspace's broker socket as a file, which the upgrade's restart strands; the reasons,
the symptoms (`exit 69`, git and `gh` failing with *GitHub access unavailable*) and a loop that
lists the containers still needing it are in
[runbook §10.1](first-deployment.md#101-upgrade). A play should not rebuild workspaces: a rebuild
replaces a container someone may be working in.

### 10.2 Roll back

- **Automatic:** when a new binary does not start, the installer restores the previous binary (and
  unit), and exits non-zero with `the upgrade failed and was rolled back to vA; see the log above`.
  The play fails on that task with that message. Then **set `drydock_version` back to `vA`**, or the
  next run tries the broken release again.
- **But a rollback cannot undo a migration** ([Known issue 2](first-deployment.md#0-known-issues--read-these-first)):
  if the new binary migrated the database before failing, the old one refuses it with
  `refusing to run an older Drydock against a newer database`, and the rollback does not start
  either. The fix is to restore the backup the play made, `/root/drydock.db.<timestamp>`.
- **Manual, to an older release:** restore the matching database backup **by hand first**, exactly as
  in [runbook §10.2](first-deployment.md#102-roll-back) (stop, `install` the backup over
  `drydock.db`, remove `-wal` and `-shm`), then set `drydock_version` to the older release and
  re-run. Restoring a database is not something a play should do on its own; it throws away
  everything since the backup. (The play backs up the newer database first, because the version
  differs. That backup is harmless.)

### 10.3 Uninstall

There is no `uninstall` ([Known issue 4](first-deployment.md#0-known-issues--read-these-first)), and
this document has no tasks for one: [runbook §10.4](first-deployment.md#104-uninstall) is a short,
destructive list best run by a person who has just deleted every workspace in the UI.

---

## What this does not automate

| Runbook step | Why it stays by hand |
|---|---|
| [0. Known issue 1](first-deployment.md#0-known-issues--read-these-first), [2. Cut a release](first-deployment.md#2-cut-a-release) | Merging the release PR needs an admin bypass, on GitHub, not on the server. The play only installs a release that is already published. |
| [1.2 DNS](first-deployment.md#12-a-hostname-for-the-ui-and-how-clients-resolve-it), [1.3 choosing a certificate](first-deployment.md#13-a-tls-certificate-every-client-trusts-for-that-hostname) | Decisions, and client-side settings. The play checks the result: the certificate matches its key and names `drydock_ui_host`. Renewal (options B and C) stays with your ACME client; copying a renewed certificate in and re-running reloads Caddy. |
| [1.4 the App's permissions, installations and key](first-deployment.md#14-the-github-apps-private-key) | In GitHub's UI. The App is private. |
| [3.3 a Caddy that already serves other sites](first-deployment.md#33-caddy-from-its-official-package) | A decision: `drydock_take_over_caddy`. |
| [6. storing the master-key backup](first-deployment.md#6-the-secrets-master-key-supply-it-or-back-it-up) | A generated key only: the play fetches it; moving it into your vault is yours ([above](#move-to-a-vaulted-key)). With your own key there is nothing to store. |
| [7.2, 7.3 laptop and phone checks](first-deployment.md#72-from-a-laptop) | They test those devices. |
| [8. First workspace](first-deployment.md#8-first-workspace), [8.5 the reboot drill](first-deployment.md#85-optional-the-reboot-drill) | UI work. |
| Changing the operator password | Ends every session; `drydock passwd` by hand ([runbook §10.3](first-deployment.md#103-logs-and-state)). |
| [10.2 manual rollback](first-deployment.md#102-roll-back), [10.4 uninstall](first-deployment.md#104-uninstall) | Destructive; see [10.2](#102-roll-back) above. |
| [Known issue 3](first-deployment.md#0-known-issues--read-these-first): cap, bot identity, label prefix | The installer has no flags for them, and the unit is the installer's. |
