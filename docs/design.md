# Design notes

## Shape

- **The laptop is only a client.** Runners are servers on the tailnet. A runner on the laptop
  itself is optional; it gets priority -10 and is reached over a unix socket.
- **One binary.** `offload` is the CLI, the daemon (`runner serve`) and the unit wrappers
  (`runner exec`, `runner pipe`). Upgrading a runner means pushing the client's own binary.
- **Transport.** HTTP over the tailnet. Auth uses tailscaled WhoIs against `allow_users` and
  `allow_tags`. Bearer tokens are a fallback for hosts outside the tailnet. There is no SSH
  anywhere.

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

- Relays: per job, an SSH agent socket (framed request/response) and a git credential helper
  (`offload runner credential`, local socket only) queue requests that the attached client
  long-polls and answers with its own ssh-agent and `git credential fill`. With no client
  attached, requests fail immediately.
- Secrets: credential sets are resolved on the client (`cmd:`, `env:`, `file:`) and sent with
  the job. The runner keeps them in memory, writes them to `$XDG_RUNTIME_DIR/offload/secrets/<job>`
  (tmpfs) at start, and the wrapper exports them and deletes `env.json`. The directory is removed
  when the job ends. Job records only list secret names. A runner restart drops queued jobs'
  secrets and fails those jobs.

## Sessions (MCP)

`runner pipe` owns the process. Stdout is appended to a file and stdin is accepted on a unix
socket. The client shim posts stdin lines with sequence numbers, which makes retries
idempotent, and streams stdout from a byte offset, which makes reconnects lossless. A session
with no attached client for `session_grace` is stopped.

## Keeping the host alive

- One physical core (the siblings of the highest-numbered CPU) is left out of `capacity.cpus`.
- `offload.slice` gets `MemoryMax` set to capacity, `CPUWeight=50` and `ManagedOOMMemoryPressure=kill`.
- Jobs get `OOMScoreAdjust=900`. sshd and tailscaled get `-1000`, `MemoryMin=64M` and `CPUWeight=1000`.
- Port 7420 is opened in firewalld only for tailnet source ranges.

## Not built yet

- Container runtime for profiles (`image = "…"` run through podman with `--cgroups=split`).
  Today, profiles run on the host with the host's toolchains.
- Kubernetes backend: a runner deployed in-cluster that creates Pods and Jobs with its own
  service account.
- Size-based workspace GC (today GC is age-based: `storage.cache.retain`).
- Fair-share quotas per client label.
- `tsnet` embedding, so a runner can join the tailnet without a host tailscaled.
