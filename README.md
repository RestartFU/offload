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
- **Survives restarts.** Jobs are systemd units, so the daemon can restart or upgrade
  (`offload host upgrade`) without killing work. Clients reattach from the last byte after
  network drops or laptop sleep.

## Install a runner

On the server, once (copy the binary any way you like, e.g. `scp`, cloud-init or a USB stick):

```bash
./offload runner install                          # ~/.local/bin/offload, config, profiles, user units
sudo ~/.local/bin/offload runner install-system   # linger, cgroup delegation, keep-alive, firewall
systemctl --user daemon-reexec && systemctl --user restart offload-runner
```

After that, upgrades come from the laptop with `offload host upgrade <name>`.

The config lives in `~/.config/offload/runner.toml` and holds capacity, reserve, storage and
policy settings. Profiles live in `~/.config/offload/profiles/*.toml`. See
[`profiles/`](profiles) for the defaults and [`profiles/extra/`](profiles/extra) for the
Minecraft MCP session.

## Use it from the laptop

```bash
go build -o ~/.local/bin/offload .
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

[credentials.sets.github]
auto = true                    # every job gets it, no flags
env = { GH_TOKEN = "cmd:gh auth token" }

[credentials.sets.npm]         # only with: offload run --with npm -- …
files = { NPM_CONFIG_USERCONFIG = "~/.npmrc" }

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
