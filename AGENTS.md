# offload

offload runs heavy work (builds, tests, headless clients, MCP servers) on runner machines in the
developer's tailnet. One Go binary is the CLI, the runner daemon and the systemd unit wrappers.
[README.md](README.md) covers what it does and [docs/design.md](docs/design.md) covers why it
works that way. Read the design notes before changing scheduling, sync, git, credentials,
containers or networking.

## How to work here

Keep the system simple. Don't keep complexity just because it already exists, and don't add
machinery because it looks impressive. Find the real constraint, then build the smallest model
that makes the right behavior obvious. Measure twice, cut once, and remember YAGNI. Push back
on scope creep. Meet the developer's intent with the smallest realistic change.

These are good defaults, not hard rules. The developer's preferences override anything here.
If a rule fights the task in front of you, say so plainly and get a human's sign-off before you
break it.

**Terminology:** **you** are the agent reading this. **The developer** is the person you work
for. **Client** is the laptop side (`internal/client`). **Runner** is the daemon
(`offload runner serve`, `internal/runner`). **Job** is one `offload run`. **Session** is a
long-lived stdio process, usually an MCP server. **Workspace slot** is a runner's persistent
checkout for one repository.

## Three ways to hurt the developer

The machine you work on is probably the developer's own laptop or runner, and other agents'
jobs may be running on it through offload.

1. **Killing by pattern.** Never `pkill -f`, `pgrep | kill`, or `kill` a PID you found by
   matching a name or path. Never `systemctl --user stop offload-*`, and never
   `podman rm -f` containers named `offload-*`. Those are other people's jobs. Kill only a
   PID you captured when you started it, or cancel your own job with `offload cancel ID`.
2. **Touching the live install.** `~/.config/offload/`, `~/.local/share/offload/`,
   `$XDG_RUNTIME_DIR/offload*`, the `offload-runner` user unit and `~/.local/bin/offload`
   belong to the developer's real setup. Reading them is fine. Don't edit, reinstall, restart
   or clean them up, and don't run `offload runner install`, `install-system` or
   `offload host upgrade` unless asked.
3. **A throwaway runner next to a real one.** At startup a runner removes every `offload-*`
   podman container that its own state doesn't know about, and every runner uses the
   `offload.slice` cgroup and the admin socket by default. To test the daemon, give it its own
   config with `listen = ["127.0.0.1:<free port>"]` (no `unix`, no `tailscale`), its own
   `state_dir` and `profiles_dir` in a temp directory, `slice = "offload-dev.slice"`, and
   `[container] runtime = "host"` unless you are sure no real runner uses podman on this
   machine. Reach it with a pinned `[[host]]` and a token, never through discovery.

## Touch every side

The most common defect is a change that works on the path you tested and is missing
everywhere else. Before you call a change done, walk this list and say which entries applied:

- **Both ends of the wire.** Request and response types live in `internal/api`. A change
  there needs the client and the runner updated together. If an older peer would misread the
  new shape, bump `api.Proto`.
- **Jobs and sessions.** They share most of their code paths (units, containers, credentials,
  data, `--network client`) but not all of them. Decide for each one.
- **Runtimes.** Container (rootless podman), host (`--host`, `runtime = "host"`), and no podman
  at all.
- **Callers.** Tailnet identity (WhoIs), bearer token, and the local unix socket. Plus nested
  `offload run` inside a job (`OFFLOAD_JOB`), which runs in place.
- **Lifecycle.** Attach, detach (`-d`), reattach after a network drop, cancel, and a runner
  restart or upgrade while the job runs. Every way in needs a way out. A one-way door is a bug.
- **Platforms.** The binary must stay static (`CGO_ENABLED=0`) because the same file runs
  inside job containers and is pushed to runners by `offload host upgrade`. Keep Linux-only calls in the client behind build tags, as `inode_linux.go` and
  `inode_other.go` do.
