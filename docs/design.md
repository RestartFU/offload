# Design notes

## Shape

- **The laptop is only a client.** Runners are servers on the tailnet. A runner on the laptop
  itself is optional; it gets priority -10 and is reached over a unix socket.
- **One binary.** `offload` is the CLI, the daemon (`runner serve`) and the unit wrappers
  (`runner exec`, `runner pipe`). Upgrading a runner means pushing the client's own binary.
- **Transport.** HTTP over the tailnet. There is no SSH anywhere.
- **Auth is a paired client token, not tailnet identity.** Runners are devices of the same
  tailnet user, and so are their jobs' network connections: a WhoIs match can't tell the laptop
  from a job on another runner. So the API wants a bearer token whose hash is in the runner's
  `tokens` file. An allowed WhoIs identity (`allow_users`, `allow_tags`) may only read
  `/v1/status`; pairing needs a paired client or a one-time code that only someone on the
  runner can read (`offload runner pair-code`; a wrong guess burns it). The local unix socket
  needs nothing. Every pairing mints a fresh random token for that one runner (stored under
  `runner-tokens/` on the client) and retires the one it authenticated with, so re-pairing after
  `offload runner reset-pairing` never revives a leaked token. Discovery probes carry no token.
  Resets go through the running daemon's lock, so a pair request in flight can't undo one.
- **Per-client names.** Data sets live on runners as `<client id>.<name>` and local-path git
  origins are keyed by the client id (`client.id`), so two laptops' same-named data sets or
  `../upstream.git` checkouts never share a directory.
- **One protocol version at a time.** Clients send `X-Offload-Proto`; runners refuse other
  versions except for status, pairing and upgrades. A runner of a new version stops workloads
  the old one started (their mounts and sockets differ) and admits nothing until it has
  confirmed that, so upgrades check the new binary's version (`offload proto`) and refuse to
  strand running work without `--force`.
- **No browsers.** Tailnet identity is ambient: a browser on an allowed machine carries it for
  every page it shows. So the API refuses any request with `Origin` or a `Sec-Fetch-Site` other
  than `none` (cross-site request forgery). Callers trusted by tailnet identity must also name
  the runner in `Host` by IP address, localhost, its hostname or tailnet name, or an entry in
  `hostnames` (DNS rebinding). Token holders may use any name; a page can't attach the token.

## Execution

- Every job and session is a transient systemd **user** service in `offload.slice`. Each one
  gets `CPUWeight` (its guaranteed share), `CPUQuota` (its burst), `AllowedCPUs` (cpuset, so
  `nproc` tells the truth), `MemoryMax`, `MemorySwapMax=0`, `IOWeight` and `OOMScoreAdjust`.
- The unit's main process is a small wrapper that runs the command. It records the exit code,
  the signal, and whether the cgroup's OOM killer fired (`memory.events`), and packs artifacts.
  Because of that, the daemon can die or upgrade while jobs keep running.
- Admission counts guarantees against capacity and also checks live host `MemAvailable`
  against a reserve. Smaller jobs may skip ahead in the queue unless the job at the head has
  waited more than two minutes.

## Source sync

The client builds a manifest: path, sha256, mode and symlink target for each file. In a git
repo that covers tracked files plus untracked files that aren't ignored. The runner assigns a
workspace slot for the repository key (origin URL hash) and replies with the paths whose
content differs. The client uploads only those as a tar.gz, and the runner verifies every hash.
Build output is ignored by git, so it never moves and stays warm. Files a previous job edited
are detected through size and mtime and sent again. There are up to `max_slots_per_key`
concurrent slots per repository.

## Routing

The agent only writes `offload run -- <cmd>`. The client keeps a runner cache (15 minutes) and
probes known runners directly; a full tailnet scan happens only when the cache is stale or none
answer. A runner whose workspace slots are busy is skipped for the next one. The runner matches
a profile from the command (`match` globs, looking through `sh -c`) and sizes memory from history.
For each (workspace, "program subcommand") it keeps the last five peaks (`memory.peak`) and asks
for the largest plus 25%. An OOM counts as twice its limit, and the client retries such a job
once with double memory. A nested `offload run` (OFFLOAD_JOB is set) runs in place.

## Git

Each workspace key has a bare `shared.git` object store. Slots are ordinary repositories
borrowing its objects through `objects/info/alternates`. The runner advertises up to 64 commits
it holds, and the client sends `git bundle create HEAD ^have…`. The slot gets HEAD/branch and
an index at HEAD (`read-tree`), so the synced working tree shows your uncommitted changes.

