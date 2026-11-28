# offload

Run heavy work (builds, tests, headless game clients, MCP servers) on runner machines in your
tailnet instead of your laptop. One Go binary is both the CLI and the runner daemon.

```
laptop                                       runner (any Linux box on the tailnet)
offload run -- cargo build   ── HTTP over ─▶ offload runner serve
offload mcp minecraft-bedrock   tailscale    ├─ jobs: systemd user units in offload.slice
offload hosts / ps / attach                  ├─ sessions: long-lived stdio processes (MCP)
                                             └─ workspaces: persistent per-repo slots
```

- **No SSH.** Runners listen on their tailnet addresses only. The laptop pairs with each runner
  once (`offload host pair NAME --code C`) and then authenticates with a token of its own for
  that runner; tailnet identity alone only discovers runners.
- **Discovery.** The client probes online tailnet peers on port 7420 and keeps the ones that answer.
- **Sliced, not exclusive.** Every job asks for a guaranteed CPU share, a burst ceiling and a
  hard memory limit. Jobs are packed onto runners and queue when nothing fits. CPU guarantees
  are overbooked up to 2x a runner's cores (`[policy] cpu_overcommit`) while the cores are
  really idle, since most jobs use far less than they reserve. Memory is booked at the most a
  command used in its last runs rather than its limit, so limits may add up to 2x the memory
  capacity (`[policy] mem_overcommit`). A job the kernel kills because the runner ran out (not
  for its own limit) is retried once at the same size. A queued job moves to another runner
  that can start it now. Tools see their
  real core count through cpusets plus `CARGO_BUILD_JOBS`, `GOMAXPROCS`, `MAKEFLAGS` and similar.
- **Warm by default.** Each repository gets persistent workspace slots on the runner. Only
  changed files are uploaded (content hashes), and ignored build output such as `target/` stays
  in place between runs. Set `persistent = false` to make everything ephemeral.
- **Keeps the server alive.** One core is never handed out, and the slice has a hard memory cap.
  Admission refuses work that would push host `MemAvailable` below a reserve. Jobs run with
  `OOMScoreAdjust=900` under systemd-oomd, while sshd and tailscaled get `-1000`.
- **Automatic routing.** Agents type `offload run -- <cmd>` and nothing else. The client
  picks the runner (warm workspace, free capacity, load, cached discovery), the runner picks the
  profile from the command, and the memory limit comes from earlier runs. An out-of-memory kill
  on a guessed limit is retried once with twice the memory.
- **Shards.** `offload run --shards 4 -- cargo nextest run --partition count:{shard}/{shards}`
  runs the command four times, each a job of its own spread over runners, with `{shard}` and
  `{shards}` (and `OFFLOAD_SHARD`, `OFFLOAD_SHARDS` in the job) set per shard, counting from 1.
  Output lines are prefixed with the shard, and the exit code is the first failed shard's.
  Shards write no edits back, and their `--get` outputs stay on the runner: each shard prints
  the `offload get` that fetches them. A shard whose source differs from the rest (a file
  changed while they started) fails instead of testing other code. Each shard builds the code
  again, so it pays off for suites that take far longer to run than to build.
- **Feels local.** The runner gets a real `.git` at your HEAD (only missing commits travel, as a
  bundle). Files the job edits are written back. stdout and stderr stay separate.
- **Credentials without copying keys.** The job's `SSH_AUTH_SOCK` and git credential helper
  are answered by the attached client from the laptop. Credential sets in `client.toml` (for
  example `GH_TOKEN = "cmd:gh auth token"`) can be marked `auto` so every job gets them. They
  live in tmpfs only for the job's lifetime.
