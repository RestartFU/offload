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

- **No SSH.** Runners listen on their tailnet addresses only. Callers are identified with
  tailscaled's WhoIs, so your tailnet login is the credential. Optional bearer tokens cover
  hosts outside the tailnet.
- **Discovery.** The client probes online tailnet peers on port 7420 and keeps the ones that answer.
- **Sliced, not exclusive.** Every job asks for a guaranteed CPU share, a burst ceiling and a
  hard memory limit. Jobs are packed onto runners and queue when nothing fits. Tools see their
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
  and a warm job starts in about 0.5s. Toolchains (rustup, Go modules, npm and pip caches) live
  in shared volumes, so every environment reuses them. `--host` runs directly on the runner.
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
  (`offload host upgrade`) without killing work. Clients reattach from the last byte after
  network drops or laptop sleep.

## Install a runner

On the server, once (copy the binary any way you like, e.g. `scp`, cloud-init or a USB stick):

```bash
./offload runner install                          # ~/.local/bin/offload, config, profiles, user units
sudo ~/.local/bin/offload runner install-system   # linger, cgroup delegation, keep-alive, firewall
sudo dnf install podman   # or apt: rootless podman for container jobs (optional; without it jobs run on the host)
systemctl --user daemon-reexec && systemctl --user restart offload-runner
```

After that, upgrades come from the laptop with `offload host upgrade <name>`.

The config lives in `~/.config/offload/runner.toml` and holds capacity, reserve, storage and
policy settings. Profiles live in `~/.config/offload/profiles/*.toml`. See
[`profiles/`](profiles) for the defaults and [`profiles/extra/`](profiles/extra) for the
Minecraft MCP session.

## Use it from the laptop

```bash
CGO_ENABLED=0 go build -o ~/.local/bin/offload .   # static: the same binary runs inside job containers
offload hosts
offload run -- cargo test
offload run --profile cargo --mem 16G --get target/release/app -- cargo build --release
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
  `{"command": "offload", "args": ["mcp", "<profile>"]}`.

## Layout

```
main.go                 CLI
internal/client         discovery, host choice, run/attach, source sync, MCP bridge
internal/runner         daemon: API, scheduler, workspaces, systemd units, install
internal/hook           Claude Code guard
internal/tsapi          tailscaled LocalAPI (whois, status)
profiles/               default runner profiles (embedded)
skill/offload           agent skill
docs/design.md          design notes and roadmap
```