## Credentials

- Relays: per job, an SSH agent socket (framed request/response; only list-keys and sign
  requests pass, checked on both ends) and a git credential helper
  (`offload runner credential`, local socket only) queue requests that the attached client
  long-polls and answers with its own ssh-agent and `git credential fill`. With no client
  attached, requests fail immediately.
- Secrets: credential sets are resolved on the client (`cmd:`, `env:`, `file:`) and sent with
  the job. The runner keeps them in memory, writes them to `$XDG_RUNTIME_DIR/offload/secrets/<job>`
  (tmpfs) at start, and the wrapper exports them and deletes `env.json`. The directory is removed
  when the job ends. Job records only list secret names. A runner restart drops queued jobs'
  secrets and fails those jobs.

## Containers

Jobs run as `podman run --cgroups=split` inside their transient unit (`Delegate=yes`), so the
unit keeps owning CPU and memory limits, OOM accounting (`memory.events`/`memory.peak` read at
the unit level, since the wrapper itself is moved into a child cgroup) and the keep-alive rules.
Things measured before building this (Fedora 44, podman 5.8):

- SELinux blocks writes to bind mounts, and `:z` would relabel the repo, so containers use
  `label=disable`. They're confined no more than host jobs.
- Inside, `nproc` and `memory.max` reflect only what podman sets, so `--cpuset-cpus` (when
  cpuset is delegated; crun fails otherwise) and `--memory` mirror the unit's limits.
- Podman's default 2048-process cap is lifted (`--pids-limit=-1`).
- `-e` values land in the container's `config.json` on disk, so secrets come in through the tmpfs
  runtime-dir mount and are loaded by the mounted static `offload` binary (`runner inner`).
- Files written by non-root container users belong to subordinate IDs, so cleanup falls back to
  `podman unshare rm`.
- Cargo and `.git` store absolute paths, so the workspace, `shared.git`, the job dir, the runtime
  dir and the binary are mounted at identical paths.
- Startup overhead is about 0.17s. A warm job round trip from the laptop is about 0.5s.