- **Containers, with tools that persist.** Jobs run in rootless podman inside their systemd unit,
  so all the limits above still apply. Agents are root inside and can install anything for one
  run. Tools worth keeping go in the environment: `offload env add protobuf-compiler` maintains
  a sorted package layer in `.offload/Containerfile` (`FROM offload/base`, Ubuntu 24.04 LTS).
  `.devcontainer/devcontainer.json` (image or build, containerEnv) works too. Every runner builds
  the same definition, layer-cached: the first build takes minutes, adding a tool takes seconds,
  and a warm job starts in about 0.5s. Toolchains (rustup, cargo and Go binaries) live in
  shared read-only volumes and dependency caches are kept per repository, so every environment
  reuses them. `--host` runs directly on the runner.
- **Sessions too.** `offload mcp <profile>` runs the MCP server in the profile's environment
  (`containerfile`, `devices` such as `/dev/dri`, persistent `volumes`), or the client's
  `--environment NAME`, or on the host with `runtime = "host"`. The environment is built on first
  use while the client waits.
- **Client-defined sessions and data sets.** `~/.config/offload/sessions/NAME.toml` describes a
  whole MCP server: environment, command, data sets, persistent volumes, resources. Data sets
  (`[data.NAME]` in `client.toml`, with include/exclude/newest rules) are synced incrementally
  before each start. Files the runner changed are kept unless they also changed locally. Nothing
  is set up on the runner by hand. See `examples/minecraft/`.
- **As if on the laptop.** `--network client` (jobs) or `network = "client"` (sessions) gives
  the container its own network namespace with only a TUN device. A user-space TCP/IP stack
  (gVisor) inside hands every TCP connection and UDP flow to the attached client, which opens it
  from the laptop. The job gets the laptop's egress IP, DNS (including short tailnet names), LAN
  and tailnet, and `laptop` / `10.233.0.1` is the laptop's own `localhost`. It's opt-in, because
  every byte crosses the laptop and stops when it sleeps.
- **Any Linux runner.** The environment is the same on every distro. Runners report OS/arch,
  and the client prefers one matching the laptop (`--platform linux/arm64` forces one).
  Windows hosts can be runners through WSL2; macOS hosts through a Linux VM.
- **Survives restarts.** Jobs are systemd units, so the daemon can restart or upgrade
  (`offload host upgrade`) without killing work, unless the upgrade changes the protocol
  version (then it waits for idle, or `--force` stops the old workloads). Clients reattach from the last byte after
  network drops or laptop sleep.

## Install a runner

On the server, once (copy the binary any way you like, e.g. `scp`, cloud-init or a USB stick):

```bash
./offload runner install                          # ~/.local/bin/offload, config, profiles, user units
sudo ~/.local/bin/offload runner install-system   # linger, cgroup delegation, keep-alive, firewall
sudo dnf install podman   # or apt: rootless podman for container jobs (optional; without it jobs run on the host)
systemctl --user daemon-reexec && systemctl --user restart offload-runner
```

