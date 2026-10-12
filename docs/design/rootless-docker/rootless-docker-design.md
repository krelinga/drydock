# Drydock on non-root Docker

*An exploration, not a plan of record. The question: what would it take to run Drydock without the
`drydock` user being root on the host by way of the `docker` group? Which holes would that close,
what would it break, how much would it cost, and how would we go about it? Nothing here is built.
Claims marked **measured** were run for this document (§7 lists how). Claims marked **unmeasured**
come from reading code or vendor documentation and are what Spike 06 (§5.3) is for.*

Reads with: [overall design](../overall/drydock-design.md) §6 (*What a configuration may ask of the
host*, *The docker guard*, *Opening a workspace in VS Code*), §7.1, §13.4 and §13.5;
[port forwarding](../port-forwarding/port-forwarding-design.md) §8; the
[security review](../security-review.md); [testing](../testing/testing-design.md) §3, §4.1 and §11.2.

---

## Decisions and recommendation, in brief

- **The biggest hole is not the `docker` group. It is an approved `privileged`.** Today a
  docker-in-docker repository's container *is* host root, and an agent in it needs no exploit to
  reach the host. Every other route to host root needs a container-escape bug, a compromised
  Drydock, or an approved `initializeCommand`. Fix the zero-exploit path first.
- **Do Spike 06 first.** It is cheap and specified in §5.3. The two options worth having rest on
  claims nobody has measured on this stack: whether the devcontainers docker-in-docker Feature runs
  under Sysbox without `privileged`, and how rootless Docker treats the bind-mounted clone and the
  broker socket.
- **Recommended, if the spike is green: Sysbox as the runtime for workspaces approved for
  `privileged`.** The docker guard would turn the approved `--privileged` into
  `--runtime=sysbox-runc`, and every other workspace stays on `runc`. That closes the zero-exploit
  hole for the repositories that need docker-in-docker. It keeps every path, uid, address and
  `/proc` read Drydock relies on today, because the daemon stays rootful and Sysbox ID-maps the
  bind mounts. The cost is real but bounded:
  - The guard learns a *substitutive* argv rewrite. Today its only edit is additive (`WithLabels`),
    so this changes its character (§3.6).
  - The approval records the runtime as part of `privileged`'s value (§3.7).
  - The operator installs a third-party runtime, which costs one daemon restart and a `daemon.json`
    edit (§3.10).
  - Some UI text changes.

  Sysbox as the *default* runtime for every workspace container is the natural second step. It turns
  a container-escape bug from "host root" into "an unprivileged subuid" **for the host**. Sysbox CE
  maps every container to one common range, so an escape still reaches the other Sysbox workspaces.
  The price is a wider compatibility risk.
- **Defer rootless Docker.** It is the only option that also takes host root away from a
  compromised *Drydock* (and from `initializeCommand`, the devcontainer CLI and the docker guard).
  It costs the most by a wide margin. It breaks the preview proxy's dial, the clone's ownership
  model and the broker directory's location. It also needs a host-level AppArmor change on Ubuntu
  24.04+ (measured: blocked on this devcontainer's host). And when the daemon runs as Drydock's own
  uid, an escape lands on the uid that owns the GitHub App key and the secrets master key.
- **Do not do `userns-remap` or rootless Podman.** `userns-remap` refuses `privileged` outright
  (measured), so docker-in-docker repositories would need `--userns=host`, which is today's host
  root. Its plain mapping breaks the clone (measured), and the mapping that fixes the clone maps
  container uid 998 to `drydock`, so an escape is back in the `docker` group. Podman would need the
  docker guard rewritten for a second CLI and a second `HostConfig` dialect, and the guard was
  measured against neither.
- **What none of them closes**, and the doc says so plainly rather than leaving it implied: a
  compromised Drydock still holds every repository's GitHub access (the App key) and every secret.
  Escaping one workspace still reaches the other workspaces on the same daemon. That includes Sysbox
  CE, whose user-ID mapping is common to all its containers. The VS Code user
  still controls whatever daemon they attach to.

---

## 1. Today's threat model, precisely

The host is the trust boundary (§13.4, last row; security review §1.2 puts "root on the host" out of
scope). The installer puts `drydock` in `docker` (`deploy/install.sh`, `ensure_docker_access`, which
says *"root-equivalent; see design §13.4"*). So **every route below ends at host root**. What differs
is how many steps it takes and whether a bug is needed.

| Compromised piece | Runs as on the host | Reaches today | Steps to host root |
|---|---|---|---|
| **Drydock process** | `drydock` (in `docker`) | App key, secrets master key, database, every broker socket, the Docker socket | One: `docker run -v /:/host` (§13.4). No bug needed. |
| **devcontainer CLI** (`up`, `read-configuration`, `exec`) | `drydock`, in Drydock's unit | Same as Drydock. It runs every docker command through the guard, but the CLI process itself is not confined. | One, if the CLI or a Feature tarball it extracts is hostile (§6, *What it does not close*: node-tar's extraction). |
| **docker guard** (`internal/dockerguard`) | `drydock` (the drydock binary under the name `docker`) | Same as Drydock. It is the filter, not a sandbox. | One. |
| **`initializeCommand`** (approved) | `drydock`, **on the host** | Same as Drydock (§6, *What approval means*) | One, by the operator's choice |
| **Workspace container, nothing approved** | Container root is **host uid 0**, namespaced: no host mounts but Drydock's, default caps, seccomp, AppArmor | Its clone, its broker socket, its secrets, the shared Claude credential (§13.4, "One container") | A container-escape bug in runc or the kernel. Any escape lands as **host root**. |
| **Workspace container, approved `privileged`** (docker-in-docker) | Host uid 0, all capabilities, every device, no seccomp, AppArmor or masks | Everything: *"privileged is root on the host: Drydock's keys and every other workspace are in reach"* (the approval card's own text, `web/src/components/HostAccessApproval.vue`) | **Zero.** No bug needed (e.g. `mount /dev/sda1`, or write `release_agent` on cgroup v1). |
| **Workspace container, approved bind mount / `runArgs` / `--network host`** | Depends on what was approved | What was approved | Zero to one, by choice |
| **The VS Code SSH user** (`--vscode-ssh-host`) | Their own login, with Docker access | The daemon (§6, *Opening a workspace in VS Code*: *"Docker access, which is root on the host by another name"*) | One |
| **Ephemeral helpers** (cleanup, volume owner, identity, login, log probe) | Host uid 0 in a narrowed container | One host directory or the credential volume | A container escape, as above |

Three distinct holes come out of that table, and non-root Docker addresses each one differently:

- **H1, the approved-`privileged` hole.** It needs no bug, and it is the one docker-in-docker
  repositories (this one included) live with. Approval is a trust decision, not containment (§6).
- **H2, the escape hole.** Any runc or kernel container-escape bug from any workspace container lands
  as host root. runc's CVE-2024-21626 ("Leaky Vessels") and its later mount-race advisories are the
  class. A user namespace turns "host root" into "an unprivileged host uid".
- **H3, the `docker` group hole.** Anything that runs as `drydock` is one `docker run` away from host
  root: Drydock itself, the CLI, the guard, `initializeCommand`, a dependency of any of them.
  Drydock already holds the App key and the master key, so for *GitHub and secrets* H3 adds nothing.
  It matters only for **the host**: the owner's own home directory, SSH keys, and other services on
  the dev server.

What none of the options reaches, and §13.4 already says honestly: a compromised Drydock or
signed-in browser can do everything Drydock can. One container reaches the shared Claude
credential. Repository secrets are readable by the agent (§10.4).

> [!NOTE]
> **§13.4 dismisses rootless Docker in one sentence, and part of its reasoning overstates the cost.** It says
> rootless "breaks the path identity §1 relies on (the clone and the broker socket are bind-mounted
> by path)". A rootless daemon resolves bind sources in RootlessKit's mount namespace, which is the
> host's tree. The exceptions are **`/etc` and `/run`, which are copied up** (measured: the
> `docker:dind-rootless` entrypoint passes `--copy-up=/etc --copy-up=/run`). So the clone under
> `/srv/drydock/ws` keeps its path. The broker directory under `/run/drydock/sock` does break
> (§3.2). That is real, but fixable: move it out of `/run`. So "path identity" is a cost to pay,
> not a reason the design cannot work. The sentence's other reason,
> `updateRemoteUserUID`, holds, and §3.1 measures it. If an option here is adopted, §13.4 is
> rewritten in the same change.