Environments are Containerfiles; `FROM offload/base` is rewritten to the runner's base tag
(a hash of the embedded `envs/base.Containerfile`). Image tags are content hashes. Builds run as
their own units with `container.build_mem`, never inside a job's limit, and their log streams to
every waiting job's stderr. A build never blocks other jobs in the queue. After a build, the base
image's `/etc/offload/bootstrap.sh` runs once against the toolchain volumes (also in its own unit)
to install or repair rustup and the cargo/Go bin directories. Only the runner's own base image
ever writes them; jobs see them read-only, because one job's planted binary would otherwise run
in every later job. Dependency and build caches (cargo registry and git, Go modules and build
cache, npm's cache with its runnable `npx` trees) are writable and therefore scoped per
repository, and `CARGO_HOME` itself stays in the
container, so cargo config and credentials never carry over. The cache dir records its layout
version; an older layout is wiped once. Workspace slots
are keyed by repo plus environment base (`<key>--b<base>`), so a different base never reuses
stale `target/`, while adding tools on top of the same base keeps builds warm. Unused images are
removed after `container.image_retain`.

## Sessions (MCP)

Sessions run in a container by default too (`podman run -i`, stdin forwarded through the
`runner pipe` socket into `runner inner --stdin`). The environment comes from the client's
`--environment`, else the profile's `containerfile`, else the base. Profiles can add `devices`
and persistent `volumes`. Sessions don't queue: if the image isn't ready the runner starts the
build and answers 409, and the client retries until it is.

`runner pipe` owns the process. Stdout is appended to a file and stdin is accepted on a unix
socket. The client shim posts stdin lines with sequence numbers, which makes retries
idempotent, and streams stdout from a byte offset, which makes reconnects lossless. A session
with no attached client for `session_grace` is stopped.

## Client networking

A job or session with network "client" runs with `--network=none --device /dev/net/tun
--cap-add NET_ADMIN`. `runner inner --net-client` creates tun0 (10.233.0.2/32, default route
via netlink, since the kernel adds no prefix route for a /0 address), writes resolv.conf (nameserver
10.233.0.1, the client's search list) and runs gVisor's stack on the TUN fd with TCP and UDP
forwarders. Each new connection is `POST /v1/{jobs|sessions}/{id}/dial` on the runner's local
socket. The runner queues a "dial" callback on the relay. The client dials the destination
itself (10.233.0.1:53 maps to its resolver, 10.233.0.1:* to its localhost), opens
`POST /v1/streams/{sid}`, and answers "ok". Both legs are HTTP/1.1 upgrades, and the runner splices
them. UDP flows travel as length-prefixed frames and close after two idle minutes. IPv6 isn't
routed yet; resolvers fall back to IPv4. Sessions prefer runners that already hold their data
sets (`data_sets` in /v1/status).

## Minecraft (examples/minecraft)

The launcher fork's flatpak runs inside the container: `nested_sandbox` unmasks /proc, drops the
seccomp filter and grants SYS_ADMIN and NET_ADMIN in the container's own namespaces, and the
environment starts a system D-Bus for `flatpak run`. The updates mod's Google credentials
consent window is approved by mcpelauncher-agent itself (its `src/consent.ts`, via xdotool), so
the session needs no laptop-side help.

## Usage metrics and the ledger

The runner samples usage every 5 seconds (`metrics.go`):

- each CPU's busy share, from `/proc/stat` deltas;
- host memory;
- each live workload's `cpu.stat` `usage_usec` and `memory.current`/`memory.peak`, read from its
  unit cgroup (the slice path expanded, falling back to systemd's `ControlGroup`).

Samples are kept for an hour at full resolution and for a week as one-minute averages,
persisted to `metrics.json` every five minutes. Longer ranges are averaged into round steps of
at most 720 points. The wrapper records the unit's CPU time in the exit record, next to
`memory.peak`.

Every finished job and session is appended to `ledger.jsonl`: command signature, workspace,
caller, wait and run time, resources, peak memory, CPU time, and synced vs uploaded files.
The ledger outlives `storage.jobs.retain` and is trimmed to `storage.ledger.retain`.

`X-Offload-Caller` is set only by `auth`: the tailnet login or tags, `token`, or `local`.
Clients can't spoof it. It is recorded as the job's `caller`, separate from the free-form
`client` label.

## Dashboard

`offload dashboard` runs on the laptop. It polls every runner every 2 seconds while a page is
open (every 30 when none is) and rediscovers runners every 30 seconds. It serves a page with no
build step: plain JS and hand-drawn SVG charts, embedded in the binary.

It acts with the laptop's tailnet identity, so it guards that identity:

- it listens on loopback only, and checks `Host` against its own address (no DNS rebinding);
- every API request carries a random per-run key in an `X-Offload-Key` header, so other local
  users can't use it. The key arrives in the printed link's fragment (`#key=…`), which browsers
  never send to the server. The page keeps it in its own origin's storage. It isn't a cookie,
  because cookies are shared by every port on `127.0.0.1`, so any other local web server the
  browser visits would receive one;
- the browser is opened on a private (0600) HTML file that redirects to the link, so the key
  never appears on a command line, where `ps` would show it to other users;
- changes (cancel, stop) need an `X-Offload-Dashboard` header and a same-origin request;
- a strict CSP forbids inline script and style;
- runner data goes into the page as text nodes only.

Session logs are read with `observe=1`, so watching doesn't count as an attached client and
doesn't keep an idle session alive. Runners older than that (no usage metrics) would count the
viewer as a client and ignore `tail`, so the dashboard doesn't show their logs.

Runners are keyed by URL. Two runners that report the same name are both listed: the second
one's name gets its address added.

The ledger is append-only. Each append writes a whole line under its own lock, and starts a
fresh line if a crash left a torn one. Readers skip lines that don't parse or are over 1 MiB,
and keep the last entry per workload. That covers a job recorded twice because the runner
crashed before saving its final state. Garbage collection reads and filters without holding
the scheduler lock, then briefly takes the ledger lock to copy what was appended meanwhile and
swap the file in.

## Keeping the host alive

- One physical core (the siblings of the highest-numbered CPU) is left out of `capacity.cpus`.
- `offload.slice` gets `MemoryMax` set to capacity, `CPUWeight=50` and `ManagedOOMMemoryPressure=kill`.
- Jobs get `OOMScoreAdjust=900`. sshd and tailscaled get `-1000`, `MemoryMin=64M` and `CPUWeight=1000`.
- Port 7420 is opened in firewalld only for tailnet source ranges.

## Not built yet

- Sharing built images between runners (a tailnet registry) once there is a second runner.
- devcontainer "features".
- Kubernetes backend: a runner deployed in-cluster that creates Pods and Jobs with its own
  service account.
- Size-based workspace GC (today GC is age-based: `storage.cache.retain`).
- Fair-share quotas per client label.
- `tsnet` embedding, so a runner can join the tailnet without a host tailscaled.