`runner install` ends by printing a one-time pairing code (valid a day; `offload runner
pair-code` prints a new one). On the laptop: `offload host pair <name> --code <code>`. To revoke every
paired client (a lost laptop, a leaked token), run `offload runner reset-pairing` on the runner
and pair again; every pairing mints a new token. A detached job's (`-d`) edits are written back
when you `offload attach` it. A data set whose local copy is empty is never synced (that would
empty the runner's copy) unless its `[data.NAME]` sets `allow_empty = true`. After that, upgrades come from the laptop with
`offload host upgrade <name>`.

The config lives in `~/.config/offload/runner.toml` and holds capacity, reserve, storage and
policy settings. Profiles live in `~/.config/offload/profiles/*.toml`. See
[`profiles/`](profiles) for the defaults and [`profiles/extra/`](profiles/extra) for the
Minecraft MCP session.

## Use it from the laptop

```bash
CGO_ENABLED=0 go build -o ~/.local/bin/offload .   # static: the same binary runs inside job containers
offload hosts
offload run -- cargo test
offload run --profile cargo --get target/release/app -- cargo build --release
offload mcp echo            # stdio MCP bridge to a runner session
```

Optional `~/.config/offload/client.toml`:

```toml
discover = true   # probe tailnet peers (default)
local = true      # use a runner on this machine too, at low priority (default)

[credentials]
relay = ["ssh-agent", "git"]   # default; answered from this machine while attached
git_hosts = []                 # restrict git credential lending, e.g. ["github.com"]
git_config = ["url.*", "user.name", "user.email"]   # default; this machine's git settings in jobs

[credentials.sets.github]
auto = true                    # every job gets it, no flags
env = { GH_TOKEN = "cmd:gh auth token" }

[credentials.sets.npm]         # only with: offload run --with npm -- …
files = { NPM_CONFIG_USERCONFIG = "~/.npmrc" }

# Secrets are NAME = source: "cmd:<shell>", "env:<VAR>", "file:<path>" or a literal. Besides
# sets, they can come from flags (--with SET, --secret NAME[=SOURCE]) or from the environment of
# offload itself, which is how an MCP server entry configures them:
#   "env": { "OFFLOAD_SECRET_API_KEY": "cmd:pass show api", "OFFLOAD_SECRET_FILE_KUBECONFIG": "~/.kube/config", "OFFLOAD_WITH": "github" }

[[host]]          # pin a runner explicitly, e.g. outside the tailnet
name = "box"
url = "http://box.example:7420"
token = "…"
priority = 10
```

Unpaired tailnet callers may only discover a runner (and pair the first client), and only when
the URL names it by IP address, its hostname or tailnet name. Add any other DNS name to `hostnames` in the runner's `runner.toml`.
The runner API refuses requests made by web pages.

## GitHub Actions

```bash
offload github add ZenoMCPE          # or ORG/REPO; every runner that has podman
offload github ls
offload github rm ZenoMCPE
```

Workflows then use `runs-on: offload`. The runners also carry the labels GitHub gives every
self-hosted runner (`self-hosted`, `linux` and the architecture, such as `x64`), so workflows
written for ordinary self-hosted runners match too. A connection made before these defaults
existed keeps its own labels until you add it again with `--system-labels`;
`--system-labels=false` leaves them off a new one. The dashboard's add form lists every label,
GitHub's defaults included, and the runners get exactly what is in it. `add` uses your GitHub CLI login: GitHub wants the
`admin:org` scope to manage an organization's runners (`repo` for a repository), and `add` says
how to grant it when it's missing (`gh auth refresh -h github.com -s admin:org`). For a narrower
credential, pass `--token` a fine-grained token with only "Self-hosted runners: write", as a
value or a `cmd:`, `env:` or `file:` source.

Each runner keeps its own runner scale set on GitHub (`offload-<runner>-<id>`) and tells GitHub how
many jobs fit there right now. Scale sets share labels, so GitHub hands each job to a runner with
room and holds it while none has any. Every job gets a one-job runner as an ordinary offload job:
admission, CPU and memory limits (the runner's default CPU guarantee and 8G unless `--cpu`,
`--cpu-max` or `--mem` say otherwise), memory booked from earlier runs, the ledger and the
dashboard's GitHub tab. `--max N` caps how many run at once on each runner, `--labels` sets what
`runs-on` matches, and `--on` picks runners. Adding it again, to give it a new token for example,
keeps the settings you don't give. New labels take effect on a runner once the jobs running there
finish, because GitHub can't relabel a scale set in use.

The container is new for every job. Docker works through podman inside it (`docker build`,
`run`, `login`, `push`; not `buildx` or `compose`), so `docker/setup-buildx-action` and
`docker/build-push-action` fail there: build with `docker build` and push with `docker push`.
Short image names without an alias, such as `golang:1.26`, default to Docker Hub.
`gh` and `skopeo` are installed. A repository's jobs keep checkouts, the `actions/setup-*` tool cache, Go,
cargo and npm caches and the inner image store warm between runs, each parallel job in a warm
copy of its own. An organization's jobs start clean every time,
because its repositories would otherwise share what one leaves for the next; `--shared-cache`
keeps them warm when they all trust each other. It is chosen when the organization is added
(`rm` it to change that).

`github rm` stops the jobs and deletes the scale sets on GitHub. If GitHub can't be reached or the
token no longer works, it keeps retrying and says so; `--forget` removes it locally anyway (then
delete the scale set under Settings → Actions → Runners).

The token stays on each runner (mode 0600 in its state directory) so it can keep talking to
GitHub. Workflows can't read it; anything running directly on the runner host as its user,
such as an `offload run --host` job, could. A workflow can read its own job's runner
configuration, as on any self-hosted runner, so don't run pull requests from forks of public
repositories here.

## Dashboard

```bash
offload dashboard            # opens http://127.0.0.1:7421 in the browser
```

A local web page covering every runner:

- **Overview.** Free and busy cores, and memory across all runners. CPU and memory over the
  last hour to 7 days. Each runner's CPUs as a grid shaded by how busy they are, with the cores
  pinned to jobs and the ones kept for the system marked. Jobs waiting and why. Warnings for
  runners that are down, short on memory or disk, or on another version. The pause button on
  a runner's card makes it refuse new jobs and sessions until you resume it; work it already
  took, queued jobs included, finishes. A pause survives runner restarts.
- **Containers.** Every job and session with live CPU and memory against its guarantee and
  limit. Open one to follow its logs, see its details, or cancel it.
- **GitHub.** GitHub Actions jobs running on the runners, with links to their runs, and each
  connected organization or repository: which runners take its jobs, how many more fit, and the
  `runs-on` line to copy. Add an organization or repository there (with this machine's gh login
  or a pasted token) and remove one, as `offload github add` and `rm` do.
- **Activity.** Statistics on finished jobs: success rate, out-of-memory kills, wait times,
  CPU time, warm-file rate, and per command its run times and how much of its guaranteed CPU it
  really uses.

It follows the system's light or dark setting; the switch at the top right picks one instead,
remembered by the browser.

The dashboard listens on loopback only and talks to runners with this machine's identity. The
link it prints carries a per-run key, so other local users can't use it. Runners sample usage
every 5 seconds and keep a week of it. They also keep a ledger of finished jobs
(`storage.ledger.retain`, 30 days by default). A runner from before this version shows its
status only; `offload host upgrade` brings it up to date.

## Agents

- **Skill:** [`skill/offload/SKILL.md`](skill/offload/SKILL.md). Link it into `~/.claude/skills/`.
- **Guard hook:** `offload hook claude` is a PreToolUse hook. It denies heavy local Bash
  commands (cargo/go/make/… builds and tests) and points the agent at `offload run`. Add it to
  `~/.claude/settings.json`:

  ```json
  { "hooks": { "PreToolUse": [ { "matcher": "Bash", "hooks": [ { "type": "command", "command": "offload hook claude" } ] } ] } }
  ```

  Set `OFFLOAD_GUARD=off` (env, or as a command prefix) to let a command run locally.
- **MCP:** any `kind = "session"` profile works as a stdio MCP server:
  `{"command": "offload", "args": ["mcp", "<profile>"]}`. The session is stopped after 15
  minutes without requests and started again on the next one (its state starts over); set
  `"--idle", "1h"` to change that, or `"--idle", "0"` to keep it for the client's lifetime.

## Layout

```
main.go                 CLI
internal/client         discovery, host choice, run/attach, source sync, MCP bridge
internal/runner         daemon: API, scheduler, workspaces, systemd units, install
internal/hook           Claude Code guard
internal/dashboard      offload dashboard: local web UI (embedded, no build step)
internal/tsapi          tailscaled LocalAPI (whois, status)
profiles/               default runner profiles (embedded)
skill/offload           agent skill
docs/design.md          design notes and roadmap
```
