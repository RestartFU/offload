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
  against a reserve. Past capacity, CPU guarantees may add up to `cpu_overcommit` (default 2)
  times it, but such a job starts only while the last 5s sample shows its guarantee's worth of
  idle cores (less guarantees of work started since that sample). The guarantee stays its
  `CPUWeight`, so under contention every job still gets its share. Runners report jobs still uploading or queued (`pending`) so routing counts them,
  and an attached `offload run` moves a job queued for 10s to a runner that can start it now
  (`DELETE /v1/jobs/{id}?moved=1` cancels it only while it is still queued). Smaller jobs may skip ahead in the queue unless the job at the head has
  waited more than two minutes.
- Memory is booked, not reserved by limit: a job counts the largest peak of its command's
  history (its whole limit without history), sessions and environment builds their whole
  limit. Agents ask for far more than they use and learned limits carry 25% headroom, so
  booking limits left most of a runner's memory idle while jobs queued. Limits may add up to
  `mem_overcommit` (default 2) times capacity. The host check subtracts what running jobs
  have yet to grow into, from the last sample to their bookings, because `MemAvailable` can't
  show it.
- Overbooked memory fails differently from CPU: when bets lose, `offload.slice` reaches its
  `MemoryMax` and the kernel kills a job, never the host. That kill is the runner's fault, so
  it must not count as the job's OOM, which doubles its learned limit and its retry.
  `memory.events` tells them apart: `oom_kill` counts kills in the cgroup for any reason, but
  `oom` counts only limits hit by the cgroup itself or below it, never by an ancestor. A kill
  without `oom` fails as "host memory pressure", which clients retry once at the same size.

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

That grace alone doesn't free much: MCP clients start every configured server when a
conversation opens and keep it until the conversation closes, so an attached client says
nothing about use. The client shim therefore stops its own session once every request has been
answered and none arrived for `--idle` (15 minutes). The next request starts a new session,
which first gets the MCP client's original `initialize` (under a reserved id whose answer the
shim swallows) and `notifications/initialized`. Pings are answered locally while stopped, so a
keepalive doesn't wake it. Server state, such as a running game, doesn't survive this.

## GitHub Actions

- **Scale sets, one per runner.** Their names carry the runner's ID, because a removal deletes
  the scale set: two runners of the same name must never share one. GitHub's runner scale sets
  hand out jobs to a listener that
  says how many it can take (`maxCapacity` on each poll). Each runner registers its own scale set
  per owner with the same labels and advertises what fits there now. GitHub then routes across
  runners itself: no runner knows another, there is no leader, and a dead runner only takes its
  own share offline. This was checked before building it: two scale sets sharing a label never
  got more jobs than they advertised, and the rest waited on GitHub.
- **A scale set's labels are fixed.** GitHub answers an update to them with success and keeps
  the old ones (checked with System and User labels alike), so new labels mean deleting the
  scale set and making a new one under the same name. Deleting it under working runners would
  strand their jobs, so the runner keeps listening on the old one, claiming nothing new and still
  starting runners for jobs already assigned to it, and replaces it once nothing is assigned or
  running there.
- **A one-job runner per assigned job.** For every job GitHub assigns, the runner generates a
  just-in-time runner config and queues an offload job running the actions runner with it. The
  config is a credential, so it lives only in the job's secrets. When GitHub assigns fewer jobs
  than there are runners still waiting for one, the extra runners are stopped and removed on
  GitHub. Runners that end without ever taking a job pause the pool, so a broken setup can't spin.
- **Capacity counts the runner's real room** (CPU with overbooking, memory room, queued work)
  in units of one GitHub job, booked at its learned peak. Several pools on one runner each see
  the same room; the few jobs that over-assigns wait in offload's queue like any job.
- **The token is stored on the runner,** in the pool's record (one atomic 0600 write, so settings
  and token never disagree after a crash), unlike job secrets: the listener needs it for as long
  as the pool exists. It defaults to the client's gh login because that is the setup with no
  extra steps; a fine-grained token with only the runners permission is the narrow option.
- **Shared, read-only actions runner.** The runner installs the release itself (checked daily)
  and mounts it into jobs as an overlay, so a job can't plant anything in the next one's runner.
