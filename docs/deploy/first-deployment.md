# First deployment

A step-by-step checklist for putting Drydock on your own dev server for the first time, by
yourself, from a release published on GitHub. Work through it top to bottom and tick each box.
Every command is for a **Debian or Ubuntu** host unless a step says otherwise.

Each step that has a non-obvious reason gets one line of *why*, with a link to the design
section that argues it. If you want the argument, follow the link; to deploy, you do not need to.

**Managing the server with Ansible?** [The Ansible companion](first-deployment-ansible.md) does
steps 3 to 7.1 and the upgrade as copy-pastable tasks, numbered the same way, and links back here
for the rest.

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
- [6. The secrets master key: supply it, or back it up](#6-the-secrets-master-key-supply-it-or-back-it-up)
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
Four issues listed here earlier are fixed: three in v0.2.0, and one in v0.2.1. This runbook
deploys **v0.4.2**. v0.3.0 is the first release whose installer takes `--secrets-key`
([step 6](#6-the-secrets-master-key-supply-it-or-back-it-up)); v0.4.0 is the first that tells a
GitHub App missing a permission apart from a revoked repository ([§11](#11-troubleshooting)) and
signs Claude in ([§8.6](#86-a-claude-session-phase-5)); and v0.4.2 is the first whose installer
refuses a `--secrets-key` before it replaces anything, the binary included, and restarts a
Drydock left running a replaced binary ([6.3](#63-switch-an-installed-key-to-one-you-supply)).
To confirm the installer you are about to run is v0.4.2 or later,
`curl -fsSL https://github.com/krelinga/drydock/releases/latest/download/install.sh | grep '^RELEASE_VERSION='`
prints `RELEASE_VERSION="v0.4.2"` or a later tag. v0.4.0 and v0.4.1 install and run the same way,
except on a refused `--secrets-key` during an upgrade
([6.3](#63-switch-an-installed-key-to-one-you-supply)). An installer older than v0.3.0 stops at the
flag with `unknown option: --secrets-key`; on one, leave the flag off and take
[step 6's generated key](#62-a-generated-key-back-it-up-now).

- A first install with `--github-app-key` on a host without `/etc/drydock` no longer stops with
  `mktemp: failed to create file via template '/etc/drydock/.github-app.XXXXXX'` (fixed in
  v0.2.1). To install v0.2.0 itself (`--version v0.2.0`), run
  `sudo install -d -m 0755 -o root -g root /etc/drydock` first.
- A private CA no longer fails the installer's final check: pass `--ca-cert` ([step 5](#5-install)).
  A certificate the server cannot verify is now reported as exactly that, apart from a real failure.
- A mixed-case `--ui-host` is lowercased by the installer, and Drydock refuses to start on one, rather
  than refusing every sign-in with `forbidden_origin`.
- A release is created as a draft and becomes public and *Latest* only after its assets are uploaded
  and installed by the README one-liner, so a failed release never leaves *Latest* without files
  ([step 2](#2-cut-a-release)). v0.2.0's own first release run failed this way, on a missing
  `socat` in the release job's test environment, and was finished with
  [step 2's resume](#resume-a-draft-release) once the two environments became one.

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
  **Actions: read & write is the easy one to miss**: nothing in `git` needs it, so clones, fetches
  and pushes all work, and only `gh` inside a workspace fails, because the `gh` token asks for
  every permission in this list.
  *Why:* each token Drydock mints asks for a subset of these, and GitHub refuses a token request
  for a permission the App lacks. A missing `workflows` permission shows up later as a confusing
  push rejection.
- [ ] **After changing any of these permissions, accept them on each installation.** GitHub does
  not apply a permission change to an existing installation until its owner accepts it: open
  <https://github.com/settings/installations>, choose the App, and accept the pending request
  (GitHub also emails the owner). Until then the installation keeps the old permissions, and the
  tokens that need the new one are refused exactly as if the App still lacked it. *This runbook cannot see the App's settings, because the App is private.
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

> **This runbook deploys `v0.4.2`. Check whether it is published:**
> `gh release view v0.4.2 --repo krelinga/drydock --json isDraft --jq .isDraft`.
> - It prints `false`: v0.4.2 is published. If it, or a later release, is *Latest*, this step is
>   done: go to [step 3](#3-prerequisites-on-the-server).
> - It prints `true`: v0.4.2 is a draft whose release run did not finish. Recover it with
>   [the table below](#if-the-release-workflow-fails).
> - It fails with `release not found`: v0.4.2 has not been cut yet. It is the open release PR,
>   **`chore(main): release 0.4.2`**; cut it with the steps below.
>
> The rest of this step is also how any later release is cut.

The install one-liner always installs the **latest** GitHub release. A new release comes from the
open release-please PR, titled **`chore(main): release X.Y.Z`**; merging it publishes `vX.Y.Z`. If
more `feat:` or `fix:` commits land first, the PR's number stays the same and its title and
changelog grow.

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
  `verify` downloads them back and runs the README one-liner against them, on a fresh host and as
  an upgrade of the release that is *Latest* now (with a password and a stored secret), and
  `publish` makes the
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
| The workflow or its test environment (`.github/workflows/release-please.yml`, `.github/actions/`, `test/install/`) | Merge the fix to `main` as `ci:` (or `test:`), then [resume the draft](#resume-a-draft-release). The code at the tag was fine, so it stays the release. |
| The code | Fix forward: merge a `fix:` PR, and release-please opens a release PR for the next patch version; merge that as above. The failed draft can stay, or be removed with `gh release delete vX.Y.Z --repo krelinga/drydock --yes`, which keeps the tag. Keep the tag: release-please finds its previous release by it. |

Do not delete the tag to make release-please try again. Its release PR is already labelled
`autorelease: tagged`, so it would not recreate the release, and without the tag it would plan the
next release from the tag before it again.

#### Resume a draft release

`workflow_dispatch` runs `main`'s release workflow against an existing draft. It skips
release-please and runs `test`, `assets`, `verify` and `publish` on the **code at the tag**, with
**`main`'s workflow and test environment**: `test` runs `main`'s `.github/actions/go-suite`, and
`verify` runs `main`'s `test/install/live.sh` against the downloaded assets. It refuses a tag
whose release is not a draft, so it cannot replace a public release's files.

- [ ] **Check the draft and its tag exist**, and that the fix is on `main`:
  ```sh
  gh release view vX.Y.Z --repo krelinga/drydock --json isDraft,tagName,assets --jq '{isDraft, tagName, assets: [.assets[].name]}'
  gh api repos/krelinga/drydock/git/ref/tags/vX.Y.Z --jq .object.sha
  ```
  `isDraft` must be `true`. (v0.2.0 was finished this way, with `vX.Y.Z` as `v0.2.0`.) Any assets left from an earlier run are replaced (`--clobber`).
- [ ] **Run it and watch it**:
  ```sh
  gh workflow run release-please.yml --repo krelinga/drydock --ref main -f tag=vX.Y.Z
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
  second command must print `RELEASE_VERSION="vX.Y.Z"`, the tag you resumed. If it prints the
  previous tag, or `curl` fails, the upload has not finished.

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

> **If `apt-get update` fails with `402 Payment Required` from `dl.cloudsmith.io`**, Caddy's apt
> repository is unavailable (it was, in October 2026). Remove the repository and install the same
> package from Caddy's GitHub release instead (amd64; it provides the same `caddy` user and
> `caddy.service`, but gets no updates from apt, so bump the version by hand):
>
> ```sh
> sudo rm -f /etc/apt/sources.list.d/caddy-stable.list /usr/share/keyrings/caddy-stable-archive-keyring.gpg
> sudo apt-get update
> CADDY_VERSION=2.11.7
> base=https://github.com/caddyserver/caddy/releases/download/v$CADDY_VERSION
> deb=caddy_${CADDY_VERSION}_linux_amd64.deb
> cd "$(mktemp -d)"
> curl -fsSLO "$base/$deb" -fsSLO "$base/caddy_${CADDY_VERSION}_checksums.txt"
> grep " $deb\$" "caddy_${CADDY_VERSION}_checksums.txt" | sha512sum -c -
> sudo apt-get install -y "./$deb"
> ```

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

### 4.3 Optional: your own secrets master key

Every repository secret is encrypted under one master key. Decide now where its durable copy
lives ([step 6](#6-the-secrets-master-key-supply-it-or-back-it-up)): **your own key**, which you
make and keep in your own secret store and give to the installer with `--secrets-key`; or **a
generated key**, which the installer makes and you then back up. For a generated key, skip this
step.

- [ ] **Make the key where your secret store is** (your laptop, say), and store it there first:
  ```sh
  head -c 32 /dev/urandom > drydock-secrets.key    # exactly 32 raw bytes: not base64, no newline
  chmod 0400 drydock-secrets.key
  sha256sum drydock-secrets.key                     # the hash is safe to show; the key is not
  ```
  Put the file in your password manager as an attachment, or in an encrypted vault, before going
  on. A copy of a key that already exists works the same way (for example the one an earlier
  install generated, fetched as in [6.2](#62-a-generated-key-back-it-up-now)).
- [ ] **Copy it to the server** the way [4.1](#41-the-github-app-private-key) copies the App key:
  ```sh
  install -d -m 0700 ~/drydock-drop                                       # server
  scp drydock-secrets.key <you>@<server>:drydock-drop/secrets.key         # laptop
  sudo install -m 0400 -o root -g root ~/drydock-drop/secrets.key /root/drydock-secrets.key   # server
  shred -fu ~/drydock-drop/secrets.key && rmdir ~/drydock-drop            # server
  sudo sha256sum /root/drydock-secrets.key                                # server: the same hash
  ```
  [Step 5](#5-install) installs it at `/etc/drydock/secrets.key` (mode `0400`, owner `drydock`), and
  you delete `/root/drydock-secrets.key` right after. Then delete the laptop's loose copy
  (`shred -fu drydock-secrets.key`) once your secret store has it.

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

  **Your own secrets master key ([4.3](#43-optional-your-own-secrets-master-key)):** add
  `--secrets-key /root/drydock-secrets.key`. Without it, the installer generates the key, and
  [step 6](#6-the-secrets-master-key-supply-it-or-back-it-up) has you back it up. Like
  `--github-app-key`, the flag is a path, is not kept in `drydock.env`, and later runs need it only
  to change the key.

  Leave out the `--preview-*` flags. Previews are optional, only their sign-in is built, and
  [8.7](#87-optional-enable-previews-no-ports-panel-yet) adds them with a re-run whenever you like.

**What it does, in order.** It stops at the first failure. Steps 1 to 9 only check, stage and
decide: a refusal there leaves the installed binary, the running Drydock and every file as they
were, and removes whatever it staged. If a failure comes after the run stopped Drydock (to replace
the master key), it starts Drydock again before it exits and says so:
`warning: starting drydock again, which this run stopped`.

1. It downloads `drydock_linux_amd64.tar.gz` and `SHA256SUMS` for the tag stamped into this
   `install.sh`, checks the checksum, and runs the `install.sh` inside the tarball. Output starts
   with `==> downloading drydock_linux_amd64.tar.gz (v0.4.2)`.
2. It checks prerequisites: root, Linux, systemd, `caddy` and the `caddy` user, `curl`, `docker`,
   the `docker` group, and `devcontainer` on the service `PATH`.
3. It validates flags: the hostname must contain a dot, and is lowercased (it says so if that
   changed it); certificate paths must be absolute files; `--ca-cert` must be a certificate, not a key;
   the App ID must be numeric (the Client ID starts with `Iv` and is refused); the key must look
   like a PEM private key; `--secrets-key` must be a regular file of exactly 32 bytes. The refusals
   name the file and its size, never its contents.
4. It checks that the `caddy` user can read the certificate and key.
5. It decides about the Caddyfile: the Caddy package's stock file may be replaced (it is backed up
   in step 12), and anyone else's only with `--take-over-caddy`.
6. `==> creating group drydock` and `==> creating user drydock`: a system account with home
   `/var/lib/drydock` and shell `nologin`.
7. `==> adding drydock to the docker group (root-equivalent; see design §13.4)`. Then it runs
   `devcontainer --version` as `drydock` (and fails if that does not run), and `docker info` as
   `drydock` (only a *warning* if the daemon is unreachable).
8. It creates `/srv/drydock` (root, `0755`, only if it is absent), `/srv/drydock/ws`
   (`drydock`, `0700`) and `/etc/drydock` (root, `0755`).
9. It stages and decides, replacing nothing yet. It copies the release's binary to
   `/usr/local/bin/drydock.new`, and validates the new Caddy config with `caddy validate`. If the
   running Drydock is not the installed binary, because an earlier run was interrupted after it
   replaced the file, it says so and restarts it in step 13:
   `==> drydock is running vA, not the installed /usr/local/bin/drydock (vB): an earlier run did not finish; it will be restarted`.
   With a `--secrets-key` that differs from an installed key, it asks whether any secret is stored,
   and refuses here if one is ([6.3](#63-switch-an-installed-key-to-one-you-supply)).
10. It installs `/usr/local/bin/drydock` (keeping the one it replaces as `drydock.previous`), then
    `==> installed the GitHub App key at /etc/drydock/github-app.pem (mode 0400, owner drydock)`.
11. Without `--secrets-key`: `==> created the secrets master key at /etc/drydock/secrets.key (mode 0400, owner drydock); back it up — without it no stored secret can be read`.
    With it: `==> installed the secrets master key at /etc/drydock/secrets.key from /root/drydock-secrets.key (mode 0400, owner drydock); keep your copy of it safe — without it no stored secret can be read`.
12. It writes `/etc/drydock/drydock.env`, `/etc/systemd/system/drydock.service`,
    `/etc/systemd/system/caddy.service.d/drydock.conf`, `/etc/caddy/Caddyfile` and the directory
    `/etc/caddy/drydock.d/`. The stock Caddyfile is backed up first:
    `==> replacing the Caddy package's default Caddyfile (saved to /etc/caddy/Caddyfile.before-drydock.<timestamp>)`.
13. `==> starting drydock`, then `==> restarting caddy`. It fails unless the running Drydock is
    the installed binary.
14. **It asks for the operator password**: `New password:` and `Again:`, without echo. Then
    `Password set. Every existing session has been signed out.`
15. It runs the end-to-end check: `https://drydock.example.com/api/auth/session` through Caddy,
    resolved to `127.0.0.1`, must answer `401` over a certificate it verified, against
    `--ca-cert` if you gave one and the system's trust store otherwise. It prints nothing when the
    check passes. If everything is running and only the certificate could not be verified, it says
    exactly that, `Drydock is installed and running, and answers through Caddy, but this host could
    not verify the certificate …`, with `curl`'s reason; see [§11](#11-troubleshooting).
16. `==> installed Drydock v0.4.2` and `==> open https://drydock.example.com`.

- [ ] **Delete the temporary App key**, and the temporary master key if you gave one:
  ```sh
  sudo shred -u /root/drydock-app.pem
  sudo shred -u /root/drydock-secrets.key   # your own key only
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
| `/etc/drydock/secrets.key` | drydock `0400` | The secrets master key: 32 raw bytes, the file given to `--secrets-key` or generated. **[Keep a copy](#6-the-secrets-master-key-supply-it-or-back-it-up).** |
| `/etc/systemd/system/drydock.service` | root `0644` | `drydock serve …` as `drydock`, `ProtectSystem=strict`, the fixed `PATH`. `.previous` is kept for rollback. |
| `/etc/systemd/system/caddy.service.d/drydock.conf` | root `0644` | Caddy's `EnvironmentFile`, `SupplementaryGroups=drydock`, and `/run/caddy`. |
| `/etc/caddy/Caddyfile` | root `0644` | Drydock's. The original is kept at `/etc/caddy/Caddyfile.before-drydock.<timestamp>`. |
| `/etc/caddy/drydock.d/` | root `0755` | Optional sites. Empty unless previews are configured. |
| `/var/lib/drydock/drydock.db` | drydock, dir `0700` | SQLite (WAL). Created by systemd's `StateDirectory`. |
| `/run/drydock/{http,preview}.sock` | drydock:drydock `0660` | The two Unix sockets. The directory is `0750`. |
| `/run/drydock/sock/` | drydock `0700` | One directory per workspace, `<id>/` (`0755`), holding its token-broker socket `broker.sock` while the workspace runs. The directory, not the socket, is mounted into the container, so it is kept across restarts (`RuntimeDirectoryPreserve=yes`) and removed by a delete. |
| `/run/caddy/admin.sock` | caddy `0600` | Caddy's admin API, moved off `localhost:2019`. |
| `/srv/drydock/ws/` | drydock `0700` | Clones, at `/srv/drydock/ws/<id>/repo`. |
| `drydock.service`, `caddy.service` | enabled | Both start at boot. |
| Docker volume `drydock-claude-config` | (Docker's) | Created by Drydock, not the installer, at the first workspace or Claude sign-in. Once someone signs in ([§8.6](#86-a-claude-session-phase-5)), it holds a live Claude login. |

---

## 6. The secrets master key: supply it, or back it up

Settle this before you store a single secret.

*Why:* every repository secret is encrypted with XChaCha20-Poly1305 under this key
([§10.2](../design/overall/drydock-design.md#102--storage)). **If you lose it, every stored secret
is unreadable**, and the only recovery is to type each value in again. Re-runs and upgrades keep
it. If the file is damaged, the installer refuses to run rather than quietly generating a new one.

Keep your copy of the key **apart from any backup of the database**. A backup that holds both holds
every secret in plaintext-equivalent form.

| | Your own key | A generated key |
|---|---|---|
| In step 5 | `--secrets-key /root/drydock-secrets.key` ([4.3](#43-optional-your-own-secrets-master-key)) | nothing |
| The durable copy | already in your secret store | a backup you take now ([6.2](#62-a-generated-key-back-it-up-now)) |
| Rebuilding the server | pass the same file as `--secrets-key` | restore the backup ([6.2](#62-a-generated-key-back-it-up-now)), or pass it as `--secrets-key` |

### 6.1 Your own key

- [ ] **Check the server holds the key you stored**: `sudo sha256sum /etc/drydock/secrets.key`
  prints the hash you noted in [4.3](#43-optional-your-own-secrets-master-key).

There is no backup step: your secret store is the backup. Later runs keep the installed key without
the flag. Passing the same file again changes nothing and restarts nothing.

### 6.2 A generated key: back it up, now

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
  The stored copy is now your own key in every sense, so from here on you may pass it as
  `--secrets-key` like any supplied key: it is the installed key, so nothing changes.
- [ ] **To restore it later**, for example on a rebuilt server, copy the backup to
  `/root/drydock-secrets.key` as in [4.3](#43-optional-your-own-secrets-master-key) and pass
  `--secrets-key /root/drydock-secrets.key` to that server's first install. On an installed host
  whose key file was damaged, put it back by hand:
  ```sh
  sudo install -m 0400 -o drydock -g drydock ./secrets.key /etc/drydock/secrets.key
  sudo systemctl restart drydock
  ```

### 6.3 Switch an installed key to one you supply

For an installed host whose key you now want to keep in your own secret store, such as a first
install that generated its key. There are two ways:

- **Keep the installed key.** Back it up as in [6.2](#62-a-generated-key-back-it-up-now),
  and store that copy. Nothing is re-run and nothing changes. This works whether or not secrets are
  stored.
- **Install a different key.** Make one as in [4.3](#43-optional-your-own-secrets-master-key), then:
  ```sh
  sudo runuser -u drydock -- drydock count-secrets --db /var/lib/drydock/drydock.db   # must print 0
  curl -fsSL https://github.com/krelinga/drydock/releases/latest/download/install.sh \
    | sudo bash -s -- --secrets-key /root/drydock-secrets.key
  sudo shred -u /root/drydock-secrets.key
  ```
  With no secret stored, the run prints `==> stopping drydock to replace the secrets master key`,
  then `==> replaced the secrets master key at /etc/drydock/secrets.key with
  /root/drydock-secrets.key (no secret was stored under the old one); …`, and starts Drydock on
  the new key. It asks the database once while Drydock runs and again once Drydock is stopped, so
  no secret can be stored under the old key in between.

**With a secret stored, a different key is refused**, and nothing changes: the installed key stays
byte for byte, and Drydock is not stopped or restarted. The run ends with:

```
error: --secrets-key /root/drydock-secrets.key is not the key installed at /etc/drydock/secrets.key, and 1 stored secret(s) are sealed under the installed key. Replacing it would make every one of them unreadable, and Drydock delivers no secret while any cannot be read. The installed key was left as it is. To keep it, copy /etc/drydock/secrets.key into your secret store and pass that copy as --secrets-key, or leave the flag off. To move to the new key, delete the stored secrets on Drydock's Secrets screen, re-run with --secrets-key, and enter them again.
```

*Why:* every stored secret is sealed under the installed key, and Drydock refuses to deliver any
secret while one cannot be decrypted, so a swap would break every workspace's secrets at once. The
installer never deletes or re-encrypts a secret, and has no flag to force the swap. To move to a new
key anyway, have every value at hand first (Drydock never shows one), delete the secrets on
**Secrets**, re-run with the flag, then store them and their grants again.

If the same run was also an upgrade, none of the upgrade happened either: the refusal comes before
the new binary is installed, so `drydock version` still prints the old release, which keeps
running from its own file. Re-run without `--secrets-key` to upgrade; it ends with
`==> upgraded Drydock vA -> vB`.

Installers before v0.4.2 installed the new binary *before* this refusal, so the old process kept
running a deleted file, and a re-run with one of them said `is installed and current` and restarted
nothing. If that happened on your host, `sudo readlink /proc/$(systemctl show -p MainPID --value drydock)/exe`
ends in ` (deleted)`. Any run of a v0.4.2 or later installer repairs it: it prints
`==> drydock is running vA, not the installed /usr/local/bin/drydock (vB): an earlier run did not finish; it will be restarted`,
restarts Drydock on the installed binary, and keeps the one that was running as the rollback.
That restart is the upgrade, so back up the database first, as for any upgrade
([10.1](#101-upgrade)).

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
  Apart from systemd's own `Started …` and `Stopping …` lines, a healthy start writes one line:
  ```
  drydock: serving on /run/drydock/http.sock and /run/drydock/preview.sock
  ```
  One other line is benign: `drydock: login: removed N leftover login container(s)`, when a
  Claude sign-in was cut off by the previous stop. Everything else Drydock does at boot logs
  **only when it fails**. So silence is success, and **any other line from Drydock is a
  problem**. The ones boot can write:
  - `drydock: reconcile: …`: Drydock cannot list containers. This is usually Docker access
    ([§11](#11-troubleshooting)).
  - `drydock: sweeping cleanup helpers: …`: the helper containers an interrupted delete left
    could not be listed or removed. Usually Docker access, as above.
  - `drydock: broker: …`: the broker sockets of running workspaces were not reopened, so those
    workspaces have no GitHub access until they are stopped and started.
  - `drydock: session servers: …`: running workspaces' Claude session servers were not resumed.
  - `drydock: secrets: …`: the stored secrets could not be checked at boot.
  - `drydock: identity: Could not check the Claude login: … (…)`: the shared Claude login could
    not be read. The sentence says why; the first check builds an image and needs the network.
  - `drydock: login: sweeping leftover login containers: …`: as the reconcile line.
  - `drydock: catalog refresh: github: GET /app/installations: 401 …`: the App ID and key do not
    belong together, or the clock is off.
  - `drydock: workspace <id>: …`: one workspace's step or session server failed; its page in the
    UI says which.
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
  `sudo stat -c '%U %a %s' /etc/drydock/secrets.key` prints `drydock 400 32`. With your own key
  (`--secrets-key`), `sudo sha256sum /etc/drydock/secrets.key` matches your stored copy.

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
  (a CA profile installed but not enabled for full trust). A sign-in that the page refuses with
  *This request did not come from Drydock's own page* is neither: it is a release before v0.3.0,
  whose UI could not sign in from Safari or any other iPhone browser
  ([11](#11-troubleshooting)).

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
  config (B gets Drydock's minimal config, and the step says so), preparing credentials (it
  creates the shared Claude volume, `drydock-claude-config`, the first time), opening the broker
  socket, starting the container, verifying, and starting the session server (*"Handed to the
  session supervisor…"*). Each step reaches *done*. **Recent events** shows the same.
- [ ] The card's line is now the **session server's**, not the container's. Until someone has
  signed in to Claude it reads *Claude is not signed in* (or, once the expiry watch has looked,
  *Waiting on Claude sign-in*) with no button: that is expected, and [8.6](#86-a-claude-session-phase-5)
  fixes it for every workspace at once.
- [ ] Do the same for **A**. If A commits a `devcontainer-lock.json` that is stale, the
  *starting the container* step says that `devcontainer up` rewrote it in the clone. That is
  expected, and it is what VS Code would do too
  ([§6 *The repository's lockfile*](../design/overall/drydock-design.md#the-repositorys-lockfile)).
- [ ] **If the row says *Needs approval* instead of building**, the repository's `devcontainer.json`
  asks for something that reaches outside its container — `runArgs`, `privileged` (docker-in-docker
  sets it), a bind mount, `initializeCommand`, Docker Compose and the like
  ([§6 *What a configuration may ask of the host*](../design/overall/drydock-design.md#what-a-configuration-may-ask-of-the-host)).
  Nothing has been built and no container exists yet. Read the list on the card: each setting, its
  value, and whether the repository or a Feature set it. **Approve and continue** records the
  approval for that repository and carries on with the build; **Cancel** leaves it stopped. Approve
  only what you would let that repository do to this server — `privileged` is root on the host, and
  anything running in the container can then use it. The approval is kept per repository: the same settings, or fewer of them, never ask again;
  anything new, or a changed value, does, showing what changed.
- [ ] **On the server**, the container is there, found by label:
  ```sh
  sudo docker ps --filter label=drydock.workspace --format '{{.ID}}  {{.Label "drydock.repo"}}  {{.Status}}'
  sudo ls -l /run/drydock/sock/*/    # one <id>/broker.sock per running workspace
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
sudo docker exec -u vscode -w /workspaces/repo "$CID" gh repo view --json nameWithOwner --jq .nameWithOwner   # prints <owner>/<repo>: this workspace's repository, and nothing else
sudo docker exec -u vscode -w /workspaces/repo "$CID" gh repo view <owner>/<A>   # another PRIVATE repo, even one the App is installed on: not found
```

Use a *private* repository for that last check. An installation token can still read public
repositories, as any anonymous caller can.

If `git fetch` works and `gh repo view` fails with
`drydock: GitHub access unavailable (the GitHub App lacks a permission; see the workspace's events in Drydock)`
— or with `(revoked)` on v0.3.0 and earlier — the App is missing one of the `gh` token's extra
permissions, or its installation has not accepted a change. See [§11](#11-troubleshooting).

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

- [ ] **Stop** (detail page → *Actions*; once a session server is serving it asks first while a session is live). It moves to *Stopped*. On the server, the
  container is stopped but still exists (`sudo docker ps -a --filter label=drydock.workspace=$WS`),
  and `/run/drydock/sock/$WS/broker.sock` is **gone** (its directory stays, for the next start). *Why:* GitHub access follows Drydock's state, not
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

The Phase 2 check that proves boot reconciliation on real hardware
([§14](../design/overall/drydock-design.md#14-build-plan), testing §11.5). It was run on the real
server on 8 October 2026 and passed. The one snag was the host's own: a systemd unit ordered
before a network mount it needed, in that deployment's Ansible, not Drydock. If your workspace
root or Docker's data lives on a network mount, order both services after it.

- [ ] With one workspace *Running*, run `sudo reboot`.
- [ ] After boot, both services are active and the UI loads. The workspace shows *Stopped*: Docker
  does not restart dev containers, and Drydock never auto-starts one
  ([§6 Reconciliation on boot](../design/overall/drydock-design.md#reconciliation-on-boot)).
  **Start** brings it back with its clone intact.
- [ ] `journalctl -u drydock -b -o cat` has no `reconcile:` line.

### 8.6 A Claude session (Phase 5)

Sign in once, from Settings. Every workspace shares the login, because every container mounts the
same `drydock-claude-config` volume at its `CLAUDE_CONFIG_DIR`
([§7.1](../design/overall/drydock-design.md#71--one-shared-credential-volume)). The sign-in is also
the one check no test can make: the real `Login successful` needs you in a browser with a real
account.

- [ ] **Settings → Claude.** Before anyone signs in it says *No one has signed in yet.*, and so
  does the banner on every page. A running card says *Waiting on Claude sign-in.*, with no button.
- [ ] **Sign in to Claude.** It says *Starting a login container…* The first time, Drydock builds
  the image Claude Code runs in (`drydock-claude:<version>-<hash>`) with npm, which needs the
  network and can take a few minutes.
- [ ] **Open the Claude sign-in page**, or **Copy link** and open it in another browser. Sign in,
  and Claude shows a code. You have five minutes from when the link appears; the page counts down.
  You can leave the page meanwhile: it picks the login up again when you come back.
- [ ] Paste the code into *Code from Claude* and **Submit code**. It says *Signed in.*, and the
  section names the account. A wrong code says so and lets you paste again; half a code is refused
  in the field before anything is sent.
- [ ] **Nothing warns about the login.** No banner, no dot on any card, and Settings says
  *Signed in as …* with **Login expires** about a month out — the login's own end, as Claude Code
  records it. The access token behind it lives about eight hours and renews by itself, so it is
  not shown as an expiry at all (until v0.4.5 it was, and every sign-in was greeted with *The Claude
  login expires in 8 hours*).
- [ ] On the server, the volume is Drydock's and the login container is gone:
  ```sh
  sudo docker volume inspect -f '{{json .Labels}}' drydock-claude-config   # {"drydock.claude-config":"true"}
  sudo docker ps -a --filter label=drydock.login                           # empty
  ```
  The login runs as the `drydock` user, which is the user every workspace container runs as, so the
  credential it writes is theirs to read. Drydock gives the volume to that user when it first makes
  it, and leaves a `.drydock-volume` directory in it so no image can change that later. If the
  volume holds files that belong to another user, the sign-in stops before anything is written and
  says which two users (see [11](#11-troubleshooting)).
- [ ] **If the code was right and it does not say *Signed in.*:** the success match is the one
  part of the handshake measured from Claude Code's binary rather than from a real login (Spike 01).
  Note what the page said, run `journalctl -u drydock -b -o cat | grep login`, and report it. The
  journal never holds the code.
- [ ] Each workspace that was waiting on the login starts its session server by itself: a
  successful sign-in triggers the login check at once, and the supervisors resume on its result.
  The card goes *Starting session…* → **Capacity 1 / 4**, with **Open in Claude**. The link is
  `https://claude.ai/code?environment=env_…`; the session also appears in the Claude app on your
  phone, named after the repository. *Why 1 / 4:* the server pre-creates one session in the clone,
  and it counts ([§8](../design/overall/drydock-design.md#8-session-supervision)).
- [ ] **Open the log** on the detail page: the server's own lines, redacted, held in Drydock's memory
  only. The journal stays quiet.
- [ ] **Stop** (detail page → *Actions*) now asks first, because a session is live. Stop it, then
  **Start**: the workspace comes back, and its card returns to *Capacity 1 / 4* on the **same**
  environment link. *Why:* a stop is `SIGTERM`, which keeps the environment for the next start
  ([Spike 02](../design/spikes/02-rc-restart.md)).
- [ ] Restart Drydock itself: `sudo systemctl restart drydock`. Sessions keep running while it is
  down; after it is back the card reads *Capacity 1 / 4* again, on the same link.

If a card says *Waiting for the previous session server to release the folder*, leave it: that is a
wait of one to three minutes, not a failure, and it clears on its own.

### 8.7 Optional: enable previews (no ports panel yet)

Skip this unless you want to prepare for previews now. **The front door, the sign-in handshake and
the proxy are built**
([port forwarding §13](../design/port-forwarding/port-forwarding-design.md#13-build-plan), steps 1
to 3): a signed-in device opening the preview URL of an *enabled* port on a running workspace is
signed in to that preview and proxied to the dev server listening on that port inside the
workspace's container — page, assets, server-sent events and the dev server's live-reload
websocket alike, with nothing published on the server. But there is no ports panel to enable a port
from yet (step 4), so in practice every preview name still ends on the *This preview is not
available* page. What it buys today is the certificate, the DNS, Caddy's preview site, the
handshake and the proxy proven in place, so the later steps need no infrastructure change. Below,
`<preview-domain>` is your preview domain.

Once a port can be enabled, three things are worth knowing now. A dev server must listen on
`0.0.0.0`, not `127.0.0.1`: Drydock reaches the container over Docker's network, never from
inside it, so a loopback-only server answers *Preview not answering — Nothing is answering on port
N* (a `502`). The dev server sees `Host: localhost:<port>`, which Vite, Next.js and Django accept
without configuration, and the preview address in `X-Forwarded-Host`. And `drydock serve` bounds
previews by itself: at most 512 preview requests at once, an open live-reload websocket counting
until it closes (`--preview-max-connections`), and a websocket with no traffic for 30 minutes is
closed (`--preview-idle-timeout`). The installer passes neither flag, so those are the defaults.

You provide three things. Drydock issues no certificate and touches no DNS
([port forwarding §9](../design/port-forwarding/port-forwarding-design.md#9-caddy-configuration)):

- [ ] **A second registrable domain.** Not a subdomain, parent or sibling of the UI host: if the UI
  is `drydock.example.com`, `preview.example.com` and `example.com` are both refused. A preview is
  repository code, and only a different registrable domain makes it cross-site with the UI, so the
  browser keeps it from the session cookie
  ([port forwarding §4](../design/port-forwarding/port-forwarding-design.md#4-naming-and-routing)).
  Check it before you buy or wire anything, with the installed binary:
  ```sh
  drydock check-preview-domain --ui-host drydock.example.com --preview-domain <preview-domain> && echo separate-ok
  ```
  It prints `separate-ok`, or the reason the installer would give for refusing.
- [ ] **A LAN wildcard DNS record**, `*.<preview-domain>`, pointing at the server's LAN address — the
  same address the UI host resolves to ([1.2](#12-a-hostname-for-the-ui-and-how-clients-resolve-it)).
  Every preview gets its own name under it, made up at runtime, so a list of names will not do.
  Check from a laptop: `getent hosts anything.<preview-domain>` prints the server's address.
- [ ] **A wildcard certificate and key** for `*.<preview-domain>`, full chain, leaf first. In
  practice that is ACME DNS-01 (option B in [1.3](#13-a-tls-certificate-every-client-trusts-for-that-hostname)),
  or your private CA (option A) — then `--ca-cert` must vouch for both, because the installer's
  check verifies both sites against that one file. If the two certificates come from different
  CAs, make it a bundle: concatenate both CAs' certificates (PEM, certificates only) into one file
  and pass that. With `--ca-cert` set, the check trusts only that file, so a publicly trusted
  certificate's root has to be in the bundle too. Run 1.3's two checks on it; the SAN must show
  `DNS:*.<preview-domain>`.

Put the certificate and key where Caddy can read them, exactly as in
[4.2](#42-the-tls-certificate-and-key), under their own names:

```sh
sudo install -m 0644 -o root -g caddy ~/drydock-drop/preview-fullchain.pem /etc/caddy/certs/preview.crt
sudo install -m 0640 -o root -g caddy ~/drydock-drop/preview-privkey.pem   /etc/caddy/certs/preview.key
shred -fu ~/drydock-drop/preview-privkey.pem; rm -f ~/drydock-drop/preview-fullchain.pem
sudo runuser -u caddy -- test -r /etc/caddy/certs/preview.key && echo key-ok
```

If your certificates live on a mounted share instead, the preview pair can sit beside the UI's:
give the installer those paths, and make sure the ordering that already starts Caddy after that
mount covers them too. Keep that ordering in a drop-in of your own, not in
`/etc/systemd/system/caddy.service.d/drydock.conf`, which the installer rewrites. As with the UI
certificate, a renewal ends with `sudo systemctl reload caddy`.

- [ ] **Re-run the installer with the three preview flags**, all three together. The installer keeps
  the rest of your settings from `/etc/drydock/drydock.env`, so nothing else needs repeating:
  ```sh
  curl -fsSL https://github.com/krelinga/drydock/releases/latest/download/install.sh \
    | sudo bash -s -- \
        --preview-domain <preview-domain> \
        --preview-cert /etc/caddy/certs/preview.crt \
        --preview-key /etc/caddy/certs/preview.key
  ```
  It refuses a domain that is not a separate registrable domain before it changes anything,
  installs `/etc/caddy/drydock.d/preview.caddy`, restarts Drydock with `--preview-domain` and
  reloads Caddy. Its final check now fetches `https://drydock-check.<preview-domain>/` through
  Caddy as well, and wants a `302` to `https://drydock-check.<preview-domain>/.drydock/denied`
  over a certificate it verified, as it verifies the UI host. `drydock-check` is never a preview's
  name, so that is always the denied page.
- [ ] **From a phone or laptop on the LAN**, open `https://anything.<preview-domain>/`. The
  certificate is trusted, and the browser goes to Drydock's own address
  (`https://<ui-host>/preview/authorize?…`) and straight back to
  `https://anything.<preview-domain>/.drydock/denied`: a short page, *This preview is not
  available*, which names nothing. If the device was not signed in, it shows Drydock's sign-in
  page first, and after signing in carries on to the same place. That is the handshake working: it
  checked the device is signed in and that `anything` names no enabled port. A sign-in form on a
  preview address is never Drydock's: the real one is only ever on `<ui-host>`.

To turn previews off again: re-run the installer with `--no-preview`. It removes the preview site
and forgets the three settings; afterwards a preview name gets no TLS answer at all.

---

## 9. What does not work yet

None of the following is a deployment fault. These are the phases still being built
([§14](../design/overall/drydock-design.md#14-build-plan)):

- **Previews have no ports panel yet (port forwarding steps 1 to 3 are built).** With
  [8.7](#87-optional-enable-previews-no-ports-panel-yet), a preview URL runs the sign-in
  handshake and would be proxied to the workspace's dev server, but since no port can be enabled
  yet it ends on *This preview is not available*; without it, a preview name gets no answer. The
  ports panel, and with it the first real phone on a preview, is step 4.
- **Phase 6 is partly done.** Stop, rebuild and delete work, and so do the live session count
  (the card's capacity fraction) and the log viewer. These do not exist yet: memory and
  disk per workspace on the card, and the rest of the failure-mode
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
  (`--if-unset`), so no one is signed out. It keeps the App key and the master key, so neither
  `--github-app-key` nor `--secrets-key` is needed on an upgrade.
- [ ] **To change a setting**, re-run with only that flag, for example
  `… | sudo bash -s -- --cert /new/path.crt --key /new/path.key`. To replace the App key, pass
  `--github-app-id 5189455 --github-app-key <new.pem>`, then revoke the old key on GitHub. Moving
  from a private CA to a public certificate: `--no-ca-cert` with the new `--cert` and `--key`.
  To change the master key: [6.3](#63-switch-an-installed-key-to-one-you-supply), which is refused
  once a secret is stored.
- [ ] **A specific release:** `… | sudo bash -s -- --version vX.Y.Z`.
- [ ] **Upgrading from v0.4.1 or earlier: Rebuild each workspace once.** Earlier releases mounted
  each workspace's broker socket into its container as a single file, and a bind mount of a file
  pins the socket that existed when the container started. The upgrade's restart replaces that
  socket, so every container created before it has had no broker since — and keeps that mount
  until it is replaced. Now the workspace's directory, `/run/drydock/sock/<id>/`, is mounted
  instead, and a restart no longer strands anything. The symptoms in an old container: `git fetch`
  or `git push` and `gh` fail with `drydock: GitHub access unavailable: the broker did not answer`,
  and every Bash command a Claude session runs exits `69` with
  `drydock: secrets unavailable: the broker did not answer`. Drydock says so itself: a running
  workspace's card shows **Container misconfigured** with **Rebuild** (its session server is not
  restarted at boot), and a **Start** of a stopped one fails at *starting the container* with
  *"This workspace's container was created by an earlier Drydock … Rebuild it once; the clone is
  kept."* To find them all on the server:
  ```sh
  for c in $(sudo docker ps -aq --filter label=drydock.workspace); do
    sudo docker inspect -f '{{index .Config.Labels "drydock.repo"}} {{range .Mounts}}{{if eq .Destination "/run/drydock/broker.sock"}}needs a rebuild{{end}}{{end}}' "$c"
  done
  ```
  **Rebuild** keeps the clone and replaces the container, so anything installed in the container
  outside the clone goes with it, as on any rebuild.

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
  useful: its installer predates the App and secrets flags. Rolling back past the release that
  mounts each workspace's broker directory (v0.4.1 or earlier) strands the other way: those
  binaries put the socket at `/run/drydock/sock/<id>.sock`, which a container created or rebuilt
  since cannot see. Rebuild each workspace after such a rollback, as after the upgrade
  ([10.1](#101-upgrade)).

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

> [!WARNING]
> **The `drydock-claude-config` Docker volume holds a live Claude login**: `.credentials.json`,
> with a refresh token, and `.claude.json`, for the account that signed in at
> [§8.6](#86-a-claude-session-phase-5). Removing Drydock does not remove it; step 2 below does.
> Sign that account out of its other sessions at claude.ai as well if the host is going to
> someone else.

```sh
# 1. Delete every workspace in the UI first. That removes containers, clones and sockets properly.
#    Then stop Drydock, so it starts no container while you remove the rest, and remove anything
#    left over, by label: workspaces, cleanup helpers, login containers and log probes.
sudo systemctl disable --now drydock
for l in drydock.workspace drydock.cleanup drydock.login drydock.log-probe; do
  sudo docker ps -aq --filter "label=$l" | xargs -r sudo docker rm -f
done
# 2. The shared Claude login (see the warning above), and the images Drydock built to read it.
sudo docker volume rm drydock-claude-config
sudo docker image ls --format '{{.Repository}}:{{.Tag}}' drydock-claude | xargs -r sudo docker image rm
# 3. Drydock itself
sudo rm -f /etc/systemd/system/drydock.service /etc/systemd/system/drydock.service.previous
sudo rm -f /usr/local/bin/drydock /usr/local/bin/drydock.previous
# 4. Give Caddy back: drop-in, Caddyfile, sites directory
sudo rm -f /etc/systemd/system/caddy.service.d/drydock.conf
sudo cp -p "$(ls -1 /etc/caddy/Caddyfile.before-drydock.* | head -n1)" /etc/caddy/Caddyfile   # the oldest backup is the original
sudo rm -rf /etc/caddy/drydock.d
sudo systemctl daemon-reload && sudo systemctl restart caddy
# 5. Data and keys. Save the DB, and secrets.key unless your secret store has it, if you might come back.
sudo rm -rf /srv/drydock/ws /var/lib/drydock
sudo shred -u /etc/drydock/secrets.key /etc/drydock/github-app.pem
sudo rm -rf /etc/drydock
# 6. The account (userdel also removes the drydock group when it is the user's own)
sudo userdel drydock; getent group drydock && sudo groupdel drydock
```

Then revoke the App key on GitHub (*Private keys → Delete*). Docker, Node, the devcontainer CLI
and Caddy stay installed, because you installed them. So do the images the workspaces were built
from and the pinned `busybox` and `node` images Drydock pulled. Deleting a workspace in the UI
removes the `vsc-repo-…` images the devcontainer CLI built for it; a workspace you never deleted
leaves its own behind, and base images (`mcr.microsoft.com/devcontainers/base`, whatever your
repositories' configurations name) are never removed. `sudo docker image ls`, and remove what you
do not want.

---

## 11. Troubleshooting

| Symptom | Likely cause | Fix |
|---|---|---|
| Installer: `--preview-domain refused: … must be a different registrable domain`, or `drydock serve` refusing to start with it | The preview domain shares the UI host's registrable domain — it is the same name, a subdomain, the parent, or a sibling (`preview.example.com` beside `drydock.example.com`), all of which make previews same-site with the UI. Or the UI host is a single label (`drydock`), which has no registrable domain to compare, so every preview domain is refused | Use a separate domain you own for previews ([8.7](#87-optional-enable-previews-no-ports-panel-yet)), or leave `--preview-domain` unset (only the front door is built: [step 9](#9-what-does-not-work-yet)). Before v0.4.3 only a subdomain was refused, so a configuration the old check let through is refused on upgrade |
| Installer: `Docker is not installed` / `there is no 'docker' group` | Docker is missing, or was installed without its group | [3.1](#31-docker-engine-official-repository) |
| Installer: `the devcontainer CLI is not installed where the service can find it` | `devcontainer` is under nvm, snap or a home directory | Install Node from NodeSource and run `npm install -g` as root ([3.2](#32-nodejs-20-and-the-devcontainer-cli-on-the-services-path)). Check with the `PATH=… command -v` line there. |
| Installer: `…installed but does not run as the drydock user` | Node older than 20 on `/usr/bin` | `node --version` with the service `PATH`. Upgrade Node. |
| Installer warning: `the drydock user cannot reach the Docker daemon yet` | The daemon is stopped | `sudo systemctl enable --now docker`, then re-run the installer |
| Installer: `the caddy user cannot read …` | Key in `/etc/ssl/private`, or mode `0600` | [4.2](#42-the-tls-certificate-and-key): group `caddy`, mode `0640`, directory `0750 root:caddy` |
| Installer: `/etc/caddy/Caddyfile was not written by this installer` | Caddy already serves other sites | Move them elsewhere, or `--take-over-caddy` (a backup is kept) |
| Installer: `the new Caddy configuration does not validate` | Bad certificate or key file, or a path typo | Read the five lines above the error. Nothing under `/etc/caddy` was changed. |
| Installer: `--github-app-id must be the numeric App ID` | The Client ID (`Iv…`) was given | `--github-app-id 5189455` |
| Installer: `Drydock is installed and running, and answers through Caddy, but this host could not verify the certificate …` | Everything works except certificate verification. `unable to get local issuer certificate`: a private CA without `--ca-cert`, or the wrong CA file, or a public certificate whose `--cert` file lacks the intermediates. `no alternative certificate subject name matches`: the certificate is for another name | Option A: re-run with `--ca-cert /etc/caddy/certs/drydock-ca.pem` ([4.2](#42-the-tls-certificate-and-key)). Otherwise check the SAN and the full chain ([1.3](#13-a-tls-certificate-every-client-trusts-for-that-hostname)). Everything else is installed, so a re-run with the fix is all it takes. |
| Installer: `… could not verify the certificate Caddy serves for drydock-check.<preview-domain> …`, or `end-to-end check failed: https://drydock-check.<preview-domain>/ answered …` (it wants `'302 https://drydock-check.<preview-domain>/.drydock/denied'`) | The UI is fine; the preview site is not. Either the preview certificate does not carry `*.<preview-domain>` or is not the full chain, or (option A) it is from a CA other than `--ca-cert`; or Caddy is not serving the preview site | Check the preview certificate as in [1.3](#13-a-tls-certificate-every-client-trusts-for-that-hostname) — the SAN must show `DNS:*.<preview-domain>` — and re-run with the right `--preview-cert`/`--preview-key` ([8.7](#87-optional-enable-previews-no-ports-panel-yet)). Or turn previews off with `--no-preview`. |
| Browser: a preview URL goes to Drydock's sign-in page every time, even signed in | The device's Drydock session is gone (signed out, *Sign out everywhere*, a password change, or 14 days unused) — a preview sign-in dies with it, by design | Sign in on the UI host and open the preview again. If it still loops, check the device allows cookies for both the UI host and the preview domain. |
| Browser: a preview URL ends on *This preview is not available* | Expected until the ports panel exists (port forwarding step 4): no port is enabled, or its workspace is not running, or its container has stopped. The page names no reason on purpose | Nothing to fix yet. *Try again* on the page restarts the sign-in. |
| Browser: a preview shows *Preview not answering* — *Nothing is answering on port N* (`502`), or *… in time* (`504`) | The port is enabled and the workspace runs, but nothing in its container accepts connections on that port: the dev server is not started, crashed, or listens on `127.0.0.1` only | Start the dev server inside the workspace listening on `0.0.0.0` (Vite: `--host 0.0.0.0`), then *Try again*. |
| Browser: a preview shows *Preview unavailable — Drydock could not look up this workspace's container* (`503`) | Drydock could not ask Docker | `journalctl -u drydock` names the reason (`drydock: preview: looking up workspace …`); check `systemctl status docker`. |
| Browser: a preview answers *Drydock is serving as many preview connections as it allows* (`503`) | More preview requests or open live-reload websockets at once than `--preview-max-connections` (512) allows | Close preview tabs left open on other devices, and try again. |
| Browser: `https://<ui-host>/preview/authorize?…` answers `{"error":{"code":"bad_request",…}}` | The link's `return` is not a preview address on `<preview-domain>`: a different domain, `http:`, a port, or a mistyped domain. Drydock never redirects anywhere else | Open the preview's own `https://<name>.<preview-domain>/` address instead. |
| Installer: `end-to-end check failed … answered '000'` | Nothing answered over TLS: Caddy is not serving this name, or the handshake failed (`curl`'s reason is in the message) | `journalctl -u caddy -u drydock`. Check the cert/key pair ([1.3](#13-a-tls-certificate-every-client-trusts-for-that-hostname)). |
| Installer: `--ca-cert … holds a private key` | The CA's key was given instead of its certificate | Pass the CA's certificate. Never copy the CA key to the server. |
| Installer: `… answered '502'` | Caddy is up, Drydock is not | `journalctl -u drydock -n 50`. Check the socket with `ls -l /run/drydock/http.sock`. |
| Installer: `the upgrade failed and was rolled back` | The new release would not start | The log above the message. Open an issue, and fix forward. |
| Installer: `secrets.key is not a 32-byte key file` | The master key is damaged or truncated | Restore it from your copy ([6.2](#62-a-generated-key-back-it-up-now)). Never move it aside unless you accept losing every secret. |
| Installer: `--secrets-key … is N bytes; a secrets master key is exactly 32 raw bytes` | The file is base64 or hex text, has a trailing newline, or is cut short | Make it with `head -c 32 /dev/urandom > secrets.key` ([4.3](#43-optional-your-own-secrets-master-key)). Nothing was changed. |
| Installer: `--secrets-key … is not the key installed at /etc/drydock/secrets.key, and N stored secret(s) are sealed under the installed key` | A different key was given after secrets were stored | Nothing was changed, and Drydock kept running. Keep the installed key: store a copy of it ([6.2](#62-a-generated-key-back-it-up-now)) and pass that, or leave the flag off. Or move keys by deleting and re-entering the secrets ([6.3](#63-switch-an-installed-key-to-one-you-supply)). |
| Installer: `… could not tell whether any secret is stored under the installed one` | `drydock count-secrets` could not read the database | The error above it. Run `sudo runuser -u drydock -- drydock count-secrets --db /var/lib/drydock/drydock.db` by hand. Nothing was changed. |
| Browser: certificate warning or `NET::ERR_CERT_AUTHORITY_INVALID` | Private CA not installed or not fully trusted on this device | Install the CA. On iOS, also enable it under *Certificate Trust Settings*. |
| Browser: `ERR_SSL_PROTOCOL_ERROR` / `SSL_ERROR_INTERNAL_ERROR_ALERT` | **Host mismatch**: you used an IP, a short name or another alias. Caddy has no site for that name, so there is no certificate to offer | Use exactly `https://drydock.example.com`. This is the DNS-rebinding defence working ([§13.3](../design/overall/drydock-design.md#133--what-a-browser-can-be-talked-into)). |
| Browser: cannot connect / times out | DNS points elsewhere, or a firewall blocks the port | `nslookup` from that device. Check `sudo ss -ltnp \| grep :443` and the firewall rules. |
| JSON `forbidden_host` | Drydock's own `Host` check, behind Caddy | Should not happen with the installer's config. Compare `systemctl cat drydock` with `/etc/drydock/drydock.env`. |
| Sign-in fails with `forbidden_origin` | The browser is on a page that is not `https://drydock.example.com` (another name for the server, or an old tab) | Open exactly `https://drydock.example.com`. The installer lowercases `--ui-host`, so case is no longer a cause. |
| Sign-in fails with *This request did not come from Drydock's own page* (`forbidden_origin`) on exactly `https://drydock.example.com`, and the browser's developer tools show the `POST /api/auth/session` request carrying **`Origin: null`** | **A release before v0.3.0** (v0.2.1 or earlier). Its UI sent every sign-in and every other change with `Origin: null` from Safari (macOS and iOS) and Firefox, which the server refuses. Chrome and other Chromium browsers were not affected | Upgrade ([10](#10-upgrade-roll-back-uninstall-logs)), then reload the page so the browser loads the new UI. Until then, sign in from Chrome or another Chromium browser. On an iPhone or iPad every browser is built on WebKit, Chrome included, so expect the same refusal from all of them until the upgrade. |
| Journal: `drydock serve: config: UIOrigin … must be lowercase` | The unit was hand-edited, or written by an installer older than `--ca-cert` | Re-run the installer: it lowercases the setting in `/etc/drydock/drydock.env`. |
| Sign-in: *too many failed sign-ins; retry after …* | Lockout: per-IP exponential backoff (up to 15 min), and a global cap of 50 failures in 15 min | Wait it out. The lockout survives a restart on purpose. |
| Forgot the password | — | `sudo -u drydock drydock passwd --db /var/lib/drydock/drydock.db` (this signs out every device) |
| UI: *No GitHub App is set up yet* / API `503 app_not_configured` | The unit has no `--github-app-id` | `systemctl cat drydock \| grep github`. Re-run the installer with `--github-app-id 5189455 --github-app-key <pem>`. |
| UI: *Could not refresh the repository list from GitHub: … 401 …*; journal `drydock: catalog refresh: github: GET /app/installations: 401 …` | **Wrong App ID or key**: the key is from a different App (the dev App?) or was deleted on GitHub. Or the server clock is off | Check `grep APP_ID /etc/drydock/drydock.env` says `5189455`. Generate a fresh key and re-run with `--github-app-key`. `timedatectl`. |
| Inside a workspace, `gh` fails with `drydock: GitHub access unavailable (the GitHub App lacks a permission; see the workspace's events in Drydock)`, or with `drydock: GitHub access unavailable (revoked)` on v0.3.0 and earlier, while `git fetch` works. The workspace's events show `GitHub refused a gh token for this workspace: the GitHub App lacks a permission this scope needs.` (on v0.3.0 and earlier, `GitHub refused a token for this workspace (revoked).`) | The App lacks one of the permissions the `gh` token asks for beyond `git`'s: **Actions: read & write** (the usual one), *Checks: read*, *Issues: read & write* or *Pull requests: read & write*. Or the App has them, but the installation has a permission request nobody accepted. GitHub refuses both with the same `422` as a repository outside the installation, which is why v0.3.0 said `revoked` | Check the App's permissions ([1.4](#14-the-github-apps-private-key)), then accept any pending request at <https://github.com/settings/installations>. Nothing in Drydock needs restarting: the next `gh` call asks again. The event's `data.permissions` lists exactly what the `gh` token asks for. The client's sentence comes from the Feature, so a container built with Feature 0.3.0 or earlier says `(app_permission_missing)` instead, until it is rebuilt. |
| UI: refreshed, `0 repositories` | The App is installed on nothing | Install it on repositories ([1.4](#14-the-github-apps-private-key)), then **Refresh catalog** |
| A repository you expected is missing | Not in the App's installation | Its row's *Installation settings* link, or GitHub → the App → *Configure* |
| Journal: `drydock: reconcile: … permission denied … docker.sock` | **Docker permission**: `drydock` is not in `docker`, or the service started before it was | `id drydock` shows `docker`. `sudo systemctl restart drydock` (new groups apply only on restart). |
| Journal: `drydock: reconcile: … Cannot connect to the Docker daemon` | The daemon is down | `sudo systemctl enable --now docker`, then `sudo systemctl restart drydock` |
| Workspace fails at *starting the container* immediately, or `devcontainer: not found` in the journal | **devcontainer CLI missing for the `drydock` user** (Node removed or upgraded, or the CLI uninstalled) | `sudo runuser -u drydock -- env PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin HOME=/var/lib/drydock devcontainer --version` must print `0.89.0`. Reinstall ([3.2](#32-nodejs-20-and-the-devcontainer-cli-on-the-services-path)). |
| Workspace fails at *starting the container* after a while | Image pull or build failure, a failing `postCreateCommand`, or no network to `ghcr.io` / `mcr.microsoft.com` | The step's detail, then `journalctl -u drydock \| grep "workspace <id>"`. Try `sudo docker pull mcr.microsoft.com/devcontainers/base:debian`. |
| Workspace fails at *verifying* | The broker round-trip failed inside the container | Check that `/run/drydock/sock/<id>/broker.sock` exists. The journal's `workspace <id>` lines. |
| Workspace fails at *cloning* | GitHub refused the clone token: the App lacks `contents`, or the repository was removed from the installation | Check the App permissions ([1.4](#14-the-github-apps-private-key)) |
| A workspace says *Needs approval* (step *resolving config*: *This configuration asks for host access that has not been approved for this repository: …* or *…differs from what was approved…*) | Not a failure. The `devcontainer.json` asks for something that reaches outside the container: `initializeCommand` (runs on the server as `drydock`), `runArgs`, `privileged`, a bind mount, `appPort`, `workspaceMount`, Docker Compose, `build.options`, a build context outside the repository, `capAdd`/`securityOpt` other than `SYS_PTRACE`/`seccomp=unconfined`, or a field Drydock does not know — and it goes beyond what was approved for this repository — a new setting, a changed value, or a list that gained an element (fewer settings than approved never ask). *from a Feature or the image* means a Feature the config declares asks for it: docker-in-docker sets `privileged`. The list and the reasons are in [§6 of the design](../design/overall/drydock-design.md#what-a-configuration-may-ask-of-the-host) | **On a create, or a repository you know needs it** (docker-in-docker): read the list and **Approve and continue** ([8.1](#81-clone-and-watch)). **On a start or rebuild of a workspace that worked before, with a setting you did not add:** something in the container changed the file — perhaps the agent "fixing" its dev container. Do not approve it unread. Look: `sudo cat /srv/drydock/ws/<id>/repo/.devcontainer/devcontainer.json`. Press **Cancel**, then restore the file by editing it as `drydock` (`sudo -u drydock nano …`) — do **not** run `git checkout` in the clone on the server, since its `.git/config` was writable by the container too — and **Start**. Or delete the workspace. Nothing in the config ran: the container was stopped before the check. |
| Approving says *The configuration changed after this was shown, so nothing was approved* | The file (or a Feature it names) changed between the page loading and the press | Read the new list the page now shows, then approve that or cancel. |
| A workspace fails at step *starting the container* with *devcontainer up asked Docker for host access the operator has not approved for this repository: … Drydock refused it, and no container was created.* | Drydock's docker guard checks every docker command `devcontainer up` runs against the repository's approval, and this one asked for more than step *resolving config* had seen — most likely a Feature or image referenced by a tag (`:1`, `:latest`) whose publisher changed it between the check and the build. Nothing was created. The journal (`journalctl -u drydock`) has the guard's line, `drydock-docker-guard: refused: <setting>: <option>`, naming the docker option ([§6 of the design](../design/overall/drydock-design.md#the-docker-guard)) | **Start** again: step *resolving config* reads the configuration afresh, sees the new setting, and asks for it as *Needs approval* — read it before approving, since you did not add it. To stop a tag moving under you, commit a `devcontainer-lock.json` (it pins Features by digest) or name the image by digest. If it repeats with nothing to approve, the guard does not know a docker option this CLI version writes; the journal line names it. |
| A workspace fails at step *starting the container* with *Drydock's docker guard had no record of what this run may ask Docker for* | The guard's policy file in `/srv/drydock/ws/<id>/.drydock/guard/` could not be read during the build — removed or damaged while it ran | **Start** again; Drydock writes it afresh for every build. If it repeats, the journal line `drydock-docker-guard: …` says why. |
| A workspace fails at step *starting the container* with *… has not approved for this repository: privileged …* (or another setting) right after a **Start** of a workspace that ran before | Its container was created with host access the repository's approval no longer covers — approved once and since narrowed, or created by a Drydock before v0.6.0, whose `up` could be given a moved tag's settings. The docker guard reads a stopped container's settings before starting it and refused this one | **Start** again: a start from *failed* recreates the container, under the current approval. If the access is wanted, the configuration step asks for it. After upgrading from an earlier release, a **Rebuild** of each workspace that runs with approved host access does the same at once. A **named volume** such a workspace was given with driver options (`volume-opt o=bind,…`) outlives both the container and a narrowed approval, and is reused by the same name: remove it (`docker volume ls`, then `docker volume rm`) after narrowing an approval that allowed one. A container using an approved `seccomp=<profile file>` is refused on every start, since docker stores the profile's content: each start recreates it. |
| A workspace fails at step *starting the container* with *… has not approved for this repository: runArgs …* on a **Start** after you changed Docker's default log driver or `log-opts` (`/etc/docker/daemon.json`), and the journal's `devcontainer up` lines say `drydock-docker-guard: refused: runArgs: a container created with LogConfig …, not the daemon's default` | The container was created under the old default. A start passes a container's log configuration only when it is a file driver with no options or exactly what the daemon now gives a new container; any other could only have come from `runArgs` (`--log-driver`, `--log-opt`). Before v0.6.1 every other Start failed this way on a host whose daemon sets any log driver or option, since the guard did not accept the daemon's own default | **Start** again: a start from *failed* recreates the container under the current default, and the clone is kept. Changing the default needs `sudo systemctl restart docker`, which stops every running workspace unless `live-restore` is set (Docker restarts none of them), so start each again afterwards. |
| A workspace fails at step *resolving config* with *devcontainer.json names a Dockerfile, build context or bind mount inside the clone that is a symbolic link leading outside it* | The path is a link out of the repository — perhaps made by something in the container. An approval is of the path, and the link could be pointed anywhere, so Drydock never runs it | Look: `sudo -u drydock ls -l /srv/drydock/ws/<id>/repo/<path>`. Replace the link with what it should hold (or name the outside path directly in `devcontainer.json`, which can then be approved), and **Start**. |
| Create refused with `at_capacity` | 10 workspaces already hold a container | Stop or delete one |
| `git push` in a container: `GitHub access unavailable …` | The workspace is not *Running* in Drydock (its socket is closed), or GitHub refused | Start it in the UI. A container started by hand with `docker start` gets no GitHub access, by design. |
| Pushing a workflow file is rejected | The App lacks `workflows: write` | Add the permission on the App, then accept the new permissions on the installation |
| Secret check prints `drydock: secrets unavailable: …`, exit 69 | Broker unreachable, or a stored secret no longer decrypts (a replaced master key) | **Secrets** lists any *undeliverable* secret. Restore the master key, or store those values again. |
| Workspace fails at *preparing credentials*: *The shared Claude credential volume drydock-claude-config holds files that belong to uid N, not to Drydock's uid…*, or the sign-in says *…holds files that belong to uid N, and Drydock runs as uid M* | The volume was written by another user: made by hand, or by an earlier Drydock whose first workspace's remote user was not `vscode`. An empty volume is fixed by itself; one with files in it is not, because re-owning a login is a decision Drydock does not make for you | Re-own it to Drydock's user, keeping whatever login is in it: `sudo docker run --rm -v drydock-claude-config:/v busybox chown -R "$(id -u drydock):$(id -g drydock)" /v`. Then **Start** or **Rebuild** the workspace, or sign in again. |
| Workspace fails at *starting the container*, and the journal's `workspace <id>` lines say `drydock feature: /home/vscode/.claude belongs to uid N (Drydock's), but the remote user … is uid M` | The repository's dev container runs as a user the dev container CLI did not move to Drydock's uid: its `devcontainer.json` sets `"updateRemoteUserUID": false`, or the image already has another user at Drydock's uid. That container could not read the shared login, so it alone is refused; every other workspace is unaffected | Remove `"updateRemoteUserUID": false` from the repository's configuration, or use an image without a user at Drydock's uid (`id -u drydock`). |
| Banner, right after a sign-in: *The Claude login expires in 8 hours. Sign in again before then.* (or 7, 6 … hours), and a dot on every card | Drydock v0.4.4 or earlier read the **access token's** expiry, which is about eight hours after every sign-in and every refresh, as the login's ([design §7.3](../design/overall/drydock-design.md#73--expiry-watch)). The login was fine | Upgrade to v0.4.5 or later (re-run the installer). The first check after the upgrade clears the banner; signing in again does not. |
| Banner: *The Claude login expires in N days. Sign in again before then.*, and a dot on every card | The login itself — Claude Code's own record of when the refresh token ends — is at most three days off. It is what Claude Code would tell you in a terminal (*Your login expires in N days · run /login to renew*) | **Sign in to Claude** (Settings) before then: one sign-in renews every workspace. **Not now** hides it on this page until the date changes; it is held in the page's memory only, so a reload or another device shows it again. |
| Settings: *Signed in. The access token has lapsed; the next session server to start renews it.* | Normal. The file records when the short-lived access token runs out, and Claude Code renews it from the stored login whenever it next runs | Nothing. Session servers still start. If the login itself has ended, the next start turns this into *Signed out. Sign in again.* |
| Banner: *Could not check the Claude login: reading the shared volume did not finish in the time allowed…* | Docker stopped answering, or something in a workspace replaced `.credentials.json` on the shared volume with something that cannot be read to its end. The last known state is kept, and the next check runs as usual | `sudo docker ps` answers? Then look inside the volume: `sudo docker run --rm -v drydock-claude-config:/v:ro busybox ls -la /v`. A `.credentials.json` that is not a plain file is not Claude Code's: remove it and sign in again. |
| A delete is stuck in *Deleting…* at a sub-step | A container still holds the mount, or the cleanup image cannot be pulled | The sub-step's detail. Press **Delete again**. A restart also resumes it. |
