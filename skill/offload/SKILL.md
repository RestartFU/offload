---
name: offload
description: Run builds, tests, headless clients and other CPU/memory-heavy commands on offload runner machines instead of this computer. Use for cargo/go/make/npm builds and tests, benchmarks, long scripts, and runner-backed MCP servers (offload mcp). Not for quick commands like ls, git, grep or formatting.
---

# offload

`offload` sends work to runner machines on the tailnet. The current directory is synced
(tracked + untracked-but-not-ignored files; build output stays warm on the runner), the command
runs there under CPU/memory limits, and output streams back with the command's exit code.

## Run something

```bash
offload run -- cargo test                     # that's it
offload run -- ./scripts/ci.sh                # whole scripts work too
offload run --get target/release/app -- cargo build --release   # bring a file/dir back
```

You never choose where or how big. Offload picks the runner (warm workspace, free capacity,
load), the profile (from the command: cargo, go, …), and the memory limit (learned from earlier
runs of the same command). If that guess runs out of memory, the job is retried once with twice
the memory. Don't pass `--mem` unless a job ran out of memory even after that retry: an explicit
limit replaces the learned one. `--cpu 8` and `--on NAME` are there when you know better.

What the job sees on the runner:
- The repository as it is here: tracked files plus untracked files that aren't ignored, including
  uncommitted edits. Build output such as `target/` stays warm on the runner between runs.
- A real `.git` at your HEAD and branch (log, describe, diff all work). Only missing commits are sent.
- Your credentials, without copying keys: `SSH_AUTH_SOCK` and git's credential helper are answered
  by this machine while you're attached (each use is announced on stderr), plus any credential
  sets the user marked `auto` in `client.toml`. Detached jobs (`-d`) get no relay answers.
- Real core counts (`nproc`, `CARGO_BUILD_JOBS`, `GOMAXPROCS`, `MAKEFLAGS`).

Where it runs: a container built from the repo's environment (`.offload/Containerfile`,
else `.devcontainer/devcontainer.json`, else the runner's Ubuntu 24.04 base with git, build tools,
clang, cmake, Python, Node, Go and Rust). You are root in it.
- Missing a tool for one run: install it in the command (`offload run -- sh -c 'apt-get update && apt-get install -y foo && …'`).
- Needed again: `offload env add foo` (adds an apt package to `.offload/Containerfile`; commit it),
  or `offload env add --run 'curl … | sh'` for anything else. The next job builds that layer once
  per runner, in seconds. `offload env show` prints the environment.
- Never `sudo` on the runner host and never `--host` just to get a tool.

What comes back:
- stdout and stderr (kept separate) and the exit code. Out of memory after the retry exits 137.
- Synced files the job edited (`cargo fmt`, `Cargo.lock`, codegen) are written back here, unless
  they also changed locally during the job (then they're reported, not overwritten).
- Anything else only with `--get PATH`.

Needs the laptop's network (a service on its localhost, its LAN, its IP)? Add `--network client`:
the job's connections then go out from this machine, and `laptop:PORT` reaches its localhost.

A long test suite whose tool can take one slice of it can run as shards, each a job of its own,
spread over runners: `{shard}` and `{shards}` in the command (and `$OFFLOAD_SHARD`,
`$OFFLOAD_SHARDS` in the job) are replaced per shard, counting from 1.

```bash
offload run --shards 4 -- cargo nextest run --partition count:{shard}/{shards}
offload run --shards 3 -- npx jest --shard={shard}/{shards}
```

Output lines are prefixed `[2/4]`, and the exit code is the first failed shard's. Every shard
builds the code again, so shard only suites where running the tests takes far longer than
building them. Shards write nothing back; `--get` outputs land in `shards/<i>/`.

Long jobs: run the Bash call in the background, or `offload run -d -- …` and later
`offload attach <runner:id>` (several IDs follow them all). Killing `offload run` cancels the job; Ctrl-C on `attach` detaches.
A script that calls `offload run` inside a job runs that command in place.

## Look around

```bash
offload hosts        # runners, capacity, load, profiles
offload ps           # running/queued jobs and sessions everywhere (--all for finished)
offload cancel ID    # stop a job or session
offload get ID DIR   # fetch a finished job's --get artifacts again
```

## MCP servers on runners

A runner profile of kind `session` can be used as a stdio MCP server:

```json
{ "mcpServers": { "minecraft": { "command": "offload", "args": ["mcp", "minecraft-bedrock"] } } }
```

After 15 minutes without a tool call the runner session is stopped to free the runner; the next
call starts a fresh server, so state such as a running game client has to be set up again.

## Rules

- Prefer `offload run` for anything that compiles, links, runs test suites or burns CPU for
  more than a few seconds. A PreToolUse hook may deny such commands locally and point here.
- If a command truly has to run locally (it needs local hardware or files outside the repo),
  prefix it with `OFFLOAD_GUARD=off`.
- Files outside the repository are not synced. Ignored files (`.gitignore`) are not synced
  either; add inputs to the repo or generate them in the job.
- Tools must exist on the runner (`offload hosts` lists runners and profiles).