- **Warm state only within one trust domain.** A runner is started before GitHub picks its job,
  so its mounts can't depend on the repository. Writable state (work directory with checkouts and
  tool cache, the inner image store, dependency caches, a rustup layer) therefore persists only
  where every job is the same trust domain: a repository's pool, or an organization added with
  `--shared-cache`. There it lives in slots, one job at a time, and its dependency caches in
  `cache/github/<owner>`, a tree no client's workspace key can name; otherwise each job gets its
  own, deleted when it ends. The slot and paths come from the runner's own job record: a client's job
  can't name a GitHub pool.
- **A removal finishes only once GitHub confirms it** (or the scale set is already gone), so a
  timeout never leaves a scale set nobody knows about. A token that no longer works would block
  it forever, so `rm --forget` is the way out.
- **Nested podman, not a docker daemon.** Inner containers share the job container's
  namespaces and cgroup, so offload's limits cover them and nothing needs real privileges. The
  job container gets the `nested_sandbox` permissions, as flatpak sessions do, plus SYS_PTRACE:
  `docker exec` opens the inner container's namespaces, and when its PID 1 has dropped root
  (postgres) that takes ptrace access. Each job has its own PID namespace, so it reaches only
  the job's own processes.

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

The runner samples CPU and memory every 5 seconds, per CPU and per workload unit cgroup. Offload's
CPU total comes from the slice's own counter, because jobs shorter than a sample never show up
in two unit readings. It
keeps an hour at full resolution and a week of one-minute averages in `metrics.json`. The
sampler snapshots the scheduler's state under `s.mu`, then reads `/proc` and cgroups without
it. `finishLocked` takes the metrics lock while holding `s.mu`, so the sampler must never wait on
I/O while holding either lock.

A workload's CPU time and peak memory come from its wrapper's exit record, read just before the
cgroup goes away. The last sample is only a fallback: a short workload can start and end
between two samples.

`ledger.jsonl` keeps one line per finished job or session. It outlives `storage.jobs.retain`,
so statistics can cover weeks. It is append-only, and appends hold a ledger lock that is a leaf
in the lock order. A crash between the append and saving the job's terminal state finishes the
job again after the restart, with the restart's time, so readers and GC keep the *first*
entry for each ID, even when it falls outside the range asked for. A torn last line from a crash would swallow the next append, so an append starts a
fresh line when the file doesn't end in one. GC runs off the scheduler loop.

The job's `caller` comes from how `auth` admitted the request: `local` (the socket), `token`
(a paired client), or the tailnet identity. It is separate from the free-form `client` label,
which clients set themselves.

## Dashboard

`offload dashboard` runs on the laptop, talks to runners with the laptop's own credentials, and
serves a page with no build step (plain JS and SVG, embedded in the binary).

Because it acts with those credentials, it guards them:

- It listens on loopback only and checks `Host` (no DNS rebinding).
- Every API request carries a per-run key in an `X-Offload-Key` header. The key isn't a cookie,
  because cookies are shared by every port on `127.0.0.1`: any other local web server the
  browser visited would receive it. The printed link carries the key in its fragment
  (`#key=…`), which browsers never send to a server, and the page keeps it in its own origin's
  storage.
- The browser is opened on a private (0600) HTML file that redirects to the link, never on the
  link itself: command lines are visible to other users in `ps`.
- Changes (cancel, stop) also need an `X-Offload-Dashboard` header and a same-origin request.
- A strict CSP forbids inline script and style, and runner data goes into the page as text
  nodes only.

The dashboard reads session output through `/v1/sessions/{id}/observe/{stream}`, which doesn't
count as an attached client. The client streams count as attached, which keeps an idle session
alive. It is a separate route rather than a flag so that an older runner refuses the request
instead of treating the viewer as a client.

Runner follows end every few minutes, so the dashboard reconnects from its offset like the CLI
does. Runners are keyed by their status ID, so one reachable at two addresses (pinned and
discovered) is counted once. A second runner reporting a taken name is shown with its address
added, because names identify runners in the page's requests.

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