---

## 2. The options

### 2.1 Rootless Docker

`dockerd-rootless-setuptool.sh` installs a daemon that runs as an ordinary user inside RootlessKit's
user, mount and network namespaces. Container uid 0 is that user. Other container uids come from
the user's `/etc/subuid` range. Networking goes through slirp4netns (the default) or pasta, with a
port driver for `-p`. Resource limits need cgroup v2 with systemd delegation, and only `memory pids`
are delegated by default
([Docker: rootless tips](https://docs.docker.com/engine/security/rootless/tips/)). The data root is
`~/.local/share/docker` and cannot be on NFS. The container IP *"is not reachable from the host
without `nsenter`-ing into the network namespace"*, and Ubuntu 24.04 needs an AppArmor profile for
`rootlesskit` because it restricts unprivileged user namespaces
([Docker: rootless troubleshooting](https://docs.docker.com/engine/security/rootless/troubleshoot/)).
In rootless mode `--privileged` grants every capability and device *of the user namespace*, which is
not host root.

The question that decides the design is **which uid runs the daemon**.

**(a) Drydock's own uid.** The daemon is a user service of `drydock` (lingering) or a system unit
with `User=drydock` and `Delegate=yes`. Drydock reaches it through its own `$XDG_RUNTIME_DIR`
socket.

- *Closes:* H1, H2 and H3 **for the host**. An escape from any container, `privileged` included,
  lands as `drydock`, which is not root and not in `docker`. A compromised Drydock cannot get host
  root either, short of a kernel bug.
- *Leaves, and makes sharper:* an escape now lands **on the uid that owns
  `/etc/drydock/github-app.pem` and `secrets.key` (`0400 drydock`) and the database**. Today an
  escape also gets those, because host root reads everything, so these assets are no worse off than
  now. They are also no better off, and the argument "rootless contains the container" must not be
  read as "rootless protects the keys". It does not, by construction. Worse, container root *is*
  `drydock` inside the user namespace. So **any bind mount of a Drydock-owned path is fully
  writable by container root**, and an approved mount of `/etc/drydock` reads the key with no escape
  at all. (Today the same is true via host root, so this is parity, not a regression.)
- *The VS Code user* needs `DOCKER_HOST` pointed at `drydock`'s socket. Whoever can use that socket
  can start a container that bind-mounts `/etc/drydock`, as `drydock`. **The SSH user would hold the
  App key.** Today they hold host root, which includes the key. So again parity for the keys, and a
  gain for the host.

**(b) A dedicated uid, `drydock-docker`.** The daemon belongs to a user with no other duties, and
Drydock reaches its socket through a group (`drydock-docker`'s runtime directory or a socket at a
fixed path, `0660`).

- *Closes:* H1, H2 and H3 for the host, as in (a). An escape lands as `drydock-docker`, which
  **cannot read the App key, the master key or the database**. That is a real inner boundary §13.4
  says "no packaging choice would add", and it is the strongest security result in this document.
  It has a price that is easy to miss: **the daemon becomes hostile input to the process that holds
  the keys**. An escapee who owns the daemon also controls what `docker inspect`, `devcontainer up`'s
  JSON result, `container.Address` and the `State.Pid` behind `container.Listeners` tell Drydock. It
  could steer the preview proxy's dial to any address Drydock can reach, point the `/proc` read at
  any PID, and fabricate labelled containers for reconciliation to adopt. In (b), every parser of
  daemon output becomes part of the boundary that keeps the keys in, and none was written to that
  standard. Today the daemon is trusted, because it is root.
- *Leaves:* an escape as `drydock-docker` controls the daemon, so it controls **every workspace
  container**: every clone, every running agent, and, through each container's mount, **every
  workspace's broker socket**. For the daemon to bind-mount `/run/drydock/sock/<id>`, that user must
  be able to traverse to it, which today's `0700` parent forbids (§6 step 5). So the escapee can
  mint a token for every repository *with a running workspace* and read every running workspace's
  granted secrets. The daemon user must likewise traverse the `0700` `/srv/drydock/ws` (§13.4) to
  mount a clone. That is less than the App key (every repository in the installation) and less
  than the master key (every secret), but not much less on a busy server.
- *Costs the most.* The clone belongs to Drydock on the host and must be writable by the daemon
  user's subuid range in the container. Drydock is neither root nor that user, so it cannot `chown`,
  and every host-side write, read and delete of the clone has to go through a helper container
  (§3.1). The CLI itself writes the clone on the host (a stale lockfile, §6) as `drydock`, and that
  would fail with `EACCES`. That is unmeasured, and blocking.

*Common to both:* the daemon is **separate from the host's rootful Docker**, which the owner may
also run. Nothing Drydock made before is visible to it (§3.10).

### 2.2 `userns-remap` on the rootful daemon

One `daemon.json` key. Every container's uids map into a subuid range of a remap user. The daemon
and its socket stay root, so **H3 is untouched** (the `docker` group is still root), and the escape
hatch `--userns=host` is a `docker run` flag. That flag is not on the guard's allowlist, so it is
`runArgs` and needs an approval. Good, but it means an approved `runArgs` can opt out.

Measured on Docker 29.9.0 (`docker:dind` with `--userns-remap=default`, §7):

- `--privileged` is **refused**: *"privileged mode is incompatible with user namespaces. You must run
  the container in the host namespace when running privileged mode"*. With `--userns=host` it runs,
  identity-mapped, which is today. **So `userns-remap` does nothing for H1.** docker-in-docker
  repositories keep host root.
- **The clone breaks.** A host directory owned by uid 998 (`0755`) appears inside as
  `65534:65534`, and neither container root nor container uid 998 can write it. After a `chown` to
  the mapped uid (`165536+998`), the container writes fine, but host uid 998 can then neither
  create nor delete in it. A default ACL granting host uid 998 `rwx` survives creation, but **one
  `chmod 0700` in the container zeroes the ACL mask** (`getfacl`: `user:998:rwx #effective:---`),
  and Drydock loses the directory. ACLs are not a boundary the container respects.
- **The data root moves** to `/var/lib/docker/165536.165536/`: every existing image, container and
  volume, the Claude credential volume included, is invisible after the switch.
- Container IPs are reachable from an unprivileged host uid, `/proc/<pid>/net/tcp6` is readable by
  one, and `docker stats` and `pause` work. Previews and usage are unaffected.

**A variant that fixes the clone, measured, and why it does not help.** `/etc/subuid` may give the
remap user several ranges. `dockremap:100000:998`, `dockremap:998:1`, `dockremap:101000:64537`
produces the map `0→100000 ×998, 998→998, 999→101000`. Container uid 998, which is what the CLI's
`updateRemoteUserUID` sets the remote user to, is host uid 998, so the clone round-trip is exactly
today's. Root-made files land as host `100000` and the existing cleanup helper removes them
(measured), and a host-root `0400` file is unreadable to container root (measured). But container
root can `setuid(998)`, so an escape lands as `drydock`, which is in `docker`, so it reaches host
root. **H2 is closed only if the mapped uid is not Drydock's.** Map container 998 to a
`drydock-ws` uid instead and an escape lands there. That uid is shared by every workspace (remap is
daemon-wide) and cannot read the keys or reach the daemon. But Drydock then needs a
`--userns=host` helper to hand each new clone to `drydock-ws`, plus the helper-mediated host access
of §3.1. It is still the rootful daemon, so it is cheap on networking. It is still useless for H1.

### 2.3 Rootless Podman (docker-compatible)

Two ways in, both bad for Drydock specifically:

- **The `podman` CLI as `--docker-path`.** CLI 0.89.0 detects Podman by running `docker -v` and
  looking for "podman" (read from `devContainersSpecCLI.js`). It then adds
  `--security-opt label=disable` to every run and **`--userns=keep-id`** whenever the remote user is
  not root. `keep-id` is exactly the mapping the clone needs: the host user is the same uid inside.
  But the docker guard refuses `--userns` (an unknown option is `runArgs`) and `label=disable` (not
  `seccomp=unconfined`), its `start` check holds Podman's `inspect` to **Docker 29's `HostConfig`
  field table** (`internal/dockerguard/start.go`, `hostConfigFields`), and Podman's differs. So the
  guard, the most security-critical component, would need a second dialect, re-recorded with
  `record.sh dockerargv` against Podman. `container.Listeners`, `usage` and the ephemeral helpers
  parse Docker's JSON as well.
- **Podman's Docker-API socket with the real docker CLI** (`DOCKER_HOST=unix:///run/user/N/podman/podman.sock`).
  The CLI then sees Docker, adds no `keep-id`, and the clone breaks as in §3.1. API-compatibility
  gaps in `inspect` and `stats` are unmeasured.

Rootless Podman's security result equals rootless Docker's (2.1) for the same uid choice. Its one
advantage, per-container `--uidmap`/`keep-id` (and `:idmap` bind mounts, which Docker 29's CLI does
not offer: `docker run --help` lists `--userns` and nothing ID-mapped), does not repay the cost of a
second container engine under the guard.

### 2.4 Sysbox as a runtime (with the rootful daemon)

[Sysbox](https://github.com/nestybox/sysbox) is an OCI runtime (`sysbox-runc`, plus `sysbox-mgr` and
`sysbox-fs` daemons) that always runs containers in a user namespace and emulates enough of `/proc`
and `/sys` that **systemd, dockerd and Kubernetes run inside without `--privileged`**. Bind mounts
are ID-mapped by the kernel or go through the shiftfs module, so *"files owned by users in [0:65536] show up
with the same ownership inside"*. Container root writing to a bind mount *"will do so as if it were
the root user on the host"* (Sysbox docs, `user-guide/storage.md`). The Sysbox limitations page says
`--privileged`, `--userns=host`, `--pid=host` and `--net=host` *"do not work"* with it, and that
*"Sysbox must run as root"*: **it is a rootful-daemon runtime, not a rootless one**. Kernel:
shiftfs is required below 5.12 and recommended from 5.12 up to 6.3, and only from 6.3 is it
unnecessary (`install-package.md`; `distro-compat.md` says 5.19, so take the stricter figure until
P3 measures it). Installing it is not free on a live host:
- The install guide recommends *"stopping and removing all Docker containers"* first.
- The package adds a `runtimes` entry to `/etc/docker/daemon.json` and restarts dockerd. It skips
  the restart only if `bip` or `default-address-pools` is already set there.
- Docker installed as a snap is unsupported.

**Sysbox CE's uid mapping is common to all containers**, and that is documented, not a question.
Its `security.md` says *"the root user in all containers is mapped to the same user-ID on the
host"*, and that *"container-to-container is not as strong as it could be"*. Exclusive per-container
ranges are an Enterprise Edition feature. The project is
Docker-sponsored since 2022 and supported best-effort. Releases: v0.6.6 (2025-01), v0.6.7 (2025-05),
v0.7.0 (2026-06), v0.7.1 (2026-07-31, which fixes Ubuntu 24.04 on kernel 6.8+).

- *Closes:* **H1** for every repository whose `privileged` exists only to run dockerd inside, which
  is docker-in-docker, the reason `privileged` is in the approval list. **H2 for the host** for
  every container put on it: an escape lands as an unprivileged subuid. That subuid is **the same
  for every Sysbox container**, so an escape from one Sysbox workspace reaches every other Sysbox
  workspace's processes and files.
- *Not closed by the translation:* a workspace whose configuration uses **Compose**. The guard
  approves `dockerComposeFile` as a whole and passes Compose's commands (`check.go`), and Compose
  creates containers through the API, so there is no `--privileged` in argv to translate. A
  privileged Compose workspace keeps today's meaning, host root, and its approval must say so.
- *A non-escape path that remains:* container root on an ID-mapped bind mount writes **as host
  root** (quoted above). So an approved DinD container under Sysbox can still plant host-uid-0
  files, setuid-root binaries included, in its clone and its broker directory. That is parity with
  runc today, not a regression, and such a file still needs something on the host to run it. Drydock's
  unit has `NoNewPrivileges=yes`, but the VS Code SSH user and that user's `initializeCommand` do
  not. Mounting the workspace root `nosuid` is a cheap hardening that stands on its own (§6).
- *Leaves:* **H3** unchanged (rootful daemon, `docker` group). `initializeCommand` and the CLI are
  unchanged.
- *Breaks little:* the daemon, its addresses, PIDs, `/proc`, `docker stats`, labels and storage stay
  as they are. ID-mapped bind mounts mean the clone's ownership, `updateRemoteUserUID` and the
  root-owned-leftover cleanup all behave as today. The credential volume is a plain directory, as
  now. *What it does break:* the docker-in-docker **Feature** declares `privileged: true`, and Sysbox
  refuses that flag. So Drydock has to translate the approved `privileged` into
  `--runtime=sysbox-runc` instead of passing it (§3.6). Whether the Feature's entrypoint then
  succeeds without the capabilities it expects is **the central unmeasured claim**.

### 2.5 Compared

| | Rootless, Drydock's uid | Rootless, dedicated uid | `userns-remap` | Rootless Podman | **Sysbox (privileged-only, then default)** |
|---|---|---|---|---|---|
| **H1** approved `privileged` = host root | Closed for the host. The escapee holds the keys. | Closed. The escapee holds the daemon. | **Open**: `privileged` needs `--userns=host` (measured) | As rootless Docker | **Closed** for `docker run` DinD, if the Feature runs (unmeasured). Open for Compose. Container root still writes host-root files on its bind mounts. |
| **H2** escape = host root | Closed for the host | Closed | Closed only with a non-Drydock mapped uid | As rootless Docker | Closed **for the host**, for containers on Sysbox |
| **H3** `docker` group = root | Closed | Closed, and the keys are out of the escapee's reach | Open | Closed | Open |
| An escape reaches the App key / master key | Yes (same uid) | **No** | Yes | Per uid choice | No (subuid), unless it then finds a kernel bug |
| An escape reaches other workspaces | Yes | Yes (daemon + every mounted broker socket) | Yes (shared range) | Per uid choice | **Yes**, the other Sysbox ones (CE's common mapping, documented) |
| Clone ownership / `updateRemoteUserUID` | **Breaks**: needs helpers or a non-root mapping | **Breaks**, worse | **Breaks** (measured), or remapped to `drydock` | Works via `keep-id`, but the guard refuses it | **Unchanged** (ID-mapped mounts, unmeasured on Drydock) |
| Preview dial to the container IP | **Breaks**: IP lives in RootlessKit's netns | **Breaks**, and Drydock cannot `nsenter` another uid's namespace | Works (measured) | Breaks (rootless netns) | Unchanged |
| Discovery `/proc/<pid>/net` | Probably works (host PIDs, no PID namespace in RootlessKit's flags; unmeasured) | Same | Works (measured) | Probably works | Unmeasured (sysbox-fs emulates `/proc`, but this is the host's `/proc`) |
| `docker stats` / `pause` | Needs cgroup v2 + delegation (memory delegated by default) | Same | Works (measured) | Same | Unmeasured |
| Broker dir under `/run` | **Breaks**: `/run` is copied up (§3.2) | Breaks, and needs group traversal | Works | Breaks the same way | Works |
| docker guard | Same argv, `privileged` means less | Same | Must allow nothing new (`--userns=host` stays `runArgs`) | **Second dialect** | Translate `--privileged`; allow `Runtime: sysbox-runc` |
| Migration | New daemon: rebuild everything, re-login | Same | Data root moves: rebuild, re-login | New engine | One dockerd restart with every workspace stopped. Images, volumes and the login are kept. A privileged workspace is rebuilt once. |
| Host changes | uidmap, subuid, AppArmor profile, linger or `Delegate=`, rootless extras | Same plus a user and group | One `daemon.json` key | Podman, the same userns plumbing | Sysbox package (systemd units), `daemon.json` edited (`runtimes`, maybe `bip`/`default-address-pools`), kernel ≥ 6.3 to avoid shiftfs, no snap Docker |
| Code + docs + tests (rough, §4) | ~4–6k lines, many PRs | ~6–9k lines | ~2–3k lines, for little | ~5–8k lines | **~1.8–2.4k lines**, then ~0.5k for default-on |

---

## 3. What breaks or changes in Drydock, area by area

Each item names the code, says what happens today, and says what happens under each option that
changes it. Where an option is not mentioned it is unaffected.

### 3.1 The clone and file ownership

**Today.** Drydock clones as `drydock` into `/srv/drydock/ws/<id>/repo` (§6 step 2,
`internal/clone`). The CLI's `updateRemoteUserUID` rebuilds the image with the remote user at
`NEW_UID=getuid()`, Drydock's uid (read from CLI 0.89.0's `updateUID.Dockerfile` build args). So the
agent writes the clone as host `drydock`. Root-owned leftovers from `sudo` or a build go through the
cleanup helper at delete (§6, *What the host cannot remove*; `internal/container/cleanup.go`). On the
host, Drydock reads the lockfile's bytes (§6, *The repository's lockfile*), walks the tree for disk
usage (`sys.HostDisk`), and deletes it. The CLI writes a stale lockfile into it **on the host**.

**Rootless (either uid), `userns-remap` (plain), Podman over the API socket.** Host uid
`drydock` is not container uid `drydock` (measured for remap. For rootless it is container *root*,
by construction), so the agent, running as the remote user at Drydock's numeric uid, cannot write
the clone. Ways out, none free:

1. **Hand the clone to the mapped uid after cloning**, through a new `ephemeral` helper kind (say
   `chown`), and do all later host-side access through helpers. The CLI's own lockfile write on the
   host then fails (unmeasured: it may fail `up` outright). The disk walk becomes `partial` wherever
   the container made a directory private. Delete already uses a helper and keeps working
   (measured for remap: a `DAC_OVERRIDE`+`FOWNER` helper in the namespace empties the tree).
2. **Map the remote user to Drydock's host uid**: Podman's `keep-id`, or remap's multi-range trick
   (measured to work). Everything behaves as today, and an escape lands as `drydock` (§2.2).
   Rootless Docker has no `keep-id`.
3. **Run the agent as container root** (rootless (a): container root is `drydock`). This breaks the
   Feature's and the minimal config's assumption of a non-root `vscode` user (§6 step 3) and the
   credential volume's ownership rule (§7.1).
4. **ACLs**: refuted. A container `chmod` zeroes the mask (measured, §2.2).

**Sysbox.** Unchanged, if ID-mapped mounts behave as documented. Container uid 998 writes as host
998, and container root writes as host root, so leftovers still need the cleanup helper (unmeasured
on Drydock: P3 in §5.3).

### 3.2 The broker socket directory

Today `/run/drydock/sock/<id>` is bind-mounted, a directory, so a restarted Drydock's new socket is
seen (§6 step 5). `RuntimeDirectoryPreserve=yes` keeps it across restarts.

- **Rootless:** RootlessKit copies up `/run` (measured in `docker:dind-rootless`'s entrypoint). The
  daemon sees a tmpfs populated from `/run` *as it was when the daemon started*. RootlessKit's
  default copy-up driver, `tmpfs+symlink`, replaces each top-level entry of `/run` with a symlink
  into a bind of the original, so a *child* of an entry that existed at daemon start stays live,
  while a *new top-level entry* is invisible. That is the documented mechanism, not measured here.
  Drydock's unit starts after Docker, so `/run/drydock` itself may well be such a new top-level
  entry, and then every broker mount fails. P4 probes both orders. **The fix is to move the directory out of `/run`**
  (e.g. `/var/lib/drydock/sock`, or beside the workspace root). That loses the "tmpfs, gone at
  reboot" property the delete step's note leans on (`broker_socket`: *"stays until the next reboot
  — the directory is on a tmpfs"*). With a dedicated daemon uid, the `0700` parent must open to that
  uid's group, and §2.1(b) explains what that costs.
- **Socket permissions:** `0666` in a `0755` directory (`internal/broker/broker.go`). A container
  uid mapped to any subuid can still connect. Unchanged.

### 3.3 The shared Claude credential volume (§7.1)

- **Atomic `mkdir`:** a local volume is a plain directory on the data root's filesystem under every
  option, so Spike 00's guarantee holds. A concurrent `mkdir` race is measured only trivially under
  remap (the second `mkdir` failed with *File exists*). **Under Sysbox-for-privileged the volume is
  shared between runtimes.** Sysbox and runc workspaces race the `.oauth_refresh.lock` and
  `.claude.json.lock` `mkdir` against each other, and the 0600 credential must show the same owner
  to both. Sysbox without ID-mapping on volumes shows foreign-owned files as `nobody:nogroup`, which
  would sign every Sysbox workspace out (P5). Rootless's data root must not be NFS. That
  is Docker's own rule, and the Feature's `fuse.*`/`nfs*` refusal still catches a data root on one.
  Rootless with `fuse-overlayfs` affects image layers, not volumes (unmeasured).
- **Ownership:** the owner helper (`internal/container/volumeowner.go`) `chown`s to `DRYDOCK_UID`
  *inside the container*, which is the container uid the CLI gives every remote user. Under any
  mapping that stays consistent, because every workspace's remote user is container uid
  `getuid()`, so the login is shared as today. Under rootless (a), container uid 998 is a subuid,
  not `drydock`, which is harmless because the host never reads the volume directly (§7.3 reads it
  through containers already).
- **Migration:** the volume lives in the daemon's storage. A new daemon (rootless, Podman) or a
  moved data root (remap) has none, and **"nothing in Drydock removes the volume"** cuts both ways:
  the old one stays on the old daemon and the operator signs in once more on the new one.

### 3.4 Previews: the dial and discovery

**The dial.** `container.Address` resolves the container by label and returns its bridge IP
(`internal/container/address.go`). The preview proxy then `net.Dial`s `IP:port`
(`internal/preview/proxy.go`). A loopback or host-network address is refused by design (PF §8.1).

- **Rootless:** the bridge is inside RootlessKit's network namespace, *"not reachable from the
  host without nsenter-ing"* (Docker docs). Options: (i) Drydock dials from a locked OS thread that
  has `setns`'d into the daemon's user and network namespace. That is possible only when Drydock is
  the daemon's uid (2.1a), and it is new low-level code with a rule-3 bookkeeping story. (ii) A
  long-lived **relay** inside the namespace, reached over a Unix socket. That is a new component,
  and an `ephemeral` helper is the wrong shape for it because it is not short-lived. (iii) Publish
  ports with `-p`: refused, because that is `appPort` host access and RootlessKit's port driver
  exposes it on the host. Any of these reopens PF §8.1's "bound to the freshly-resolved container"
  argument for review. **This is the single largest code change rootless requires.**
- **Remap, Sysbox:** unchanged (measured for remap; Sysbox uses Docker's bridge).

**Discovery.** `container.Listeners` reads `/proc/<pid>/net/tcp{,6}` from the host for the PID
`inspect` reports (`internal/container/listeners.go`, whose comment notes that `/proc/<pid>/net`
"sits outside the ptrace check").

- **Remap:** works (measured: unprivileged host uid read the remapped container's `tcp6`, socket
  uid shown as `165536`).
- **Rootless:** RootlessKit's flags include no `--pidns` (read in the entrypoint), so `inspect`'s
  `State.Pid` should be a host PID and the read should work. Unmeasured.
- **Sysbox:** the read is of the *host's* `/proc`, which sysbox-fs does not touch, so it should work.
  Unmeasured.

### 3.5 Usage, pause and reconciliation

- **Memory** is `docker stats` (`internal/container/usage.go`). Rootless needs cgroup v2 with
  systemd delegation. Memory is delegated by default, so this should work, and CPU limits would
  need `Delegate=cpu cpuset io memory pids`. Remap: measured working. Sysbox: unmeasured.
- **Disk** is a host walk plus `SizeRw`. It becomes `partial` wherever host access to the clone is
  lost (3.1).
- **`pause`:** boot handles a paused container (CLAUDE.md, `internal/provision`). Remap: measured
  working. Rootless needs cgroup v2's freezer, which comes with v2.
- **Reconciliation and labels** (§6, *Reconciliation on boot*): unchanged in mechanism. A **new
  daemon is a new truth**. Under rootless or Podman, every `running` row finds no container and is
  marked `stopped`, which is correct by the table, and every container on the old daemon is
  invisible, neither adopted nor removed. The single-instance rule (the label prefix) is per daemon.
  Two Drydocks on two daemons cannot see each other's containers at all, which is safer than today,
  but a test Drydock on the host's rootful daemon no longer exercises the production daemon.

### 3.6 The docker guard

The guard checks argv and, for `start`, the container's `HostConfig` against a table of Docker 29
fields (`internal/dockerguard/start.go`). That table accepts `Runtime` of `""` or `runc` only and
`UsernsMode` of `""` only.

- **Remap:** `HostConfig.UsernsMode` stays `""` for remapped containers, so no change is needed.
  `--userns=host` remains `runArgs` (an unknown option), which needs an approval. **The approval
  text must then say that `runArgs` can undo the remap.**
- **Rootless:** same argv. The policy is unchanged, but **`privileged` means less**, and the
  approval wording, which keys off the field name (`HostAccessApproval.vue`), must change with the
  deployment.
- **Sysbox (privileged-only): the guard becomes a transformer, not only a filter.** Today it
  makes one argv edit, and that edit is *additive*. `WithLabels` (`internal/dockerguard/guard.go`)
  inserts the policy's `devcontainer.local_folder` and `devcontainer.config_file` labels right after
  `run`/`create`. `Main` then runs `Check` on the result and `execve`s the real docker with it
  (`check.go`'s package comment: *"argv untouched but for WithLabels"*). Design §6 says the same in
  *Opening a workspace in VS Code*: *"the one change it makes to an argv"*. Swapping `--privileged`
  for `--runtime=sysbox-runc` is a different kind of edit: it **removes** an option the CLI wrote
  and **substitutes** another. That makes the guard decide what the container is, not only whether
  it may exist. Its design needs:
  - **Rewrite only inside the option region the parser has already delimited**, between `run` (or
    `create`, or `container run`/`create`) and the image. The same table-driven pass `Check` makes
    (the `runFlags` table, with its arities) finds the region. A `--privileged` after the image is the
    container's command and must stay exactly as it is.
  - **Every spelling the parser accepts.** `--privileged` is a bool option, so `--privileged`,
    `--privileged=true` and `--privileged=false` all parse. Only a true value is swapped. `=false` is
    dropped or left alone, never turned into a runtime. A second `--privileged` is resolved by
    docker's last-wins rule, the way the parser reads it.
  - **Refuse an existing `--runtime`** in the same argv. It is not on the allowlist today (it would
    be `runArgs`), and with the swap it must never be both approved and substituted.
  - **Check the rewritten argv as a second pass** against a policy in which `privileged` is not
    approved but the Sysbox runtime is. The command docker will run is held to the rules, not only
    the command the CLI wrote, as `WithLabels` is today.
  - **`exec --privileged`** (`check.go`'s `exec` rule) into a Sysbox container is either refused or
    passed without the flag. Which one Sysbox needs is unmeasured (P8).
  - **`start`** accepts `Runtime: sysbox-runc` exactly when the policy's `privileged` is the Sysbox
    value (§3.7), and holds `Privileged` to `false` for such a container. Sysbox's own `MaskedPaths`
    and `ReadonlyPaths` are recorded (P10) and allowed only beside that runtime.
  - **Docs:** §6 *The docker guard* ("replaces itself with the real docker by `execve`, so argv … are
    docker's own, exactly", and the *Opening a workspace in VS Code* paragraph's "the one change it
    makes"), §13.5's guard sentence, CLAUDE.md's `internal/dockerguard` row, and the package comment
    all say "one additive edit" today, and all change in the same PR.

  **Sysbox as default:** the daemon's `default-runtime` fills `HostConfig.Runtime` at create
  (unmeasured), so every `start` check must accept it, and `--runtime=runc` in argv becomes the
  opt-out to refuse. The ephemeral helpers are created by Drydock directly, not through the guard,
  and stay `runc` explicitly (§6).
- **Podman:** a second dialect (2.3). Not recommended.

### 3.7 The host-access approval and its words

Today the card says *"privileged is root on the host: Drydock's keys and every other workspace are in
reach."* Under Sysbox-for-privileged it would say something like *"privileged runs docker-in-docker
in a Sysbox container: it is not root on the host, but a kernel bug would make it so"*. **The
approval has to record which meaning it was given under.** An approval of `privileged`-as-Sysbox
must not quietly become `privileged`-as-host-root if the operator later uninstalls Sysbox or turns
the mapping off.

That is decided where runs are allowed, not in the subset hash. Step 3 allows a run with
`container.Covered(approved, current)` (`internal/container/policy.go`), which compares settings by
field and source and then by canonical value. At `up`, the guard enforces the approved *settings*
it reads from `policy.json`. A mode recorded only in the hash would change neither of them. A
Sysbox-era approval would still pass `Covered` after the flag flipped, and the guard would pass
`--privileged` to runc: exactly the silent widening this section forbids. So **the mode is part of
the setting's value**:
- Step 3 computes `privileged`'s current value as `"sysbox"` when the deployment maps it, and as
  `true` otherwise.
- `Covered`'s value comparison then refuses across modes, in both directions, and the operator is
  asked again, with the change shown as `true → "sysbox"`.
- The guard reads the same value from `policy.json` and swaps only for `"sysbox"`.
- A start of an existing container is checked against the same value (§3.6).

An approval given before the feature existed (`true`) keeps meaning host root, and turning Sysbox
on asks again rather than reinterpreting it.

**The new value is not safe on its own, because of how the guard reads values today.** `Policy.has`
(`internal/dockerguard/check.go`) treats any approved value that is not literally `false` as
approved. So `"sysbox"` would pass the existing `privileged` rules unchanged: `run`'s `--privileged`
check, `exec --privileged`, and `start`'s *"a container created --privileged"*
(`internal/dockerguard/start.go`). Nothing would refuse a runc container with `Privileged: true`.
The migration path reaches exactly that case:
- A workspace stopped before the switch still has its runc `--privileged` container.
- An approval continues the stopped run with the request's own `RemoveExisting`
  (`internal/provision/provision.go`, `removeExisting = pending.RemoveExisting`), which is `false`
  for a start from `stopped`.
- So `up` would `start` the old container, and the guard's start check would pass it.

That is host root under an approval the operator was told meant Sysbox. Four changes close it:
1. **The `privileged` rules accept only the literal `true`.** Every `has("privileged")` in
   `run`/`create`, `exec` and `start` compares the value with `true`, not "not false". `"sysbox"`
   matches only the Sysbox rules of §3.6. The same audit applies to every other caller of `has` as
   values gain shapes.
2. **`start` refuses a runc container with `Privileged: true` when the approved value is
   `"sysbox"`.** The refusal names the mode change, and it is recorded in `refused.json` like any
   other. It also refuses a Sysbox container when the approved value is `true`, which is the reverse
   switch.
3. **A mode change forces a recreate.** When the approval being granted differs from the
   repository's previous one in `privileged`'s value, provisioning passes `RemoveExisting = true`
   whatever the request carried, so `up` runs `docker run` under the swap. This adds no new state,
   because the diff is already computed for the card.
4. **Belt and braces:** if a start still reaches the guard with the old container, the refusal in
   (2) fails the run. That ends it `failed`, and the next start from `failed` passes
   `--remove-existing-container` (provision.go, `removeExisting := rebuild || w.State ==
   workspace.Failed`), which recreates under Sysbox. The operator sees one refused start rather than
   a host-root container. Under
rootless, the honest sentence is *"privileged is root over every workspace and, if Drydock runs the
daemon itself, Drydock's keys"*.

### 3.8 The devcontainer CLI, `initializeCommand` and docker-in-docker Features

- **Reaching the daemon:** rootless and Podman need `DOCKER_HOST`. Most docker invocations inherit
  Drydock's environment (`up` passes `os.Environ()` plus `TMPDIR`, `internal/container/container.go`).
  The guard refuses `-H` and `--context` as global options, so the variable has to come from the
  unit's environment (`/etc/drydock/drydock.env`), not argv. That is a change to the README's
  contract file, so the deploy documents move with it.
- **`initializeCommand`** still runs on the host as `drydock`. Rootless (a) or (b) takes host root
  away from it, but under (a) it still holds the keys. Remap and Sysbox change nothing about it.
  **It stays the most dangerous field to approve under every option**, and the approval card should
  say so whatever is chosen.
- **docker-in-docker Feature** (`ghcr.io/devcontainers/features/docker-in-docker`): declares
  `privileged`, runs a rootful dockerd inside, and mounts a `${devcontainerId}` volume for
  `/var/lib/docker`. Under rootless it runs inside a user namespace with all of that namespace's
  capabilities. Docker's own `dind-rootless` image needs `--privileged` for the outer container. A
  *rootful* inner dockerd in a rootless privileged container (cgroup v2 nesting, overlay2 in a user
  namespace on kernel ≥ 5.11) is unmeasured. Under Sysbox the Feature runs *without* `privileged`
  (Drydock removed it), which is exactly what Sysbox is for. But the Feature's scripts are
  unmeasured there. **A repository cannot turn the Feature's `privileged` off.** The merged
  configuration's `privileged` is true if any source sets it (§6 step 3, *measured*: `privileged`
  merges as a boolean), so `"privileged": false` in `devcontainer.json` does nothing. If the
  Feature's scripts refuse to run under Sysbox, the fallbacks are a Feature option upstream, a
  Drydock-maintained fork of the Feature, or extra work in the guard's translation. That is why P8
  must run the CLI's real argv with only the guard's swap applied.

### 3.9 Open in VS Code (`internal/vscode`)

The link makes the operator's VS Code run docker over Remote-SSH as **the SSH user**, against
whatever daemon that user's docker CLI reaches.

- **Rootful, remap, Sysbox:** unchanged. The SSH user is in `docker`, which is root (as today).
  Under Sysbox they could still start a non-Sysbox privileged container by hand. Drydock does not
  stand between them and the daemon.
- **Rootless (a):** the SSH user needs `drydock`'s socket (`/run/user/<uid>/docker.sock`, which is
  `0700` by default), so access has to be granted deliberately: a `docker context` plus a group, or
  ACLs on the runtime directory. **Whoever attaches is the `drydock` user, App key and master key
  included.** That needs saying beside `--vscode-ssh-host`, and an operator may reasonably prefer to
  drop the feature.
- **Rootless (b):** the SSH user joins `drydock-docker`'s socket group and is then that user:
  every workspace, but not the keys.
- In both rootless cases the extension's *Reopen in Container* (which builds a container from the
  folder) has to reach the same daemon, or it builds a second container on the host's rootful one,
  the failure §6 labels against.

### 3.10 Installer, units, runbooks and migration

- **`deploy/install.sh`** today checks `docker` and the `docker` group and adds `drydock` to it
  (`ensure_docker_access`). It writes a system unit with `NoNewPrivileges=yes`, `ProtectSystem=strict`,
  `ProtectHome=yes` and `PrivateTmp=yes`.
  - *Sysbox:* detect `sysbox-runc` in `docker info`'s runtimes and refuse the setting when it is
    absent. Installing Sysbox stays the operator's job, as Docker is today: a `.deb` from the
    release, pinned by SHA-256 in the runbook. That is roughly a new flag plus one `drydock.env`
    key. The runbook must say what the Sysbox package does to the host:
    - it edits `/etc/docker/daemon.json`, adding `runtimes` and possibly `bip` and
      `default-address-pools`;
    - it restarts dockerd unless one of those keys is already there;
    - Sysbox recommends stopping and removing every container first;
    - snap Docker is unsupported.

    That is a daemon-wide change to the owner's host, the same objection raised against remap below,
    except that here nothing moves: the data root, the images and the volumes stay.
  - *Rootless:* `uidmap`, `/etc/subuid` and `/etc/subgid` ranges, `docker-ce-rootless-extras`, an
    AppArmor profile for `rootlesskit` on Ubuntu 24.04+, then **either** `loginctl enable-linger`
    and a user unit for a system account with no shell, **or** a system unit with `User=` and
    `Delegate=yes`. Rootless's `newuidmap` is setuid or file-capable, so the daemon's unit cannot
    carry `NoNewPrivileges=yes`. It must be a separate unit from Drydock's. The data root and
    `XDG_RUNTIME_DIR` move outside `ProtectHome`. The broker directory leaves `/run`. A new
    `DOCKER_HOST` goes into `drydock.env`. The `docker` group step is removed, and the upgrade path
    must take `drydock` *out* of the group, or nothing is gained. This is most of a second installer,
    and `test/install` (systemd in a container) would need user namespaces inside it.
  - *Remap:* edits the host's `daemon.json`, which is **daemon-wide**: the owner's other containers
    move to the remapped data root too. Drydock's installer should not do this to someone's host.
- **`docs/deploy/first-deployment.md`** and **`-ansible.md`** name every command and journal line
  (CLAUDE.md, *The deploy documents*). Every option above that touches the installer updates both,
  and `test/ansible`.
- **Migration** of a live install, for any option that changes the daemon or its data root: stop
  every workspace on the old daemon, keep the clones (they are on host disk and survive, §6), switch,
  sign Claude in again (new credential volume), and **Rebuild** each workspace. **Sysbox is not
  migration-free either**, though it keeps the daemon, so it needs no new sign-in:
  1. Stop every workspace from Drydock, and stop Drydock.
  2. Install the package. This edits `daemon.json` and restarts dockerd.
  3. Start Drydock.

  The images, the named volumes and the credential volume stay, so the Claude login survives and
  no one signs in again.

  **Whether to remove the containers is the operator's choice, and the two choices differ.**
  - *Keeping them*, against Sysbox's advice: each stopped workspace starts again on its own
    container. Runc workspaces are unaffected.
  - *Removing them*, as Sysbox recommends: each workspace's next start fails at `up`, since the
    container is gone and the row says `stopped`. It then needs a **Rebuild**, or a start from
    `failed`, which recreates it from the clone.

  A recreate keeps the clone and the named volumes, the Feature's `${devcontainerId}`
  `/var/lib/docker` included. It **loses the container's writable layer**: packages installed after
  create, everything in the remote user's `$HOME` outside the clone and the mounted credential
  directory (shell history, tool caches such as `~/.cache`, `~/.npm`, `~/go/pkg`), and anything
  written to `/tmp` or `/var`. A `postCreateCommand` re-runs. Whether that matters depends on how
  much state the agents leave outside the clone, and the runbook should say so before recommending
  removal.

  Turning the mapping on changes `privileged`'s value (§3.7). So each approved-`privileged`
  workspace asks for approval again, and the approval forces the recreate under Sysbox (§3.7,
  change 3). The old daemon's images, volumes, including
  each docker-in-docker workspace's inner `/var/lib/docker` volume, and the old credential volume
  stay where they were. Drydock removes none of them (§6 step 4, §6 *Delete*), and the runbook
  would have to say how the operator removes them by hand.

---

## 4. How much complexity

Rough sizes are lines of Go, TypeScript, shell and docs changed or added, tests included. They come
from the size of the files named, not from building anything.

**Sysbox for approved `privileged` (recommended):**

| Item | Where | Size |
|---|---|---|
| Config: `--privileged-runtime=sysbox-runc` (off by default), `Validate`, a check against `docker info` at startup | `internal/config`, `cmd/drydock` | ~100 |
| Guard: the substitutive rewrite inside the parsed option region (every `--privileged` spelling, a refused existing `--runtime`, a second check of the rewritten argv), `exec --privileged`, `start` accepting `Runtime` and Sysbox's masked paths, recorded fixtures, mutation-checked negative tests (testing §4.1), and the §6, §13.5, CLAUDE.md and package-comment text that says "one additive edit" today | `internal/dockerguard`, `test/fixtures/devcontainer`, docs | ~700–1,000 |
| Approval: `privileged`'s value carries the mode (`true` or `"sysbox"`) in step 3, `Covered`, `policy.json` and the guard; the diff shows `true → "sysbox"`; the guard's `privileged` rules accept only the literal `true`, and `start` refuses a cross-mode container; a mode change forces `RemoveExisting` | `internal/container/policy.go`, `internal/workspace`, `internal/dockerguard`, `internal/provision` | ~400 |
| UI wording, both meanings, tests, `dist` rebuild | `web/` | ~80 |
| Installer flag, detection, env key, runbook and Ansible | `deploy/`, `docs/deploy/` | ~150 + docs |
| Container-tier tests (§5) | `test/container` | ~300 |
| Design doc updates (§6, §13.4, §13.5, CLAUDE.md invariants) | `docs/` | ~100 |
| **Total** | 4–5 PRs | **~1.8–2.4k** |
| *Then Sysbox as default runtime* | guard start check, opt-out refusal, ephemeral helpers stay `runc` explicitly | ~400–600 |

*Ongoing:* the guard is now a transformer, and every later rule has to be read against the rewrite
as well as the original argv. There is also a third-party runtime on the critical path. Every kernel or Docker upgrade on the dev
server is a Sysbox compatibility question. The §11.2 ritual (CLI or Docker bump) grows a Sysbox row,
and a Sysbox release that lags a kernel blocks the kernel.

**Rootless Docker, Drydock's uid (deferred):**

| Item | Where | Size |
|---|---|---|
| Preview dial into the namespace (setns dialer or relay) and its PF §8.1 re-argument | `internal/preview`, `internal/container` | ~800–1,200 |
| Clone ownership: helper kind, host-side reads through helpers, CLI lockfile write | `internal/ephemeral`, `internal/clone`, `internal/container`, `internal/provision` | ~800 |
| Broker directory out of `/run`, delete semantics | `internal/broker`, `internal/provision`, installer | ~300 |
| `DOCKER_HOST` plumbing, guard and approval wording | several | ~300 |
| Installer: rootless setup, AppArmor, units, removing the `docker` group, upgrade | `deploy/install.sh`, `test/install` | ~800–1,200 |
| Runbook, Ansible, VS Code docs | `docs/deploy`, `test/ansible` | large |
| Container tier on a rootless daemon in CI | `.github/actions/go-suite`, `test/container` | ~500 + CI time |
| **Total** | 8–12 PRs | **~4–6k**, more for (b) |

*Ongoing:* two daemons on the host, if the owner keeps his own rootful Docker. Rootless networking
quirks (MTU, source IP, `ping_group_range`). Cgroup delegation. AppArmor profile drift on Ubuntu
upgrades. A test environment that cannot be the devcontainer (§5.1).

**Unknowns that could move these numbers a lot:** whether the docker-in-docker Feature runs under
Sysbox unmodified; whether the CLI tolerates a clone it cannot write; RootlessKit copy-up semantics
for a directory created after daemon start.

---

## 5. Testing

### 5.1 Where it can run

- **This devcontainer:** rootless cannot run here. Measured: the VM's kernel has
  `kernel.apparmor_restrict_unprivileged_userns=1`, `unshare -Ur` fails, and `docker:dind-rootless`
  dies with `[rootlesskit:parent] error: failed to start the child: fork/exec /proc/self/exe:
  operation not permitted`. Changing that sysctl changes the host VM and is out of bounds for a
  test. `userns-remap` *can* run here, inside a throwaway privileged `docker:dind`, which is how §2.2
  was measured. Sysbox needs its daemons on the host kernel. Whether it runs inside a privileged
  container is unmeasured, and Nestybox does not document it as supported.
- **GitHub-hosted `ubuntu-24.04` runners** are disposable VMs with passwordless `sudo`, so a job can
  install Sysbox (stop Docker, `dpkg -i`, start), or set the AppArmor sysctl and run
  `dockerd-rootless-setuptool.sh`. Whether the runner image already relaxes the userns restriction,
  and whether Sysbox's `.deb` installs cleanly on the runner kernel, are **unmeasured**. A
  `test/container` job on such a runner would be new CI time on top of the `go` job, kept out of the
  required checks until it is stable, like the nightly stress job.
- **The real server** is the final check for any option, as Phase 1's deployment was.

### 5.2 Tests by tier (testing §3), each negative with its positive control (§4.1)

| Assertion | Tier | Positive control in the same function |
|---|---|---|
| Container root maps to an unprivileged host uid: a file created by container root on a bind mount is owned by a non-zero host uid, and a host-root `0400` canary on a bind mount is unreadable | container | The same test on a `runc` container in the same run sees uid 0 and reads the canary. Without that control, a broken mount would pass. |
| An approved `privileged` docker-in-docker workspace under Sysbox cannot read a host block device (`mknod` + read-only `dd` of the first sector, never a write) | container | Its inner `docker run hello-world` succeeds (the DinD actually works), and a `runc --privileged` control container *can* read the sector |
| The guard turns `--privileged` into `--runtime=sysbox-runc` only when policy and deployment both say so | unit (`internal/dockerguard`) | The same argv with Sysbox off passes `--privileged` unchanged, and with no approval is refused |
| An approval given under Sysbox does not cover a run without it, at step 3 (`container.Covered`) **and** at the guard (`policy.json`'s value) | unit (`internal/container`, `internal/dockerguard`) | The same value under the same mode runs without asking, and the guard swaps it |
| The rewrite touches only the option region: a `--privileged` after the image is passed unchanged; `--privileged=false` is never a runtime; an existing `--runtime` is refused | unit (`internal/dockerguard`) | `--privileged` and `--privileged=true` before the image are swapped, and the rewritten argv passes the second check |
| `start` of a Sysbox container accepts `Runtime: sysbox-runc` and refuses `Privileged: true` beside it | unit, from a recorded `inspect` | A `runc` container recorded beside it is held to today's table |
| Under an approved `"sysbox"`, the guard refuses `start` of a runc container with `Privileged: true`, `exec --privileged` into it, and a `run` whose `--privileged` it did not swap; `has`-style "not false" never approves `privileged` | unit (`internal/dockerguard`) | The same three under an approved `true` pass as today, and under `"sysbox"` a Sysbox-runtime container's `start` passes |
| Approving a request whose `privileged` value changed mode (`true → "sysbox"`) recreates the container even when the stopped run's `RemoveExisting` was `false`; and a guard refusal on that start ends `failed`, whose next start recreates | component (`internal/provision`, fake devcontainer) | An approval with the value unchanged keeps `RemoveExisting = false` and reattaches, as today |
| Clone round-trip: the agent writes, Drydock reads the lockfile, the cleanup helper empties the tree | container | Unchanged from today's tests, run on the new runtime |
| Preview dial and discovery reach the container (rootless only) | container | A port not listening is reported closed, not "unreachable" |

### 5.3 Spike 06: non-root Docker (specified, not built)

**Numbering.** The highest spike on `main` is 04. PR #139 (the VS Code extension design) proposes a
Spike 05, so this one is **06**, whatever order they merge in. If #139 drops its spike, keep 06
anyway rather than renumber a cited document.

**Harness:** `docs/design/spikes/harness-06-nonroot-docker/`, with a README naming the command for
each result (the pairing CLAUDE.md asks for). `run.sh <mode>` with modes `rootful` (the positive
control for every probe), `remap`, `remap-keepid`, `rootless-self`, `rootless-dedicated` and
`sysbox`. Each mode runs the same probes and writes `results-<mode>.txt` with one
`PASS|FAIL|N/A <probe> <detail>` line per probe, plus the versions it ran against (kernel, Docker,
RootlessKit, Sysbox, devcontainer CLI 0.89.0, Feature versions). Every container it creates carries
`drydock-spike06=<run id>` and is removed by that label on exit (`trap`). It never prunes and never
touches the host's other containers. Modes that need host changes (`rootless-*`, `sysbox`) refuse to
run unless `SPIKE06_DISPOSABLE_HOST=1`, meaning a CI runner VM or a scratch VM. **P9 needs that flag
in every mode, `rootful` included**, because it reads a raw block device. **Controls run in the same
run**: Sysbox and runc share one daemon, so the `sysbox` mode runs each probe's runc twin beside it
and records both, rather than leaning on a separate `rootful` results file (testing §4.1).

The `remap` and `remap-keepid` modes reproduce §7's measurements. Until the harness lands, those
"measured" labels point at commands run by hand, not at anything re-runnable, and the report that
lands with the harness supersedes them.

| Probe | Question |
|---|---|
| P1 | Container uid 0 maps to which host uid? Container uid `getuid()`? (`/proc/self/uid_map`) |
| P2 | Host-root `0400` canary on a bind mount: readable by container root? |
| P3 | Real `devcontainer up` of the minimal config with the clone mounted: `updateRemoteUserUID` result, agent write, host read and write, a stale lockfile rewritten by the CLI on the host, the cleanup helper's delete |
| P4 | A directory under `/run` (and under `/var/lib`) created *after* the daemon started, bind-mounted with `--mount`: visible, and a socket in it connectable from the container? |
| P5 | Credential volume: 32 concurrent `mkdir` of one lock from two containers (in `sysbox`, one Sysbox and one runc), exactly one winner per round; filesystem type, and the owner of a `0600` file each container sees (`nobody` is red) |
| P6 | Host-side `connect()` to the container's bridge IP; host-side read of `/proc/<pid>/net/tcp6` |
| P7 | `docker stats` memory non-zero; `pause`/`unpause`; `inspect --size` |
| P8 | devcontainers docker-in-docker Feature (pinned version): `up` succeeds, the inner `docker run hello-world`, and the inner storage driver on the Feature's `${devcontainerId}` `/var/lib/docker` volume. Under `sysbox`, `privileged` is removed **the way Phase B would**: the CLI's real argv, through a `record.sh dockerargv`-style shim that swaps exactly the one `--privileged` token for `--runtime=sysbox-runc`. A hand-written `docker run`, or a config override (which cannot unset a Feature's `privileged` anyway), would measure something else. An inner driver of **`vfs` is red**: it works, but its disk use is huge. Sysbox requires that volume to be mounted in one container at a time, and a rebuild must respect that. `exec --privileged` into the container: refused or not? |
| P9 | Privileged container: read the first sector of the host's root block device (read-only; `SPIKE06_DISPOSABLE_HOST=1` in every mode) |
| P10 | `docker inspect` `HostConfig` of P3's and P8's containers, saved as fixtures and fed through `dockerguard`'s start check by a Go test |
| P11 | The argv `up` produces, recorded through `record.sh dockerargv`'s shim |
| P12 | After the switch, what of the old daemon's images, volumes and containers is visible |

Run it at least in `rootful`, `remap` and `sysbox` on a disposable VM. The `rootless-*` modes are
worth running only if the owner wants rootless kept on the table after reading §6.

---

## 6. Recommendation and phasing

1. **Phase A: Spike 06** in `rootful`, `remap` and `sysbox` on a disposable `ubuntu-24.04` VM (a
   CI runner via `workflow_dispatch` is fine). This is the cheapest step by far: a script and a
   report, no product code. Gate on P8 (DinD under Sysbox without `privileged`), P3 (the clone
   unchanged) and P10 (what the guard must accept).
2. **Phase B, if A is green: Sysbox for approved `privileged`, behind a flag.** This closes H1 for
   docker-in-docker repositories, which is the hole that needs no bug and that this repository
   itself opens. Compose workspaces are outside it (§2.4). `privileged`'s approved value carries
   the mode (§3.7). The guard's substitutive rewrite is designed and documented as such (§3.6). The
   UI says what `privileged` now means. The installer detects but does not install Sysbox, and the
   runbook says what installing it does to the host (§3.10). Runbook and Ansible go in the same
   change.
3. **Phase C, after B has run for a while: Sysbox as the default runtime** for every workspace
   container. The ephemeral helpers stay pinned to `runc`: the cleanup and volume-owner helpers
   need host-root `DAC_OVERRIDE` on Drydock's own directories, and the login and identity helpers
   run the real `claude` against the credential volume. This closes H2 **for the host** for every
   workspace container, though not for those helpers. It does **not** separate workspaces from each
   other: CE's common mapping means an escape from any of them reaches all the others. Keep a
   per-repository escape back to `runc` as an *approval* (it widens host access), never a silent
   fallback.
4. **Defer rootless Docker.** Reopen it if the owner decides H3 matters: the dev server holds
   things worth more than the GitHub installation, and a compromised Drydock (a dependency, the CLI,
   a Feature tarball) should not be host root. If it is reopened, take the **dedicated-uid** form,
   since it alone keeps the keys from an escapee, and budget for the preview relay and the clone
   ownership work first.
5. **Do not pursue `userns-remap`** (it does nothing for H1, its clone fix undoes H2, and it is
   daemon-wide on the owner's host) **or Podman** (a second engine under the guard).

Independent of all of this, three cheap hardenings stand on their own and could ship first:

- **Mount the workspace root `nosuid`** (`/srv/drydock/ws`, the runbook's filesystem step). Today a
  container root writes the clone as host root, and so would a Sysbox container through its
  ID-mapped mount. `nosuid` stops a planted setuid-root file from meaning anything to a host user
  who runs it, such as the VS Code SSH user.

- The approval card's text for **`initializeCommand`** could say what §6 already says (*"runs on the
  host as Drydock, which can read its keys and is root through Docker"*). Today the card's general
  warning covers it (*"reach this server, as the drydock user or as root"*), and only `privileged`
  gets a sentence of its own (`HostAccessApproval.vue`).
- The runbook could recommend against adding the VS Code SSH user to `docker` on a server whose
  owner's account holds anything sensitive, since §6 already states that `docker` is root.

---

## 7. What was measured, and how

All in this repository's devcontainer (kernel 6.8.0, cgroup v2, Docker 29.8.2 outer), inside
throwaway `--privileged` containers labelled for removal and removed afterwards:

- **`userns-remap`:** `docker:dind` running Docker 29.9.0 with `--userns-remap=default`
  (`dockremap:165536:65536`), busybox 1.37.0. Results: the host directory appears as `65534`; neither
  container root nor uid 998 writes it. After `chown 166534`, host uid 998 can neither write nor
  delete. The ACL mask is zeroed by a container `chmod 0700`. `--privileged` is refused with the
  quoted message, and `--userns=host` gives an identity map. Bridge IP reachable, `/proc/<pid>/net/tcp6`
  readable, `stats`, `pause` and `unpause` work, as an unprivileged "host" uid. The data root is
  `/var/lib/docker/165536.165536`. A second `mkdir` of a lock on a volume fails with *File exists*.
- **Multi-range remap:** the same image with `/etc/subuid` `dockremap:100000:998`,
  `dockremap:998:1`, `dockremap:101000:64537`. The `uid_map` is as stated in §2.2. The uid-998
  clone round-trip works. Root-made files are host `100000`, and host 998 cannot delete them but a
  `DAC_OVERRIDE`+`FOWNER` helper can. A host-root `0400` file is unreadable. Container root writes
  a host-998 directory. `--privileged` is still refused.
- **Rootless:** `docker:dind-rootless` failed to start (quoted in §5.1). The host has
  `kernel.apparmor_restrict_unprivileged_userns=1`, and `unshare -Ur` fails in the devcontainer.
  The image's entrypoint passes `--net=${DOCKERD_ROOTLESS_ROOTLESSKIT_NET:-slirp4netns}`,
  `--port-driver=builtin`, `--copy-up=/etc` and `--copy-up=/run` to RootlessKit, read from the file.
- **devcontainer CLI 0.89.0** (read from `dist/spec-node/devContainersSpecCLI.js`): the uid update
  passes `NEW_UID=getuid()`. Podman is detected from `docker -v`, and then
  `--security-opt label=disable` and, for a non-root remote user, `--userns=keep-id` are added.
- **Not measured, and named as such where used:** everything about Sysbox, rootless Docker's
  behaviour beyond what failed to start, rootless Podman, `/run` copy-up semantics, the
  docker-in-docker Feature under anything but today's rootful daemon, and GitHub runner userns
  policy. Sysbox CE's common uid mapping is taken from its documentation (`security.md`), not
  measured.
- **No checked-in harness yet.** These were run by hand. Spike 06's `remap` and `remap-keepid`
  modes are specified to re-produce them (§5.3).

---

## 8. Open questions for the owner

1. **Which asset matters most?** If it is GitHub and the secrets, no option here does much: a
   compromised Drydock holds them all by design. If it is **the dev server itself** (your home
   directory, SSH keys, other services), Sysbox closes the two container routes, and only rootless
   closes the Drydock route. Which do you want to pay for?
2. **Is a third-party runtime acceptable on the dev server?** Sysbox is Docker-sponsored but
   best-effort, and it ties kernel upgrades to its releases. Which distro and kernel does the
   server run? Sysbox wants kernel ≥ 6.3 to need no shiftfs, and native Docker, not a snap. Installing
   it edits `daemon.json` and restarts dockerd once, with every workspace stopped (§3.10).
3. **Do any of your repositories ask for `privileged` for something other than docker-in-docker**
   (KVM, a device, `--net=host`), or **use Compose with `privileged`**? Sysbox refuses the first
   kind, and the guard cannot translate the second. Both would stay `runc`, with today's meaning.
4. **Sysbox for approved `privileged` only, or for every workspace?** The second turns every
   container escape from host root into an unprivileged uid, at a wider compatibility risk. But
   Sysbox CE maps **all** its containers to one common uid range (documented in its `security.md`;
   per-container ranges are Enterprise-only). So an escape from one workspace still reaches every
   other Sysbox workspace's processes and files. Is "the host is safe, the workspaces are not safe
   from each other" worth it to you?
5. **Is a one-time rebuild of every workspace and one more Claude sign-in acceptable** for an option
   that changes the daemon? Sysbox needs no new sign-in. It does need a dockerd restart with
   everything stopped, a re-approval and recreate of each DinD workspace, and, if you follow Sysbox's
   advice to remove containers first, a rebuild of every workspace. A rebuild loses whatever the
   agents left outside the clone (§3.10).
6. **Would you keep *Open in VS Code* under rootless**, knowing the attaching user would then be
   the `drydock` user, keys included (same-uid form)?
7. **May Spike 06 run on a CI runner via `workflow_dispatch`**, or only on a scratch VM you
   provide? It changes the runner's kernel settings and installs packages, which is fine on a
   disposable VM and is not something this devcontainer can do.
8. **Spike numbering:** this document claims 06 because PR #139 claims 05. Confirm, or say which of
   the two should move.
