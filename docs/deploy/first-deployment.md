# First deployment

A step-by-step checklist for putting Drydock on your own dev server for the first time, by
yourself, from a release published on GitHub. Work through it top to bottom and tick each box.
Every command is for a **Debian or Ubuntu** host unless a step says otherwise.

Each step that has a non-obvious reason gets one line of *why*, with a link to the design
section that argues it. If you want the argument, follow the link; to deploy, you do not need to.

> [!IMPORTANT]
> **Replace `drydock.example.com` everywhere below with your own UI hostname**, in lowercase.
> It shows up in commands, paths and URLs. (The installer lowercases `--ui-host` anyway, but the
> URL you type and the name on the certificate are easier to compare in one spelling.) A few other placeholders appear in `<angle brackets>`.

- [0. Known issues](#0-known-issues--read-these-first)
- [1. Before you start](#1-before-you-start)
- [2. Cut a release](#2-cut-a-release)
- [3. Prerequisites on the server](#3-prerequisites-on-the-server)
- [4. Get the secrets onto the server without leaking them](#4-get-the-secrets-onto-the-server-without-leaking-them)
- [5. Install](#5-install)
- [6. Back up the secrets master key, now](#6-back-up-the-secrets-master-key-now)
- [7. Verify](#7-verify)
- [8. First workspace](#8-first-workspace)
- [9. What does not work yet](#9-what-does-not-work-yet)
- [10. Upgrade, roll back, uninstall, logs](#10-upgrade-roll-back-uninstall-logs)
- [11. Troubleshooting](#11-troubleshooting)

---

## 0. Known issues — read these first

Gaps found while writing this runbook. None of them blocks a first deployment if you know about
them. Each one is also mentioned at the step where it bites.

1. **The release PR cannot merge without an admin bypass.** The `main` ruleset requires five
   status checks (`go`, `web`, `installer`, `conventional`, `contract`). The release-please PR is
   opened by `GITHUB_TOKEN`, and a PR opened that way triggers no workflows, so those checks never
   report and the PR shows as *blocked*. Repository admins may bypass the ruleset. Merging with the
   bypass is safe because the release workflow runs the full suite again before it uploads anything.
   See [step 2](#2-cut-a-release).
2. **An automatic rollback cannot undo a database migration.** If an upgrade's new binary migrates
   the database and then fails to start, the installer puts the old binary back. The old binary
   then refuses the newer database (`refusing to run an older Drydock against a newer database`),
   so the rollback does not start either. The same applies to a manual downgrade with `--version`.
   Back up the database before every upgrade ([step 10](#10-upgrade-roll-back-uninstall-logs)).
3. **The installer has no flag for the bot identity, the container cap or the label prefix.** The
   unit runs `drydock serve` with the built-in defaults: the bot identity of the **production** App
   (`krelinga-drydock[bot]`), at most 10 workspaces, and the label prefix `drydock`. Those
   defaults are right for this deployment. A hand edit to the unit is overwritten by the next
   installer run.
4. **There is no `uninstall`.** [Step 10](#10-upgrade-roll-back-uninstall-logs) lists what to remove
   by hand. The list is derived from what `deploy/install.sh` creates.
5. **v0.2.0's first release run failed, and v0.2.0 is a draft until it is resumed.** The release
   job's test environment lacked `socat`, which CI's had since the broker landed, so the socat half
   of the broker client tests failed (`the broker did not answer`). Nothing was published, and
   *Latest* stayed `v0.1.0`. The two now share one environment. Once that fix is on `main`, finish
   v0.2.0 with [step 2's resume](#resume-a-draft-release); do not merge a new release PR for it.

Three issues listed here earlier are fixed from the release that carries this runbook's
`--ca-cert` flag (v0.2.0, if it is cut after that fix merged — check with
`curl -fsSL https://github.com/krelinga/drydock/releases/latest/download/install.sh | grep -c -- '--ca-cert)'`,
which prints `1`):

- A private CA no longer fails the installer's final check: pass `--ca-cert` ([step 5](#5-install)).
  A certificate the server cannot verify is now reported as exactly that, apart from a real failure.
- A mixed-case `--ui-host` is lowercased by the installer, and Drydock refuses to start on one, rather
  than refusing every sign-in with `forbidden_origin`.
- A release is created as a draft and becomes public and *Latest* only after its assets are uploaded
  and installed by the README one-liner, so a failed release never leaves *Latest* without files
  ([step 2](#2-cut-a-release)).

---

## 1. Before you start

You need five things. Gather all of them before touching the server.

### 1.1 The server

- [ ] **Linux on amd64 (x86_64), with systemd as init.** Releases are built for amd64 only, and the
  installer refuses any other architecture before it downloads anything. It also refuses a host
  without `/run/systemd/system`.
- [ ] **Debian 12/13 or Ubuntu 22.04/24.04** are what this runbook's commands target. The CI
  installer test runs on Debian 12 (bookworm). Other systemd distributions can work, because the
  installer only uses `useradd`, `runuser` and `systemctl`, but the package commands below will
  differ, and SELinux hosts are untested.
- [ ] **Root via `sudo`**, and an SSH session with a real terminal. The installer asks for the
  password on `/dev/tty`.
- [ ] **Ports 80 and 443 free.** Caddy takes both: 443 for the UI, and 80 only to redirect to
  443. Check with `sudo ss -ltnp '( sport = :80 or sport = :443 )'`. It should print only the
  header line.
- [ ] **Outbound internet**: `github.com`, `api.github.com`, `objects.githubusercontent.com`
  (release downloads), `ghcr.io` (the Drydock Feature), `mcr.microsoft.com` (the default dev
  container image), Docker Hub (the cleanup image), and whatever registries your repositories'
  `devcontainer.json` files pull from.
- [ ] **The clock is synchronized.** `timedatectl` must show `System clock synchronized: yes`.
  *Why:* every GitHub App request is a signed JWT with an issue time, and GitHub returns `401` for
  a clock that has drifted far enough.
- [ ] **Disk space for images and clones**: workspaces live under `/srv/drydock/ws` and images
  under `/var/lib/docker`, so tens of GB free. Containers are bounded by RAM: plan for the 5–15
  you expect to run ([§1](../design/overall/drydock-design.md#1-problem--goals)).

### 1.2 A hostname for the UI, and how clients resolve it

- [ ] **Pick a fully qualified, lowercase hostname**, for example `drydock.home.example.com` or
  `drydock.lan`. It must contain a dot, and it cannot be a bare IP address.
  *Why:* Caddy and Drydock both turn away any request whose `Host` is not exactly this name,
  which is the DNS-rebinding defence ([§13.3](../design/overall/drydock-design.md#133--what-a-browser-can-be-talked-into)).
  There is nothing to match against if the UI is reached by IP.
- [ ] **Make every client device resolve it to the server's LAN address.** Pick one:
  - **A local DNS record** on your router, Pi-hole, AdGuard or similar: an `A` record from the
    name to the server's LAN IP. This works for phones, which cannot edit a hosts file.
  - **A public DNS record pointing at a private address** (`drydock.example.com A 192.168.1.20`).
    This only makes sense with a real domain (option B below). Many routers and resolvers
    *filter* public answers that contain private IPs. This is called "DNS rebind protection" in
    dnsmasq, OpenWrt, pfSense/OPNsense and FRITZ!Box. If yours does, allow-list the domain there.
  - `/etc/hosts` on a laptop is fine for a first test, but it does not reach a phone.
  - **Android's "Private DNS"** and browsers' **DNS-over-HTTPS** bypass your local DNS server. If
    one device cannot resolve the name and the others can, check those settings first.
- [ ] **Check from each client:** `nslookup drydock.example.com` (or `dig +short …`) returns the
  server's LAN IP. *This runbook cannot check your DNS for you. Do it on each device.*

### 1.3 A TLS certificate every client trusts for that hostname

The installer does **not** obtain certificates. Drydock relies on externally provisioned ones by
design ([§1 Non-goals](../design/overall/drydock-design.md#non-goals),
[§13.1 *The certificate*](../design/overall/drydock-design.md#the-certificate)). You need two PEM
files on the server: the **certificate**, as the full chain (leaf first, then any intermediates),
and its **private key**. There are three realistic ways to get them for a LAN-only host. Pick one.
This runbook does not pick for you.

| | **A. A private CA, installed on each device** | **B. A public certificate via ACME DNS-01** | **C. Tailscale's `*.ts.net` certificate** |
|---|---|---|---|
| What | Make a CA (`mkcert`, `step-ca`, or `openssl`), issue a cert for your name, and install the CA on every client | Own a real domain whose DNS provider has an API. Let's Encrypt (or another ACME CA) proves control through a TXT record, so the server never needs to be reachable from the internet | `tailscale cert <machine>.<tailnet>.ts.net` issues a Let's Encrypt certificate for the server's tailnet name |
| Name | Anything, including `drydock.lan` | A name under your domain | `<machine>.<tailnet>.ts.net` only |
| Client setup | **Per device.** iOS: install the profile, then turn the CA on under *Settings → General → About → Certificate Trust Settings*. Android: *Install a certificate → CA certificate* (Chrome trusts user CAs; Firefox needs its own setting). Desktops: the OS store, plus Firefox's own store | None. Every device already trusts it | Every client must run Tailscale. Resolution goes through MagicDNS, not your LAN DNS |
| Renewal | You choose the lifetime. Keep it at **825 days or less**, because Apple devices refuse longer. Renewing means re-issuing; the CA stays installed | **Every ~60–90 days, automated** (acme.sh, certbot, lego), then `systemctl reload caddy` | Re-run `tailscale cert` before 90 days, then reload Caddy |
| Exposure | Nothing public. Whoever holds the CA key can impersonate any site to your devices, so name-constrain it and keep its key offline | The name appears in public Certificate Transparency logs, and the private IP is public if you publish it in public DNS | The name appears in CT logs |
| Design fit | Breaks §13.1's assumption that devices trust the certificate "with nothing installed on them". It works, but every new phone is a profile chore | **What §13.1 assumes** | Fits, and §13.1 already names Tailscale as the way in from outside the house |

Whichever you pick:

- [ ] **The certificate's SAN contains the hostname**: run
  `openssl x509 -noout -ext subjectAltName -in <cert.pem>` and check the name appears.
- [ ] **The certificate matches the key.** These two lines must print the same hash:
  ```sh
  openssl x509 -noout -pubkey -in <cert.pem> | sha256sum
  openssl pkey -pubout -in <key.pem> | sha256sum
  ```
- [ ] **Renewal ends with `sudo systemctl reload caddy`.** Caddy reads the files when it loads its
  config and does not watch them.
- [ ] **For option A**, keep the CA certificate (`ca.pem`, *not* its key) handy. The server needs
  it in [step 4.2](#42-the-tls-certificate-and-key), for the installer's final check.

Where the files go on the server, and with what ownership, is
[step 4.2](#42-the-tls-certificate-and-key).

### 1.4 The GitHub App's private key

Drydock uses the **production** App, `krelinga-drydock`, with **App ID `5189455`**. The CI uses
the dev App, `krelinga-drydock-dev` (5189839). Do not use the dev App here: the bot identity
baked into the unit is the production App's ([Known issue 3](#0-known-issues--read-these-first)).

- [ ] **Check the App's permissions** at <https://github.com/settings/apps/krelinga-drydock/permissions>.
  They must be a superset of
  [§9.3](../design/overall/drydock-design.md#93--permissions)'s repository permissions:
  *Actions: read & write*, *Checks: read*, *Contents: read & write*, *Issues: read & write*,
  *Metadata: read*, *Pull requests: read & write*, *Workflows: read & write*.
  *Why:* each token Drydock mints asks for a subset of these, and GitHub refuses a token request
  for a permission the App lacks. A missing `workflows` permission shows up later as a confusing
  push rejection. *This runbook cannot see the App's settings, because the App is private.
  Check them yourself.*
- [ ] **Install the App on the repositories Drydock should see**:
  <https://github.com/settings/apps/krelinga-drydock/installations>. *Only select repositories* is
  the cautious choice. Include at least one small repository that **has** a `devcontainer.json`,
  and one that **does not**. You will clone both in [step 8](#8-first-workspace).
  *Why:* the repository list is exactly the App's installations, so adding a repository to
  Drydock is a GitHub-side action ([§9.4](../design/overall/drydock-design.md#94--what-this-deliberately-gives-up)).
- [ ] **Generate a private key**: on <https://github.com/settings/apps/krelinga-drydock>, go to
  *Private keys → Generate a private key*. Your browser downloads
  `krelinga-drydock.<date>.private-key.pem`. Leave it in your Downloads folder for now.
  **Never paste its contents anywhere**: not into a chat, a terminal, a shell variable or an
  issue. [Step 4](#41-the-github-app-private-key) copies it as a file.
  *Why:* the App key is the one credential Drydock stores. It must never enter the environment or
  a shell history ([§13.5](../design/overall/drydock-design.md#135--non-negotiables)).
  If you lose it, you do not need a backup: generate a new one and delete the old one on the same
  page.

### 1.5 An operator password

- [ ] **At least 12 characters** (`drydock passwd` refuses anything shorter). Keep it in your
  password manager. *Why:* it is the only credential between a device on your Wi-Fi and code
  execution on the server ([§13.5](../design/overall/drydock-design.md#135--non-negotiables)).
  There is no reset route over HTTP. You reset it from a shell on the host
  ([§13.2](../design/overall/drydock-design.md#132--session-authentication)).

---

## 2. Cut a release

The install one-liner always installs the **latest** GitHub release. Today that is `v0.1.0`,
which is the front door only, with no workspaces. Everything this runbook uses is in the open
release-please PR, which is currently titled **`chore(main): release 0.2.0`**. Merging it publishes
`v0.2.0`. If more `feat:` or `fix:` commits land first, the PR's number stays the same and its
title and changelog grow.

> **v0.2.0 specifically:** its release PR (#11) is already merged and its first run failed
> ([Known issue 5](#0-known-issues--read-these-first)), so there is no release PR to merge for it.
> Skip to [Resume a draft release](#resume-a-draft-release), then *Confirm it published*.

Run these from any machine with `gh` signed in as a repository admin:

- [ ] **Find the release PR and read its changelog**:
  ```sh
  gh pr list --repo krelinga/drydock --label "autorelease: pending"
  gh pr view <number> --repo krelinga/drydock
  ```
- [ ] **Check that `main` itself is green**:
  ```sh
  gh run list --repo krelinga/drydock --branch main --workflow ci.yml --limit 1
  ```
- [ ] **Merge it with the admin bypass** ([Known issue 1](#0-known-issues--read-these-first)):
  ```sh
  gh pr merge <number> --repo krelinga/drydock --squash --admin
  ```
  On the web, this is *Merge without waiting for requirements to be met (bypass rules)*.
- [ ] **Watch the release workflow to the end.** It has five jobs: `release-please` tags `vX.Y.Z`
  and creates the Release **as a draft**, `test` runs the whole Go suite in exactly CI's `go`
  environment (both use `.github/actions/go-suite`), `assets` uploads the files to the draft,
  `verify` downloads them back and runs the README one-liner against them, and `publish` makes the
  release public and *Latest*, then checks that `releases/latest/download/install.sh` serves it.
  ```sh
  gh run list --repo krelinga/drydock --workflow release-please.yml --limit 1
  gh run watch <run-id> --repo krelinga/drydock --exit-status
  ```
  **Wait until all five jobs are green. Do not install until `publish` is green.** If any job
  fails, nothing is public: the release stays a draft that only repository admins can see, and the
  one-liner keeps installing the previous release. *Why:* a release that became *Latest* before its
  files were uploaded would make the one-liner 404 for everyone. Confirm that with
  `gh release list --repo krelinga/drydock --limit 3`: the new tag shows `Draft`, the previous one
  `Latest`. Then read the failure with `gh run view <run-id> --repo krelinga/drydock --log-failed`
  and pick the recovery below by what has to change.

#### If the release workflow fails

| What has to change | Do this |
|---|---|
| Nothing: a flaky download, a runner hiccup | `gh run rerun <run-id> --repo krelinga/drydock --failed`. A re-run uses the **workflow file its run started with**, so it cannot pick up a fix. |
| The workflow or its test environment (`.github/workflows/release-please.yml`, `.github/actions/`) | Merge the fix to `main` as `ci:`, then [resume the draft](#resume-a-draft-release). The code at the tag was fine, so it stays the release. |
| The code | Fix forward: merge a `fix:` PR, and release-please opens a release PR for the next patch version; merge that as above. The failed draft can stay, or be removed with `gh release delete vX.Y.Z --repo krelinga/drydock --yes`, which keeps the tag. Keep the tag: release-please finds its previous release by it. |

Do not delete the tag to make release-please try again. Its release PR is already labelled
`autorelease: tagged`, so it would not recreate the release, and without the tag it would plan the
next release from `v0.1.0` again.

#### Resume a draft release

`workflow_dispatch` runs `main`'s release workflow against an existing draft. It skips
release-please and runs `test`, `assets`, `verify` and `publish` on the **code at the tag**, with
**`main`'s workflow and test environment**. It refuses a tag whose release is not a draft, so it
cannot replace a public release's files.

- [ ] **Check the draft and its tag exist**, and that the fix is on `main`:
  ```sh
  gh release view v0.2.0 --repo krelinga/drydock --json isDraft,tagName,assets --jq '{isDraft, tagName, assets: [.assets[].name]}'
  gh api repos/krelinga/drydock/git/ref/tags/v0.2.0 --jq .object.sha
  ```
  `isDraft` must be `true`. Any assets left from an earlier run are replaced (`--clobber`).
- [ ] **Run it and watch it**:
  ```sh
  gh workflow run release-please.yml --repo krelinga/drydock --ref main -f tag=v0.2.0
  gh run list --repo krelinga/drydock --workflow release-please.yml --event workflow_dispatch --limit 1
  gh run watch <run-id> --repo krelinga/drydock --exit-status
  ```
  All five jobs must be green; `release-please` only checks the draft and the tag here.
- [ ] **Confirm it published.** Both of these should print the new tag:
  ```sh
  gh release view --repo krelinga/drydock --json tagName,assets --jq '.tagName, [.assets[].name]'
  curl -fsSL https://github.com/krelinga/drydock/releases/latest/download/install.sh | grep '^RELEASE_VERSION='
  ```
  The assets must be exactly `SHA256SUMS`, `drydock_linux_amd64.tar.gz` and `install.sh`. The
  second command must print `RELEASE_VERSION="v0.2.0"`, or whatever was tagged. If it prints
  `v0.1.0`, or `curl` fails, the upload has not finished.

---

## 3. Prerequisites on the server

SSH to the server. Everything from here on runs **on the server** unless it says *from your
laptop*.

The installer checks for each of these and refuses to run without them. It installs none of them.
*Why:* Docker and the devcontainer CLI are long-lived dependencies that you, the operator, should
own ([§1 Non-goals](../design/overall/drydock-design.md#non-goals)).

```sh
sudo apt-get update
sudo apt-get install -y ca-certificates curl gnupg openssl
```

### 3.1 Docker Engine (official repository)

This is the method from <https://docs.docker.com/engine/install/>. `$ID` from `/etc/os-release`
selects `debian` or `ubuntu`. On a derivative such as Mint or Pop!_OS, use the parent
distribution's name and codename instead.

```sh
. /etc/os-release
sudo install -m 0755 -d /etc/apt/keyrings
sudo curl -fsSL "https://download.docker.com/linux/$ID/gpg" -o /etc/apt/keyrings/docker.asc
sudo chmod a+r /etc/apt/keyrings/docker.asc
echo "deb [arch=$(dpkg --print-architecture) signed-by=/etc/apt/keyrings/docker.asc] https://download.docker.com/linux/$ID $VERSION_CODENAME stable" \
  | sudo tee /etc/apt/sources.list.d/docker.list >/dev/null
sudo apt-get update
sudo apt-get install -y docker-ce docker-ce-cli containerd.io docker-buildx-plugin
sudo systemctl enable --now docker
```

> Debian's own `docker.io` package also satisfies the installer (the CI installer test uses it).
> Docker's packages include the buildx plugin, which the devcontainer CLI uses to build Features.
> That is why this runbook uses them.

- [ ] **Verify:**
  ```sh
  systemctl is-active docker          # active
  getent group docker                 # docker:x:<gid>:   (the installer needs this group)
  sudo docker run --rm hello-world    # "Hello from Docker!"
  sudo docker buildx version
  ```

**Do not add your own login to the `docker` group** unless you mean it. That group is
root-equivalent ([§13.4](../design/overall/drydock-design.md#134--blast-radius)). This runbook uses
`sudo docker` throughout.

### 3.2 Node.js 20+ and the devcontainer CLI, on the service's `PATH`

The service runs with exactly this `PATH` and nothing else:

```
/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin
```

The installer resolves `devcontainer` on that `PATH`, and then runs `devcontainer --version` **as
the `drydock` user**. So `node` and `devcontainer` must be in `/usr/bin` or `/usr/local/bin`.
**nvm, fnm, volta, snap (`/snap/bin`) and anything in a home directory are all refused.**

The distributions' own `nodejs` is too old on Debian 12 (18), Ubuntu 24.04 (18) and Ubuntu 22.04
(12): the CLI requires `node >= 20`. Use NodeSource's repository, which installs `node` and `npm` to `/usr/bin`, so `npm -g`
puts `devcontainer` in `/usr/bin`:

```sh
curl -fsSL https://deb.nodesource.com/setup_22.x | sudo -E bash -
sudo apt-get install -y nodejs
sudo npm install -g @devcontainers/cli@0.89.0
```

> On Debian 13 (trixie), the distribution's `nodejs` and `npm` are already 20+:
> `sudo apt-get install -y nodejs npm`, then the same `npm install -g` command, which lands in
> `/usr/local/bin`. The CI installer test instead unpacks the nodejs.org tarball into
> `/usr/local`, which also works.

*Why `0.89.0` exactly:* every measured fact the container code relies on (no `--json` flag, the
lockfile behaviour, `read-configuration`'s output) was recorded on CLI `0.89.0`
([§6](../design/overall/drydock-design.md#6-clone--container)), and CI pins the same version.

- [ ] **Verify, as the service will run it:**
  ```sh
  PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin command -v node devcontainer
  PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin node --version          # v20 or later
  PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin devcontainer --version  # 0.89.0
  ```

### 3.3 Caddy, from its official package

The package is what provides the `caddy` user and the `caddy.service` unit that the installer
extends. The commands are from <https://caddyserver.com/docs/install#debian-ubuntu-raspbian>:

```sh
sudo apt-get install -y debian-keyring debian-archive-keyring apt-transport-https
curl -1sLf 'https://dl.cloudsmith.io/public/caddy/stable/gpg.key' \
  | sudo gpg --dearmor -o /usr/share/keyrings/caddy-stable-archive-keyring.gpg
curl -1sLf 'https://dl.cloudsmith.io/public/caddy/stable/debian.deb.txt' \
  | sudo tee /etc/apt/sources.list.d/caddy-stable.list
sudo chmod o+r /usr/share/keyrings/caddy-stable-archive-keyring.gpg /etc/apt/sources.list.d/caddy-stable.list
sudo apt-get update
sudo apt-get install -y caddy
```

- [ ] **Verify:**
  ```sh
  caddy version                                   # v2.x
  id caddy                                        # the caddy user exists
  systemctl is-active caddy                       # active (serving the package's placeholder page)
  grep -c "easy way to configure" /etc/caddy/Caddyfile   # 1: the stock file, safe to replace
  ```
- [ ] **If `/etc/caddy/Caddyfile` is not the stock one**, because you already serve other sites with
  this Caddy, stop here and decide. Drydock needs Caddy to itself: its Caddyfile carries a global
  options block that moves the admin API off `localhost:2019`, so it cannot be imported into
  someone else's ([§13.1](../design/overall/drydock-design.md#131--the-front-door)). The installer
  refuses a foreign Caddyfile unless you pass `--take-over-caddy`, which backs it up and then
  replaces it.

### 3.4 Firewall

If the host runs a firewall, allow 443 (and optionally 80) from the LAN. With `ufw`, for example:
`sudo ufw allow from 192.168.1.0/24 to any port 443 proto tcp`. Drydock itself listens on no TCP
port, so **open nothing else**. *Why:* Caddy is the whole LAN-facing surface, and Drydock is
reachable only through a Unix socket
([§13.1](../design/overall/drydock-design.md#131--the-front-door)).

---

## 4. Get the secrets onto the server without leaking them

Two rules for everything in this step:

- **Copy files; never paste their contents.** A key that has been in a terminal, a clipboard, a chat,
  a shell variable or an environment variable is in scrollback, history files, `/proc` and crash
  reports.
- **Never `cat` a key.**

### 4.1 The GitHub App private key

- [ ] **On the server**, make a private drop directory in your home:
  ```sh
  install -d -m 0700 ~/drydock-drop
  ```
- [ ] **From your laptop**, copy the key into it:
  ```sh
  scp ~/Downloads/krelinga-drydock.*.private-key.pem <you>@<server>:drydock-drop/app.pem
  ```
- [ ] **On the server**, move it to a root-owned temporary path, then remove your copy:
  ```sh
  sudo install -m 0400 -o root -g root ~/drydock-drop/app.pem /root/drydock-app.pem
  shred -fu ~/drydock-drop/app.pem && rmdir ~/drydock-drop
  sudo head -c 40 /root/drydock-app.pem; echo   # -----BEGIN RSA PRIVATE KEY----- (the header only)
  ```
  The installer copies this file to `/etc/drydock/github-app.pem` (mode `0400`, owner `drydock`)
  in [step 5](#5-install). You delete `/root/drydock-app.pem` right after that.
- [ ] **On your laptop**, delete the download, or move it into your password manager as an
  attachment. GitHub can always issue a new key, so you do not need to keep this one.

> `shred` is best-effort on SSDs and journaling filesystems. The real protection is that the file
> was `0700`/`0400` the whole time and existed for minutes.

### 4.2 The TLS certificate and key

Caddy runs as the `caddy` user and must be able to **read both files and traverse every
directory above them**. The installer checks this, as the `caddy` user, before it changes
anything. **Do not use `/etc/ssl/private`:** it is `0710 root:ssl-cert` on Debian, and `caddy` is
not in `ssl-cert`.

- [ ] **Put them in a directory that only root and Caddy can read**. Copy them the same way as the
  App key: `scp` into `~/drydock-drop`, then `install`. The paths below are this runbook's
  convention; any absolute path works.
  ```sh
  sudo install -d -m 0750 -o root -g caddy /etc/caddy/certs
  sudo install -m 0644 -o root -g caddy ~/drydock-drop/fullchain.pem /etc/caddy/certs/drydock.crt
  sudo install -m 0640 -o root -g caddy ~/drydock-drop/privkey.pem   /etc/caddy/certs/drydock.key
  shred -fu ~/drydock-drop/privkey.pem; rm -f ~/drydock-drop/fullchain.pem; rmdir ~/drydock-drop
  ```
  If your ACME client writes the files itself, point it at these paths, or at its own paths with
  the same group and mode. Then check that a renewal keeps the group and mode.
- [ ] **Verify, as Caddy will read them:**
  ```sh
  sudo runuser -u caddy -- test -r /etc/caddy/certs/drydock.crt && echo cert-ok
  sudo runuser -u caddy -- test -r /etc/caddy/certs/drydock.key && echo key-ok
  ```
- [ ] **Option A (private CA) only**: put the CA's **certificate** beside them, for the installer's
  final check ([step 5](#5-install) passes it as `--ca-cert`). Copy it the same way; it is public,
  so `0644` is fine:
  ```sh
  sudo install -m 0644 -o root -g caddy ~/drydock-drop/ca.pem /etc/caddy/certs/drydock-ca.pem   # the CA cert, never its key
  ```
  *Why:* the installer ends by fetching `https://drydock.example.com/api/auth/session` through
  Caddy with the server's own `curl`, and it verifies the certificate. The server does not trust
  your CA, so without `--ca-cert` that check cannot pass. The installer keeps the path in
  `/etc/drydock/drydock.env` for later runs, so keep the file there. It refuses a file that holds a
  private key. (Adding the CA to the system store with `update-ca-certificates` also works, but it
  makes every program on the server trust your CA, which the check does not need.)

---

## 5. Install

- [ ] **Prime `sudo`**, so that its password prompt does not interleave with the installer's
  output:
  ```sh
  sudo -v
  ```
- [ ] **Run the one-liner** with every flag. This is the README's line plus the App:
  ```sh
  curl -fsSL https://github.com/krelinga/drydock/releases/latest/download/install.sh \
    | sudo bash -s -- \
        --ui-host drydock.example.com \
        --cert /etc/caddy/certs/drydock.crt \
        --key /etc/caddy/certs/drydock.key \
        --github-app-id 5189455 \
        --github-app-key /root/drydock-app.pem
  ```
  **Option A (private CA):** add one more flag, `--ca-cert /etc/caddy/certs/drydock-ca.pem`. With a
  publicly trusted certificate (options B and C), leave it out.

  Do **not** pass `--preview-domain`. Previews are not built yet ([step 9](#9-what-does-not-work-yet)).

**What it does, in order.** It stops at the first failure, and nothing below the failure is
changed:

1. It downloads `drydock_linux_amd64.tar.gz` and `SHA256SUMS` for the tag stamped into this
   `install.sh`, checks the checksum, and runs the `install.sh` inside the tarball. Output starts
   with `==> downloading drydock_linux_amd64.tar.gz (v0.2.0)`.
2. It checks prerequisites: root, Linux, systemd, `caddy` and the `caddy` user, `curl`, `docker`,
   the `docker` group, and `devcontainer` on the service `PATH`.
3. It validates flags: the hostname must contain a dot, and is lowercased (it says so if that
   changed it); certificate paths must be absolute files; `--ca-cert` must be a certificate, not a key;
   the App ID must be numeric (the Client ID starts with `Iv` and is refused); the key must look
   like a PEM private key.
4. It checks that the `caddy` user can read the certificate and key.
5. It decides about the Caddyfile. The stock file is backed up and replaced:
   `==> replacing the Caddy package's default Caddyfile (saved to /etc/caddy/Caddyfile.before-drydock.<timestamp>)`.
6. `==> creating group drydock` and `==> creating user drydock`: a system account with home
   `/var/lib/drydock` and shell `nologin`.
7. `==> adding drydock to the docker group (root-equivalent; see design §13.4)`. Then it runs
   `devcontainer --version` as `drydock` (and fails if that does not run), and `docker info` as
   `drydock` (only a *warning* if the daemon is unreachable).
8. It creates `/srv/drydock` (root, `0755`, only if it is absent) and `/srv/drydock/ws`
   (`drydock`, `0700`).
9. It installs `/usr/local/bin/drydock`.
10. `==> installed the GitHub App key at /etc/drydock/github-app.pem (mode 0400, owner drydock)`.
11. `==> created the secrets master key at /etc/drydock/secrets.key (mode 0400, owner drydock); back it up — without it no stored secret can be read`.
12. It writes `/etc/drydock/drydock.env`, `/etc/systemd/system/drydock.service`,
    `/etc/systemd/system/caddy.service.d/drydock.conf`, `/etc/caddy/Caddyfile` and the directory
    `/etc/caddy/drydock.d/`. The new Caddy config is validated with `caddy validate` **before**
    anything under `/etc/caddy` changes.
13. `==> starting drydock`, then `==> restarting caddy`.
14. **It asks for the operator password**: `New password:` and `Again:`, without echo. Then
    `Password set. Every existing session has been signed out.`
15. It runs the end-to-end check: `https://drydock.example.com/api/auth/session` through Caddy,
    resolved to `127.0.0.1`, must answer `401` over a certificate it verified, against
    `--ca-cert` if you gave one and the system's trust store otherwise. It prints nothing when the
    check passes. If everything is running and only the certificate could not be verified, it says
    exactly that, `Drydock is installed and running, and answers through Caddy, but this host could
    not verify the certificate …`, with `curl`'s reason; see [§11](#11-troubleshooting).
16. `==> installed Drydock v0.2.0` and `==> open https://drydock.example.com`.

- [ ] **Delete the temporary App key**:
  ```sh
  sudo shred -u /root/drydock-app.pem
  ```

**If the password step was skipped or refused**, the installer only warns. For example, the
password was under 12 characters, or the two entries did not match. Set it with:

```sh
sudo -u drydock drydock passwd --db /var/lib/drydock/drydock.db
```

**What now exists.** This is the whole footprint, which is useful for [uninstalling](#104-uninstall):

| Path / object | Owner, mode | What |
|---|---|---|
| user and group `drydock` | system | The service account. Member of `docker`. No login shell. |
| `caddy` user | (from the package) | Gains supplementary group `drydock` through the drop-in, so it can reach the socket. |
| `/usr/local/bin/drydock` | root `0755` | The binary. `drydock.previous` appears after an upgrade. |
| `/etc/drydock/drydock.env` | root `0644` | Settings: hostnames, cert paths, the `--ca-cert` path, App ID. Nothing secret. Parsed by the installer, never `source`d. |
| `/etc/drydock/github-app.pem` | drydock `0400` | The App key. Passed to Drydock as a path. |
| `/etc/drydock/secrets.key` | drydock `0400` | The secrets master key: 32 random bytes. **[Back it up](#6-back-up-the-secrets-master-key-now).** |
| `/etc/systemd/system/drydock.service` | root `0644` | `drydock serve …` as `drydock`, `ProtectSystem=strict`, the fixed `PATH`. `.previous` is kept for rollback. |
| `/etc/systemd/system/caddy.service.d/drydock.conf` | root `0644` | Caddy's `EnvironmentFile`, `SupplementaryGroups=drydock`, and `/run/caddy`. |
| `/etc/caddy/Caddyfile` | root `0644` | Drydock's. The original is kept at `/etc/caddy/Caddyfile.before-drydock.<timestamp>`. |
| `/etc/caddy/drydock.d/` | root `0755` | Optional sites. Empty unless previews are configured. |
| `/var/lib/drydock/drydock.db` | drydock, dir `0700` | SQLite (WAL). Created by systemd's `StateDirectory`. |
| `/run/drydock/{http,preview}.sock` | drydock:drydock `0660` | The two Unix sockets. The directory is `0750`. |
| `/run/drydock/sock/` | drydock `0700` | One token-broker socket per running workspace. |
| `/run/caddy/admin.sock` | caddy `0600` | Caddy's admin API, moved off `localhost:2019`. |
| `/srv/drydock/ws/` | drydock `0700` | Clones, at `/srv/drydock/ws/<id>/repo`. |
| `drydock.service`, `caddy.service` | enabled | Both start at boot. |

---

## 6. Back up the secrets master key, now

Do this before you store a single secret.

*Why:* every repository secret is encrypted with XChaCha20-Poly1305 under this key
([§10.2](../design/overall/drydock-design.md#102--storage)). **If you lose it, every stored secret
is unreadable**, and the only recovery is to type each value in again. Re-runs and upgrades keep
it. If the file is damaged, the installer refuses to run rather than quietly generating a new one.

Keep the backup **apart from any backup of the database**. A backup that holds both holds every
secret in plaintext-equivalent form.

- [ ] **On the server**, make a copy that your login user can read:
  ```sh
  sudo sha256sum /etc/drydock/secrets.key            # note the hash: it is safe to show, the key is not
  install -d -m 0700 ~/drydock-drop
  sudo install -m 0400 -o "$USER" /etc/drydock/secrets.key ~/drydock-drop/secrets.key
  ```
- [ ] **From your laptop**, pull it and check it:
  ```sh
  scp <you>@<server>:drydock-drop/secrets.key ./drydock-secrets.key
  sha256sum ./drydock-secrets.key                      # must equal the server's hash
  ```
- [ ] **Store it somewhere durable and private**: an attachment in your password manager, or an
  encrypted volume. Then delete the loose copies:
  ```sh
  shred -fu ./drydock-secrets.key                      # laptop, after it is stored (-f: the file is 0400)
  shred -fu ~/drydock-drop/secrets.key && rmdir ~/drydock-drop  # server
  ```
- [ ] **To restore it later**, for example on a rebuilt server, copy the backup to the server the
  way [4.1](#41-the-github-app-private-key) copies the App key, then:
  ```sh
  sudo install -d -m 0755 /etc/drydock
  sudo install -m 0400 -o drydock -g drydock ./secrets.key /etc/drydock/secrets.key   # after the drydock user exists
  sudo systemctl restart drydock
  ```
  On a brand-new host, put the file in place with owner `root` before the first install. The
  installer keeps a valid 32-byte file and corrects its owner and mode.

---

## 7. Verify

### 7.1 On the server

- [ ] **Both services are active**:
  ```sh
  systemctl status drydock caddy --no-pager
  ```
  Both should be `active (running)`. Then
  `ps -o user= -p "$(systemctl show -p MainPID --value drydock)"` prints `drydock`.
- [ ] **The installer's own check**, run by hand. This must print `401`:
  ```sh
  curl -s -o /dev/null -w '%{http_code}\n' --resolve drydock.example.com:443:127.0.0.1 \
    https://drydock.example.com/api/auth/session
  ```
  For option A, add `--cacert /etc/caddy/certs/drydock-ca.pem`. `000` means the TLS
  handshake failed or nothing answered ([§11](#11-troubleshooting)).
- [ ] **Drydock's journal is quiet**:
  ```sh
  journalctl -u drydock -b --no-pager -o cat
  ```
  On a healthy start, the **only** line is:
  ```
  drydock: serving on /run/drydock/http.sock and /run/drydock/preview.sock
  ```
  Boot reconciliation and the first catalog refresh log **only when they fail**. So silence is
  success, and any of these lines is a problem:
  - `drydock: reconcile: …`: Drydock cannot list containers. This is usually Docker access
    ([§11](#11-troubleshooting)).
  - `drydock: catalog refresh: github: GET /app/installations: 401 …`: the App ID and key do not
    belong together, or the clock is off.
  - `drydock serve: …` followed by a restart loop: a startup refusal, for example a key file
    whose mode is too open, or a database newer than the binary.
- [ ] **Caddy is not listening on the admin port**, and nothing else listens:
  ```sh
  sudo ss -ltnp | grep -E ':(80|443|2019)\b'
  ```
  Only Caddy should appear, on `*:80` and `*:443`, with **no** `2019`, and no `drydock` line
  anywhere. *Why:* an admin API on loopback would let any local process reconfigure the one
  process that can reach Drydock's socket
  ([§13.1](../design/overall/drydock-design.md#131--the-front-door)).
- [ ] **The secrets key is in place** and was never printed:
  `sudo stat -c '%U %a %s' /etc/drydock/secrets.key` prints `drydock 400 32`.

### 7.2 From a laptop

- [ ] **The name resolves**: `nslookup drydock.example.com` returns the server's LAN IP.
- [ ] **TLS and the gate**: `curl -s -o /dev/null -w '%{http_code}\n' https://drydock.example.com/api/auth/session`
  prints `401`. With a private CA, add `--cacert ca.pem`. macOS's `curl` does not read the
  Keychain.
- [ ] **A foreign name gets nothing**: `curl -sk --resolve evil.test:443:<server-ip> https://evil.test/`
  fails with a TLS error and never returns the UI.
- [ ] **Sign in in a browser.** Open `https://drydock.example.com`. You get the sign-in page with no
  certificate warning. Enter the password. You land on **Workspaces**.
- [ ] **The repository list populates.** Under **All repositories**, you may first see
  *Reading the repository list from GitHub…*, and then the repositories the App is installed on.
  Repositories with a dev container carry a **dev container** badge.
  - *No GitHub App is set up yet…* means the unit has no App flags: see `app_not_configured` in
    [§11](#11-troubleshooting).
  - *Could not refresh the repository list from GitHub: …* means GitHub refused. The message
    carries GitHub's reason.
  - An empty list with no error means the App is installed on nothing. Go back to
    [step 1.4](#14-the-github-apps-private-key).
- [ ] **Settings → Repository catalog → Refresh catalog** reports
  *Refreshed … : N repositories.*
- [ ] **Settings** lists this browser as a device.

### 7.3 From a phone

- [ ] On the phone's own Wi-Fi (not mobile data), open `https://drydock.example.com`. Check that
  there is no certificate warning, then sign in. The phone now appears as a second device in
  **Settings**. This is Phase 1's acceptance test: *"You can sign in from your phone over HTTPS"*
  ([§14](../design/overall/drydock-design.md#14-build-plan)).
- [ ] If the phone fails and the laptop works, the cause is almost always DNS (Private DNS or
  rebind filtering, [step 1.2](#12-a-hostname-for-the-ui-and-how-clients-resolve-it)) or trust
  (a CA profile installed but not enabled for full trust).

---

## 8. First workspace

Use two **small** repositories that the App is installed on: **A** has a `devcontainer.json`, and
**B** has none. The first build pulls images. Expect several minutes for B the first time, because
`mcr.microsoft.com/devcontainers/base:debian` plus the Drydock Feature are large. A rebuild is much
faster.

Keep a terminal open on the server with `journalctl -u drydock -f -o cat` while you do this. It
should stay quiet.

### 8.1 Clone and watch

- [ ] On **Workspaces**, find **B** under *All repositories* and press **Clone**. The row moves
  through *Waiting to start → Cloning → Building → Running*, and the workspace appears under
  **Running**.
- [ ] Open it (the repository name is a link to `/ws/<id>`). Note the **id** in the URL: it is a
  26-character ULID. **Steps** lists the eight steps from
  [§6](../design/overall/drydock-design.md#6-clone--container): allocating, cloning, resolving
  config (B gets Drydock's minimal config, and the step says so), preparing credentials (*a
  recorded no-op until Phase 5*), opening the broker socket, starting the container, verifying,
  and starting the session server (*a recorded no-op until Phase 5*). Each step reaches *done*.
  **Recent events** shows the same.
- [ ] Do the same for **A**. If A commits a `devcontainer-lock.json` that is stale, the
  *starting the container* step says that `devcontainer up` rewrote it in the clone. That is
  expected, and it is what VS Code would do too
  ([§6 *The repository's lockfile*](../design/overall/drydock-design.md#the-repositorys-lockfile)).
- [ ] **On the server**, the container is there, found by label:
  ```sh
  sudo docker ps --filter label=drydock.workspace --format '{{.ID}}  {{.Label "drydock.repo"}}  {{.Status}}'
  sudo ls -l /run/drydock/sock/      # one <id>.sock per running workspace
  ```

### 8.2 GitHub access inside the container (Phase 3)

Set the workspace id from the URL, and find its container. `vscode` is the remote user of B's
minimal config. For A, use its `remoteUser`. The clone is mounted at `/workspaces/repo` unless the
repository's config sets its own `workspaceFolder`.

```sh
WS=<workspace-id>
CID=$(sudo docker ps -q --filter "label=drydock.workspace=$WS")
sudo docker exec -u vscode -w /workspaces/repo "$CID" git remote -v      # plain https://github.com/<owner>/<repo>.git, no token
sudo docker exec -u vscode -w /workspaces/repo "$CID" git fetch          # works: the broker minted a token
sudo docker exec -u vscode -w /workspaces/repo "$CID" gh repo view --json nameWithOwner --jq .nameWithOwner
sudo docker exec -u vscode -w /workspaces/repo "$CID" gh repo view <owner>/<A>   # another PRIVATE repo, even one the App is installed on: not found
```

Use a *private* repository for that last check. An installation token can still read public
repositories, as any anonymous caller can.

Optional. This pushes a branch to GitHub, then deletes it:

```sh
sudo docker exec -u vscode -w /workspaces/repo "$CID" git push origin HEAD:refs/heads/drydock/smoke-test   # allowed
sudo docker exec -u vscode -w /workspaces/repo "$CID" git push origin HEAD:refs/heads/smoke-test           # refused: "pushes go under refs/heads/drydock/"
sudo docker exec -u vscode -w /workspaces/repo "$CID" git push origin --delete drydock/smoke-test
```

*Why the prefix:* agent branches live under `drydock/`, which the Feature enforces with a
`pre-push` hook ([§9.3](../design/overall/drydock-design.md#93--permissions)).

### 8.3 A secret (Phase 4)

There is no Claude session yet ([step 9](#9-what-does-not-work-yet)), so prove delivery the way a
session would get it. Run the same one-line prelude, `CLAUDE_ENV_FILE`, that Claude Code runs
before every command ([§10.3](../design/overall/drydock-design.md#103--delivery)).

- [ ] **Secrets → New secret.** Fill in:
  - Name: `SMOKE_TEST_VALUE`. Names are `[A-Z_][A-Z0-9_]*`. Prefixes such as `DRYDOCK_`, `GH_`,
    `GIT_`, `CLAUDE_` and `ANTHROPIC_` are reserved and refused.
  - Value: `hello-from-drydock`. Use a throwaway value, because you are about to compare it in a
    terminal.
  - *What can someone do with this?*: `nothing; a smoke test`.
  - Save. It is **granted to nothing** yet: secrets are default-deny.
- [ ] **Before granting**, check that the workspace receives nothing:
  ```sh
  sudo docker exec -u vscode "$CID" sh -c '. "$CLAUDE_ENV_FILE"; echo "value=${SMOKE_TEST_VALUE:-<unset>}"'; echo "exit=$?"
  ```
  Expect `value=<unset>` and `exit=0`. This is the control: the fetch succeeded and carried
  nothing.
- [ ] On the secret's page, under **Granted to**, choose B's repository and **Save grants**.
- [ ] **Run the same command again.** Expect `value=hello-from-drydock` and `exit=0`. Nothing was
  restarted: the grant set is resolved per command.
- [ ] Grant it **away** again (or delete the secret), and the next run is back to `<unset>`.
- [ ] Reload **Secrets**. The secret shows *Last fetched … by* this workspace, and its page has a
  **Fetched by** section. The value itself never appears in any event, list or form. That is by
  design ([§13.5](../design/overall/drydock-design.md#135--non-negotiables)).

> If the command prints `drydock: secrets unavailable: …` and the shell exits `69`, delivery
> failed *closed*, which is on purpose: a broker outage must fail the command loudly. The reason
> is in the message and in Drydock's event log.

### 8.4 Stop, start, rebuild, delete (Phase 6 server side)

- [ ] **Stop** (on the workspace's card or detail page). It moves to *Stopped*. On the server, the
  container is stopped but still exists (`sudo docker ps -a --filter label=drydock.workspace=$WS`),
  and `/run/drydock/sock/$WS.sock` is **gone**. *Why:* GitHub access follows Drydock's state, not
  Docker's ([§9.1](../design/overall/drydock-design.md#91--why-a-broker-rather-than-an-injected-token)).
- [ ] **Start**. It resumes from *resolving config*, reattaches to the same container, and is
  *Running* again with its socket back.
- [ ] **Rebuild** (detail page → *Actions*). The container is replaced (it gets a new container
  ID) and the clone is kept.
- [ ] **Delete workspace…** (detail page → *Actions*). Type the repository's full name to confirm.
  The container, the clone under `/srv/drydock/ws/$WS` and the broker socket all go:
  ```sh
  sudo docker ps -a --filter label=drydock.workspace=$WS     # empty
  sudo ls /srv/drydock/ws                                   # no $WS
  ```
  If files inside the clone are root-owned, the delete runs a pinned `busybox` helper to remove
  them, and the *files* sub-step says so. The first such delete pulls the image.

### 8.5 Optional: the reboot drill

This is the one Phase 2 check that has not yet been run on real hardware
([§14](../design/overall/drydock-design.md#14-build-plan), testing §11.5).

- [ ] With one workspace *Running*, run `sudo reboot`.
- [ ] After boot, both services are active and the UI loads. The workspace shows *Stopped*: Docker
  does not restart dev containers, and Drydock never auto-starts one
  ([§6 Reconciliation on boot](../design/overall/drydock-design.md#reconciliation-on-boot)).
  **Start** brings it back with its clone intact.
- [ ] `journalctl -u drydock -b -o cat` has no `reconcile:` line.

---

## 9. What does not work yet

None of the following is a deployment fault. These are the phases still being built
([§14](../design/overall/drydock-design.md#14-build-plan)):

- **No Claude at all (Phase 5).** Containers do not get Claude Code. No `claude remote-control`
  session server runs. There is no login handshake, no shared credential volume, no session
  discovery, and no expiry watch. The *preparing credentials* and *starting the session server*
  steps are recorded no-ops. The routes behind this answer `501` once you are signed in:
  `/api/auth/claude…`, `/api/workspaces/{id}/supervisor`, `/api/workspaces/{id}/logs`. Nothing
  appears in the Claude app.
- **No previews (port forwarding).** Every route on the preview socket answers `501`. Leave
  `--preview-domain` unset. A preview wildcard certificate buys nothing yet.
- **Phase 6 is partly done.** Stop, rebuild and delete work. These do not exist yet: memory and
  disk per workspace on the card, the live session count, the log viewer, and the failure-mode
  messages from §12.
- **Secret staleness is always *"applies to new commands"*.** It is truthful until Phase 5 adds a
  process that holds a frozen environment.
- **The container cap (10) is fixed** by the installer ([Known issue 3](#0-known-issues--read-these-first)).
  A create beyond the cap is refused, and you choose what to stop.

---

## 10. Upgrade, roll back, uninstall, logs

### 10.1 Upgrade

- [ ] Wait for a release to finish publishing ([step 2](#2-cut-a-release)).
- [ ] **Back up the database first** ([Known issue 2](#0-known-issues--read-these-first)). Stopping
  the service briefly gives a clean, checkpointed file:
  ```sh
  sudo systemctl stop drydock
  sudo install -m 0600 -o root -g root /var/lib/drydock/drydock.db /root/drydock.db.$(date +%Y%m%d%H%M%S)
  sudo systemctl start drydock
  ```
  Keep this backup away from the master key backup.
- [ ] **Run the same line with no flags.** The settings come from `/etc/drydock/drydock.env`:
  ```sh
  sudo -v
  curl -fsSL https://github.com/krelinga/drydock/releases/latest/download/install.sh | sudo bash
  ```
  A `--ca-cert` given before is kept, so the line needs no flags. The run ends with
  `==> upgraded Drydock vA -> vB`, or `==> Drydock vX is installed and current` when there was
  nothing to do. A re-run restarts only what changed. It never asks for the password again
  (`--if-unset`), so no one is signed out. It keeps the App key and the master key.
- [ ] **To change a setting**, re-run with only that flag, for example
  `… | sudo bash -s -- --cert /new/path.crt --key /new/path.key`. To replace the App key, pass
  `--github-app-id 5189455 --github-app-key <new.pem>`, then revoke the old key on GitHub. Moving
  from a private CA to a public certificate: `--no-ca-cert` with the new `--cert` and `--key`.
- [ ] **A specific release:** `… | sudo bash -s -- --version vX.Y.Z`.

### 10.2 Roll back

- **Automatic:** if the new binary does not start, the installer puts back
  `/usr/local/bin/drydock.previous`, and `drydock.service.previous` if the unit changed. It then
  restarts the service and exits with `the upgrade failed and was rolled back to vA; see the log
  above`. That works **unless the new binary already migrated the database**
  ([Known issue 2](#0-known-issues--read-these-first)).
- **Manual**, to an older release:
  ```sh
  sudo systemctl stop drydock
  sudo install -m 0600 -o drydock -g drydock /root/drydock.db.<timestamp> /var/lib/drydock/drydock.db
  sudo rm -f /var/lib/drydock/drydock.db-wal /var/lib/drydock/drydock.db-shm
  curl -fsSL https://github.com/krelinga/drydock/releases/latest/download/install.sh | sudo bash -s -- --version vA.B.C
  ```
  Restore the matching database backup first, because an older binary refuses a newer schema.
  The `--version` release's own installer then does the install. Rolling back to `v0.1.0` is not
  useful: its installer predates the App and secrets flags.

### 10.3 Logs and state

| What | Where |
|---|---|
| Drydock's process log | `journalctl -u drydock` (`-f` to follow, `-b` for this boot, `-o cat` for plain lines). Quiet when healthy: see [7.1](#71-on-the-server). |
| Caddy | `journalctl -u caddy`. There is no access log; that is deliberate ([§13.5](../design/overall/drydock-design.md#135--non-negotiables), redact by default). |
| Per-workspace steps and events | In the UI: the workspace page's **Steps** and **Recent events**. Details are Drydock's own sentences. A subprocess's stderr goes to the journal as `drydock: workspace <id>: …`, never to the event log. |
| Docker | `sudo docker ps -a --filter label=drydock.workspace`, `sudo docker logs <cid>` |
| Database | `/var/lib/drydock/drydock.db` (SQLite, WAL). Read it only as root, and only with `drydock` stopped if you are copying it. |
| Settings | `/etc/drydock/drydock.env`. The effective command line is in `systemctl cat drydock`. |

To change the password: `sudo -u drydock drydock passwd --db /var/lib/drydock/drydock.db`. This
signs out every device. A device can also be signed out from **Settings**.

### 10.4 Uninstall

There is no uninstall command. This list is derived from what the installer creates
([step 5](#5-install)).

```sh
# 1. Delete every workspace in the UI first. That removes containers, clones and sockets properly.
#    Anything left over, by label:
sudo docker ps -aq --filter label=drydock.workspace | xargs -r sudo docker rm -f
# 2. Drydock itself
sudo systemctl disable --now drydock
sudo rm -f /etc/systemd/system/drydock.service /etc/systemd/system/drydock.service.previous
sudo rm -f /usr/local/bin/drydock /usr/local/bin/drydock.previous
# 3. Give Caddy back: drop-in, Caddyfile, sites directory
sudo rm -f /etc/systemd/system/caddy.service.d/drydock.conf
sudo cp -p "$(ls -1 /etc/caddy/Caddyfile.before-drydock.* | head -n1)" /etc/caddy/Caddyfile   # the oldest backup is the original
sudo rm -rf /etc/caddy/drydock.d
sudo systemctl daemon-reload && sudo systemctl restart caddy
# 4. Data and keys. Save secrets.key and the DB first if you might come back.
sudo rm -rf /srv/drydock/ws /var/lib/drydock
sudo shred -u /etc/drydock/secrets.key /etc/drydock/github-app.pem
sudo rm -rf /etc/drydock
# 5. The account (userdel also removes the drydock group when it is the user's own)
sudo userdel drydock; getent group drydock && sudo groupdel drydock
```

Then revoke the App key on GitHub (*Private keys → Delete*). Docker, Node, the devcontainer CLI
and Caddy stay installed, because you installed them.

---

## 11. Troubleshooting

| Symptom | Likely cause | Fix |
|---|---|---|
| Installer: `Docker is not installed` / `there is no 'docker' group` | Docker is missing, or was installed without its group | [3.1](#31-docker-engine-official-repository) |
| Installer: `the devcontainer CLI is not installed where the service can find it` | `devcontainer` is under nvm, snap or a home directory | Install Node from NodeSource and run `npm install -g` as root ([3.2](#32-nodejs-20-and-the-devcontainer-cli-on-the-services-path)). Check with the `PATH=… command -v` line there. |
| Installer: `…installed but does not run as the drydock user` | Node older than 20 on `/usr/bin` | `node --version` with the service `PATH`. Upgrade Node. |
| Installer warning: `the drydock user cannot reach the Docker daemon yet` | The daemon is stopped | `sudo systemctl enable --now docker`, then re-run the installer |
| Installer: `the caddy user cannot read …` | Key in `/etc/ssl/private`, or mode `0600` | [4.2](#42-the-tls-certificate-and-key): group `caddy`, mode `0640`, directory `0750 root:caddy` |
| Installer: `/etc/caddy/Caddyfile was not written by this installer` | Caddy already serves other sites | Move them elsewhere, or `--take-over-caddy` (a backup is kept) |
| Installer: `the new Caddy configuration does not validate` | Bad certificate or key file, or a path typo | Read the five lines above the error. Nothing under `/etc/caddy` was changed. |
| Installer: `--github-app-id must be the numeric App ID` | The Client ID (`Iv…`) was given | `--github-app-id 5189455` |
| Installer: `Drydock is installed and running, and answers through Caddy, but this host could not verify the certificate …` | Everything works except certificate verification. `unable to get local issuer certificate`: a private CA without `--ca-cert`, or the wrong CA file, or a public certificate whose `--cert` file lacks the intermediates. `no alternative certificate subject name matches`: the certificate is for another name | Option A: re-run with `--ca-cert /etc/caddy/certs/drydock-ca.pem` ([4.2](#42-the-tls-certificate-and-key)). Otherwise check the SAN and the full chain ([1.3](#13-a-tls-certificate-every-client-trusts-for-that-hostname)). Everything else is installed, so a re-run with the fix is all it takes. |
| Installer: `end-to-end check failed … answered '000'` | Nothing answered over TLS: Caddy is not serving this name, or the handshake failed (`curl`'s reason is in the message) | `journalctl -u caddy -u drydock`. Check the cert/key pair ([1.3](#13-a-tls-certificate-every-client-trusts-for-that-hostname)). |
| Installer: `--ca-cert … holds a private key` | The CA's key was given instead of its certificate | Pass the CA's certificate. Never copy the CA key to the server. |
| Installer: `… answered '502'` | Caddy is up, Drydock is not | `journalctl -u drydock -n 50`. Check the socket with `ls -l /run/drydock/http.sock`. |
| Installer: `the upgrade failed and was rolled back` | The new release would not start | The log above the message. Open an issue, and fix forward. |
| Installer: `secrets.key is not a 32-byte key file` | The master key is damaged or truncated | Restore it from your backup ([6](#6-back-up-the-secrets-master-key-now)). Never move it aside unless you accept losing every secret. |
| Browser: certificate warning or `NET::ERR_CERT_AUTHORITY_INVALID` | Private CA not installed or not fully trusted on this device | Install the CA. On iOS, also enable it under *Certificate Trust Settings*. |
| Browser: `ERR_SSL_PROTOCOL_ERROR` / `SSL_ERROR_INTERNAL_ERROR_ALERT` | **Host mismatch**: you used an IP, a short name or another alias. Caddy has no site for that name, so there is no certificate to offer | Use exactly `https://drydock.example.com`. This is the DNS-rebinding defence working ([§13.3](../design/overall/drydock-design.md#133--what-a-browser-can-be-talked-into)). |
| Browser: cannot connect / times out | DNS points elsewhere, or a firewall blocks the port | `nslookup` from that device. Check `sudo ss -ltnp \| grep :443` and the firewall rules. |
| JSON `forbidden_host` | Drydock's own `Host` check, behind Caddy | Should not happen with the installer's config. Compare `systemctl cat drydock` with `/etc/drydock/drydock.env`. |
| Sign-in fails with `forbidden_origin` | The browser is on a page that is not `https://drydock.example.com` (another name for the server, or an old tab) | Open exactly `https://drydock.example.com`. The installer lowercases `--ui-host`, so case is no longer a cause. |
| Journal: `drydock serve: config: UIOrigin … must be lowercase` | The unit was hand-edited, or written by an installer older than `--ca-cert` | Re-run the installer: it lowercases the setting in `/etc/drydock/drydock.env`. |
| Sign-in: *too many failed sign-ins; retry after …* | Lockout: per-IP exponential backoff (up to 15 min), and a global cap of 50 failures in 15 min | Wait it out. The lockout survives a restart on purpose. |
| Forgot the password | — | `sudo -u drydock drydock passwd --db /var/lib/drydock/drydock.db` (this signs out every device) |
| UI: *No GitHub App is set up yet* / API `503 app_not_configured` | The unit has no `--github-app-id` | `systemctl cat drydock \| grep github`. Re-run the installer with `--github-app-id 5189455 --github-app-key <pem>`. |
| UI: *Could not refresh the repository list from GitHub: … 401 …*; journal `drydock: catalog refresh: github: GET /app/installations: 401 …` | **Wrong App ID or key**: the key is from a different App (the dev App?) or was deleted on GitHub. Or the server clock is off | Check `grep APP_ID /etc/drydock/drydock.env` says `5189455`. Generate a fresh key and re-run with `--github-app-key`. `timedatectl`. |
| UI: refreshed, `0 repositories` | The App is installed on nothing | Install it on repositories ([1.4](#14-the-github-apps-private-key)), then **Refresh catalog** |
| A repository you expected is missing | Not in the App's installation | Its row's *Installation settings* link, or GitHub → the App → *Configure* |
| Journal: `drydock: reconcile: … permission denied … docker.sock` | **Docker permission**: `drydock` is not in `docker`, or the service started before it was | `id drydock` shows `docker`. `sudo systemctl restart drydock` (new groups apply only on restart). |
| Journal: `drydock: reconcile: … Cannot connect to the Docker daemon` | The daemon is down | `sudo systemctl enable --now docker`, then `sudo systemctl restart drydock` |
| Workspace fails at *starting the container* immediately, or `devcontainer: not found` in the journal | **devcontainer CLI missing for the `drydock` user** (Node removed or upgraded, or the CLI uninstalled) | `sudo runuser -u drydock -- env PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin HOME=/var/lib/drydock devcontainer --version` must print `0.89.0`. Reinstall ([3.2](#32-nodejs-20-and-the-devcontainer-cli-on-the-services-path)). |
| Workspace fails at *starting the container* after a while | Image pull or build failure, a failing `postCreateCommand`, or no network to `ghcr.io` / `mcr.microsoft.com` | The step's detail, then `journalctl -u drydock \| grep "workspace <id>"`. Try `sudo docker pull mcr.microsoft.com/devcontainers/base:debian`. |
| Workspace fails at *verifying* | The broker round-trip failed inside the container | Check that `/run/drydock/sock/<id>.sock` exists. The journal's `workspace <id>` lines. |
| Workspace fails at *cloning* | GitHub refused the clone token: the App lacks `contents`, or the repository was removed from the installation | Check the App permissions ([1.4](#14-the-github-apps-private-key)) |
| Create refused with `at_capacity` | 10 workspaces already hold a container | Stop or delete one |
| `git push` in a container: `GitHub access unavailable …` | The workspace is not *Running* in Drydock (its socket is closed), or GitHub refused | Start it in the UI. A container started by hand with `docker start` gets no GitHub access, by design. |
| Pushing a workflow file is rejected | The App lacks `workflows: write` | Add the permission on the App, then accept the new permissions on the installation |
| Secret check prints `drydock: secrets unavailable: …`, exit 69 | Broker unreachable, or a stored secret no longer decrypts (a replaced master key) | **Secrets** lists any *undeliverable* secret. Restore the master key, or store those values again. |
| A delete is stuck in *Deleting…* at a sub-step | A container still holds the mount, or the cleanup image cannot be pulled | The sub-step's detail. Press **Delete again**. A restart also resumes it. |
