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
offload run -- cargo build --release                 # best runner, defaults
offload run --profile cargo -- cargo test -p foo     # profile = sensible limits + env
offload run --mem 16G --cpu 8 -- cargo build        # ask for more (guaranteed cpu, hard mem)
offload run --get target/release/app -- cargo build --release   # bring a file/dir back
offload run --on vps -- make -j                      # a specific runner
offload run --no-sync -- nproc                       # no source needed
```

- Exit code = the remote command's exit code. Out-of-memory exits 137 with
  `job failed: out of memory (limit …)` — rerun with a larger `--mem`.
- Long jobs: run the Bash call in the background, or `offload run -d -- …` and later
  `offload attach <runner:id>`. Ctrl-C / killing `offload run` cancels the remote job;
  Ctrl-C on `attach` only detaches.
- `--fresh` = cold build (empty workspace); `--ephemeral` = don't keep the workspace.
- Tools see the job's real core count (`nproc`, `CARGO_BUILD_JOBS`, `GOMAXPROCS`, `MAKEFLAGS`).

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

## Rules

- Prefer `offload run` for anything that compiles, links, runs test suites or burns CPU for
  more than a few seconds. A PreToolUse hook may deny such commands locally and point here.
- If a command truly has to run locally (it needs local hardware or files outside the repo),
  prefix it with `OFFLOAD_GUARD=off`.
- Files outside the repository are not synced. Ignored files (`.gitignore`) are not synced
  either; pass inputs explicitly or add them to the repo.
- Results written on the runner stay there unless you pass `--get PATH`.