- **Agent-facing text.** If agents will use the change, update
  [skill/offload/SKILL.md](skill/offload/SKILL.md) and the guard hook (`internal/hook`) as
  well as the README.
- **Docs.** Check whether the change makes existing guidance wrong. See
  [Documentation](#documentation).

## Verifying

- Run `go vet ./...` and `go test ./...` before you push. Iterate with
  `go test ./internal/<pkg> -run <Name>`.
- If the offload guard hook denies a heavy local command, run it through
  `offload run -- go test ./...` instead. Don't bypass it with `OFFLOAD_GUARD=off` unless
  the developer agrees.
- Every bug fix comes with a test that fails without the fix, where that is practical.
- For changes that unit tests can't reach (systemd units, podman, the network stack, WhoIs),
  run a throwaway runner as described above and drive it with the CLI or curl. In the PR, say
  what you could not test, such as a real tailnet runner.
- Tests don't sleep to wait for async work. Wait on the condition or the channel.

## Pull requests

- Never open a PR unless the developer asks for one. One request is one PR.
- Branch from an up-to-date `origin/master`. Commit only the files your change touched.
- Title: plain language naming the area and the visible effect, such as
  `Runner: refuse requests from web pages and foreign Host names`.
- Body: the problem in a sentence or two, then the fix, then **Testing** with what you ran and
  what you didn't test. End with the model and harness that did the work.
- When babysitting a PR, verify each review bot finding against the source. Fix the real ones
  and dismiss false positives with a written reason.

### Codex review

- Every change reaches `master` through a PR whose final head has a clean Codex review: no
  P0, P1 or P2 findings. Only trivial changes, such as docs, comments or formatting, may skip it.
- The review is repeated whenever the PR head changes. A later round reads the whole diff but
  blocks only on code changed since the last reviewed head (not merged in from `master`), or
  on earlier code those changes make reachable.
- Severity bar: P0 to P2 need an input or sequence that real developers, agents, jobs or
  runners plausibly produce, or a crossed trust boundary: a job, web page or tailnet peer
  reaching the runner host, another client's work or the developer's credentials, or secrets
  landing in job records or logs. Pathological input that only fails the job or session that
  chose it is P3, and P3 doesn't block.
- If a PR reaches a third review round, simplify or re-scope the design instead of patching
  it again.
- CodeRabbit is supplemental. Read its comments and weigh them as above, but don't wait for it
  and don't require a clean CodeRabbit review.
- A merge hold in the PR description or comments, such as "merge only when X", overrides a
  clean review. Leave the PR open until its condition is met.

## Documentation

Most code changes need no doc change. Agents can read the code.

- `README.md` is for users: what offload does, how to install it and how to configure it. Keep
  it accurate when flags, config keys or behavior change.
- `docs/design.md` is for decisions and their reasons, constraints that span the client and
  runner, and traps that are hard to see in the source. Before adding a paragraph, ask what a
  maintainer would get wrong without it. Don't enumerate fields, flags or constants, and don't
  narrate control flow or append PR summaries.
- When a documented decision changes, rewrite or remove the old text. Don't append a second
  account of the new behavior.
- A local reason the code can't show goes in a short comment next to it.

## Plans and scratch files

Don't commit implementation plans, research notes or agent scratch files. Keep temporary
material outside the repository. Put extra checkouts (`git worktree add`) next to this one,
not in `/tmp`, and remove them when you're done.

## Taste

- Default to no comment. Write one only for a constraint, an external quirk, or an invariant the
  code can't show, in 1 to 4 lines.
- Errors tell the user what to do next, such as `add that name to hostnames`.
- Never trust the caller. Paths from the wire are confined to their tree (no `..`, no
  symlinked parents). Secrets stay in tmpfs and out of job records and logs.
- Keep the runner alive before keeping a job alive. Admission, memory reserves and OOM scores
  exist so a runaway job can't take the host down. Don't weaken them to make a job fit.
- Don't add dependencies for small things. The standard library goes a long way.
