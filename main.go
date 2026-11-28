// Command offload sends heavy work (builds, tests, headless clients, MCP servers) to runner
// machines on your tailnet. The same binary is the runner daemon: `offload runner serve`.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"runtime/debug"
	"strings"
	"syscall"
	"time"

	"github.com/RestartFU/offload/internal/api"
	"github.com/RestartFU/offload/internal/client"
	"github.com/RestartFU/offload/internal/dashboard"
	"github.com/RestartFU/offload/internal/hook"
	"github.com/RestartFU/offload/internal/runner"
	"github.com/RestartFU/offload/internal/units"
)

var version = "dev"

func versionString() string {
	if version != "dev" {
		return version
	}
	if bi, ok := debug.ReadBuildInfo(); ok {
		rev, dirty := "", ""
		for _, s := range bi.Settings {
			switch s.Key {
			case "vcs.revision":
				rev = s.Value
			case "vcs.modified":
				if s.Value == "true" {
					dirty = "-dirty"
				}
			}
		}
		if len(rev) > 12 {
			return "dev-" + rev[:12] + dirty
		}
	}
	return version
}

const usage = `offload: run heavy work on runner machines in your tailnet.

Client:
  offload run [flags] -- CMD...      run CMD on the best runner, streaming output
      --on NAME        pick the runner          --profile NAME   cargo, go, … (runner-defined)
      --cpu N          guaranteed cores         --cpu-max N      burst ceiling
      --mem SIZE       memory limit (8G)        --env K=V        extra environment (repeatable)
      --get PATH       bring PATH back after the job (repeatable)
      --with SET       add a credential set from client.toml (auto sets are always added)
      --secret NAME[=SRC]  secret from $NAME, or SRC = cmd:…, env:…, file:…, literal
      --no-credentials no ssh-agent/git relays and no auto credential sets
      --no-sync        don't upload the current directory
      --no-git         don't give the runner the repository's git history
      --no-write-back  don't copy files the job edited (cargo fmt, lockfiles…) back here
      --environment N  use ~/.config/offload/envs/N instead of the repo's environment
      --host           run directly on the runner host instead of in a container
      --platform P     require a runner platform, e.g. linux/arm64
      --network client the job's connections go out from this machine (its IP, DNS, LAN,
                       tailnet; "laptop" / 10.233.0.1 inside the job is this machine's localhost)
      --fresh          start from an empty workspace (cold build)
      --ephemeral      drop the workspace afterwards
      --detach, -d     print the job id and return
      --quiet, -q      no offload status lines
  offload attach ID                  follow a job to the end (Ctrl-C detaches)
  offload cancel ID                  stop a job or session
  offload get ID [DIR]               download a job's --get artifacts
  offload ps [--all]                 jobs and sessions on every runner
  offload hosts                      runners, capacity and load
  offload dashboard [--port N] [--no-open] [--no-remote]
                                     web dashboard on this machine (127.0.0.1 only): runners,
                                     cores, memory, jobs and sessions, logs, statistics; phones
                                     paired under Devices reach it on the tailnet
  offload mcp PROFILE [--on NAME] [--with SET] [--secret NAME[=SRC]]
                                     stdio MCP server backed by a runner session; PROFILE is a
                                     runner profile or ~/.config/offload/sessions/PROFILE.toml
                                     (its data sets are synced first; --prepare runs its setup;
                                     --idle 15m stops it after that long without requests and
                                     starts it again on the next one, 0 never)

Secrets can also come from the environment (e.g. an MCP entry's "env"):
  OFFLOAD_SECRET_<NAME>=<source>   OFFLOAD_SECRET_FILE_<NAME>=<path>   OFFLOAD_WITH=set1,set2
  offload host upgrade NAME|--all [--binary PATH]   push this (or a matching) binary to runners
  offload host pair NAME [--code C]  let this client use a runner (C from "offload runner pair-code" on it)

GitHub Actions (every GitHub job runs as an offload job on a runner with room for it):
  offload github add ORG[/REPO] [--labels offload] [--max N] [--on RUNNER,…] [--shared-cache]
                     [--system-labels[=false]]
                     [--token SRC] [--cpu N] [--cpu-max N] [--mem SIZE (default 8G)]
                                     take ORG's jobs on every runner (or --on); workflows
                                     use "runs-on: offload" (or --labels). Uses your gh
                                     login unless --token gives a token or cmd:/env:/file:.
                                     An org's jobs start with clean caches unless
                                     --shared-cache (its repos trust each other)
  offload github ls                  organizations and repositories, per runner
  offload github rm ORG[/REPO] [--on RUNNER,…] [--forget]
                                     stop taking its jobs and delete them on GitHub
                                     (--forget: only here, when GitHub can't be told)

Environments (jobs run in containers; agents are root inside and can install anything):
  offload env show [--environment N]            the environment a job here would use
  offload env add PKG...                        add system packages (persisted, one sorted layer)
  offload env add --run 'CMD'                   add a build step
  offload env build [--all]                     build it now on the best (or every) runner
    Files: .offload/Containerfile (FROM offload/base), .devcontainer/devcontainer.json,
           ~/.config/offload/envs/NAME/Containerfile

Runner (on the server):
  offload runner install [--name N] [--profiles] config, profiles, systemd user units
  sudo offload runner install-system --user U    linger, cgroup delegation, keep-alive, firewall
  offload runner serve [--config PATH]           the daemon (run by systemd)
  offload runner pair-code                       print a one-time code for "offload host pair"
  offload runner reset-pairing                   unpair every client (revoke), then print a code

Other:
  offload hook claude                Claude Code PreToolUse guard
  offload version
`

func main() {
	log.SetFlags(0)
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	cmd, args := os.Args[1], os.Args[2:]
	var err error
	code := 0
	switch cmd {
	case "run":
		code, err = cmdRun(args)
	case "attach", "logs":
		if len(args) != 1 {
			die("usage: offload attach ID")
		}
		code, err = client.Attach(args[0])
	case "cancel", "stop":
		if len(args) != 1 {
			die("usage: offload cancel ID")
		}
		err = client.Cancel(args[0])
	case "get":
		if len(args) < 1 || len(args) > 2 {
			die("usage: offload get ID [DIR]")
		}
		dest := "."
		if len(args) == 2 {
			dest = args[1]
		}
		err = client.Get(args[0], dest)
	case "ps":
		fs := flag.NewFlagSet("ps", flag.ExitOnError)
		all := fs.Bool("all", false, "include finished jobs")
		_ = fs.Parse(args)
		err = client.Ps(*all)
	case "hosts":
		err = client.Hosts()
	case "dashboard":
		err = cmdDashboard(args)
	case "mcp":
		err = cmdMCP(args)
		var ee *client.ExitError
		if errors.As(err, &ee) {
			code = ee.Code // the MCP server's own status, as `run` passes its command's
		}
	case "host":
		err = cmdHost(args)
	case "env":
		code, err = cmdEnv(args)
	case "github":
		err = cmdGitHub(args)
	case "runner":
		code, err = cmdRunner(args)
	case "hook":
		if len(args) == 1 && args[0] == "claude" {
			os.Exit(hook.Claude(os.Stdin, os.Stdout))
		}
		die("usage: offload hook claude")
	case "version", "--version":
		fmt.Println(versionString())
	case "proto": // the runner protocol this binary speaks (runners check it before an upgrade)
		fmt.Println(api.Proto)
	case "help", "-h", "--help":
		fmt.Print(usage)
	default:
		fmt.Fprintf(os.Stderr, "offload: unknown command %q\n\n%s", cmd, usage)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "offload: %v\n", err)
		if code == 0 {
			code = 1
		}
	}
	os.Exit(code)
}

func die(msg string) {
	fmt.Fprintln(os.Stderr, msg)
	os.Exit(2)
}

type multi []string

func (m *multi) String() string     { return strings.Join(*m, ",") }
func (m *multi) Set(v string) error { *m = append(*m, v); return nil }

// splitArgs separates flags from the command at the first "--" (or the first non-flag word).
func splitArgs(args []string) (flags, rest []string) {
	for i, a := range args {
		if a == "--" {
			return args[:i], args[i+1:]
		}
	}
	return args, nil
}

func resourceFlags(fs *flag.FlagSet) func() (api.Resources, error) {
	cpu := fs.Float64("cpu", 0, "guaranteed cores")
	cpuMax := fs.Float64("cpu-max", 0, "burst ceiling in cores")
	mem := fs.String("mem", "", "memory limit, e.g. 8G")
	return func() (api.Resources, error) {
		m, err := units.ParseSize(*mem)
		return api.Resources{CPU: *cpu, CPUMax: *cpuMax, Mem: m}, err
	}
}

func cmdRun(args []string) (int, error) {
	fl, rest := splitArgs(args)
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	fs.Usage = func() { fmt.Fprint(os.Stderr, usage) }
	var o client.RunOptions
	var env, get, with, secrets multi
	fs.StringVar(&o.On, "on", "", "runner name")
	fs.StringVar(&o.Profile, "profile", "", "runner profile")
	res := resourceFlags(fs)
	fs.Var(&env, "env", "K=V")
	fs.Var(&get, "get", "artifact path")
	fs.Var(&with, "with", "credential set")
	fs.Var(&secrets, "secret", "env var to pass as a secret")
	fs.BoolVar(&o.NoCreds, "no-credentials", false, "")
	fs.StringVar(&o.Environment, "environment", "", "named environment")
	fs.BoolVar(&o.Host, "host", false, "run on the runner host, not in a container")
	fs.StringVar(&o.Platform, "platform", "", "require os/arch, e.g. linux/arm64")
	fs.StringVar(&o.Network, "network", "", "client: connect out from this machine")
	fs.BoolVar(&o.NoSync, "no-sync", false, "")
	fs.BoolVar(&o.NoWriteBack, "no-write-back", false, "")
	fs.BoolVar(&o.NoGit, "no-git", false, "")
	fs.BoolVar(&o.Fresh, "fresh", false, "")
	fs.BoolVar(&o.Ephemeral, "ephemeral", false, "")
	fs.BoolVar(&o.Detach, "detach", false, "")
	fs.BoolVar(&o.Detach, "d", false, "")
	fs.BoolVar(&o.Quiet, "quiet", false, "")
	fs.BoolVar(&o.Quiet, "q", false, "")
	_ = fs.Parse(fl)
	if rest == nil {
		rest = fs.Args()
	}
	if len(rest) == 0 {
		die("usage: offload run [flags] -- CMD...")
	}
	var err error
	if o.Resources, err = res(); err != nil {
		return 2, err
	}
	o.Env = map[string]string{}
	for _, kv := range env {
		k, v, ok := strings.Cut(kv, "=")
		if !ok {
			return 2, fmt.Errorf("--env wants K=V, got %q", kv)
		}
		o.Env[k] = v
	}
	o.Artifacts = get
	o.With, o.Secrets = with, secrets
	o.Argv = rest
	return client.Run(o)
}

func cmdMCP(args []string) error {
	if len(args) < 1 || strings.HasPrefix(args[0], "-") {
		die("usage: offload mcp PROFILE [--on NAME] [--cpu N] [--mem SIZE] [--idle 15m]")
	}
	profile := args[0]
	fs := flag.NewFlagSet("mcp", flag.ExitOnError)
	on := fs.String("on", "", "runner name")
	res := resourceFlags(fs)
	var with, secrets multi
	fs.Var(&with, "with", "credential set")
	fs.Var(&secrets, "secret", "NAME or NAME=SOURCE")
	environment := fs.String("environment", "", "named environment")
	host := fs.Bool("host", false, "run on the runner host")
	prepare := fs.Bool("prepare", false, "run the session's prepare step and exit")
	idle := fs.Duration("idle", 15*time.Minute, "stop the session after this long without requests (0: never)")
	_ = fs.Parse(args[1:])
	r, err := res()
	if err != nil {
		return err
	}
	return client.MCP(client.MCPOptions{Profile: profile, On: *on, Resources: r, With: with, Secrets: secrets, Environment: *environment, Host: *host, Prepare: *prepare, Idle: *idle})
}

func cmdDashboard(args []string) error {
	fs := flag.NewFlagSet("dashboard", flag.ExitOnError)
	port := fs.Int("port", 7421, "loopback port (another free one is used if it's taken)")
	noOpen := fs.Bool("no-open", false, "don't open the browser")
	noRemote := fs.Bool("no-remote", false, "don't listen on the tailnet for paired phones")
	_ = fs.Parse(args)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return dashboard.Run(ctx, dashboard.Options{Addr: fmt.Sprintf("127.0.0.1:%d", *port), Open: !*noOpen, Remote: !*noRemote, Version: versionString()})
}

func cmdHost(args []string) error {
	if len(args) >= 1 && args[0] == "pair" {
		var names []string
		all, code := false, ""
		for i := 1; i < len(args); i++ { // flags may follow the name: `host pair vps --code X`
			switch a := args[i]; {
			case a == "--all" || a == "-all":
				all = true
			case (a == "--code" || a == "-code") && i+1 < len(args):
				i++
				code = args[i]
			case strings.HasPrefix(a, "--code="):
				code = strings.TrimPrefix(a, "--code=")
			case strings.HasPrefix(a, "-"):
				die("usage: offload host pair NAME... [--code CODE] | --all")
			default:
				names = append(names, a)
			}
		}
		return client.Pair(names, all, code)
	}
	if len(args) < 1 || args[0] != "upgrade" {
		die("usage: offload host upgrade|pair NAME...|--all")
	}
	fs := flag.NewFlagSet("upgrade", flag.ExitOnError)
	all := fs.Bool("all", false, "upgrade every runner")
	binary := fs.String("binary", "", "binary to push (default: this one)")
	force := fs.Bool("force", false, "upgrade even if it stops running work")
	_ = fs.Parse(args[1:])
	return client.Upgrade(fs.Args(), *all, *binary, *force)
}

func cmdGitHub(args []string) error {
	usage := "usage: offload github add ORG[/REPO] [flags] | ls | rm ORG[/REPO] [--on RUNNER,…]"
	if len(args) < 1 {
		die(usage)
	}
	list := func(v string) []string {
		var out []string
		for _, x := range strings.Split(v, ",") {
			if x = strings.TrimSpace(x); x != "" {
				out = append(out, x)
			}
		}
		return out
	}
	switch args[0] {
	case "add":
		if len(args) < 2 || strings.HasPrefix(args[1], "-") {
			die("usage: offload github add ORG[/REPO] [--labels offload] [--max N] [--on RUNNER,…] [--token SRC] [--cpu N] [--cpu-max N] [--mem SIZE]")
		}
		fs := flag.NewFlagSet("github add", flag.ExitOnError)
		o := client.GitHubOptions{Owner: args[1]}
		fs.StringVar(&o.Token, "token", "", "token, or cmd:…, env:…, file:… (default: your gh login)")
		labels := fs.String("labels", "", "labels workflows use in runs-on, comma-separated (default offload)")
		on := fs.String("on", "", "runners to use, comma-separated (default: all)")
		maxJobs := fs.Int("max", 0, "most GitHub jobs at once per runner (default: as many as fit)")
		shared := fs.Bool("shared-cache", false, "let the organization's repositories share warm caches")
		system := fs.Bool("system-labels", true, "also carry GitHub's default self-hosted labels (default for new connections)")
		res := resourceFlags(fs)
		_ = fs.Parse(args[2:])
		if fs.NArg() > 0 {
			die("offload github add: unexpected arguments after the flags (give one ORG[/REPO], then flags)")
		}
		fs.Visit(func(f *flag.Flag) {
			switch f.Name {
			case "shared-cache":
				o.Shared = shared
			case "system-labels":
				o.System = system
			case "max":
				o.Max = maxJobs
			case "on":
				if len(list(*on)) == 0 {
					die("offload github add: --on needs runner names")
				}
			}
		})
		var err error
		if o.Resources, err = res(); err != nil {
			return err
		}
		o.Labels, o.On = list(*labels), list(*on)
		return client.GitHubAdd(o)
	case "ls", "list":
		return client.GitHubList()
	case "rm", "remove":
		if len(args) < 2 || strings.HasPrefix(args[1], "-") {
			die("usage: offload github rm ORG[/REPO] [--on RUNNER,…]")
		}
		fs := flag.NewFlagSet("github rm", flag.ExitOnError)
		on := fs.String("on", "", "runners, comma-separated (default: all)")
		forget := fs.Bool("forget", false, "remove here even if GitHub can't be told (a revoked token)")
		_ = fs.Parse(args[2:])
		if fs.NArg() > 0 {
			die("offload github rm: unexpected arguments after the flags (give one ORG[/REPO], then flags)")
		}
		fs.Visit(func(f *flag.Flag) {
			if f.Name == "on" && len(list(*on)) == 0 {
				die("offload github rm: --on needs runner names")
			}
		})
		return client.GitHubRemove(args[1], list(*on), *forget)
	}
	die(usage)
	return nil
}

func cmdEnv(args []string) (int, error) {
	if len(args) < 1 {
		die("usage: offload env show|add|build")
	}
	fs := flag.NewFlagSet("env", flag.ExitOnError)
	named := fs.String("environment", "", "named environment")
	run := fs.String("run", "", "build step")
	all := fs.Bool("all", false, "every runner")
	on := fs.String("on", "", "runner")
	_ = fs.Parse(args[1:])
	switch args[0] {
	case "show":
		return 0, client.EnvShow(*named)
	case "add":
		if fs.NArg() == 0 && *run == "" {
			die("usage: offload env add PKG... | --run 'CMD'")
		}
		return 0, client.EnvAdd(*named, fs.Args(), *run)
	case "build":
		return client.EnvBuild(*named, *on, *all)
	}
	die("usage: offload env show|add|build")
	return 2, nil
}

func cmdRunner(args []string) (int, error) {
	if len(args) < 1 {
		die("usage: offload runner serve|install|install-system|pair-code|reset-pairing")
	}
	switch args[0] {
	case "serve":
		fs := flag.NewFlagSet("serve", flag.ExitOnError)
		cfgPath := fs.String("config", runner.DefaultConfigPath(), "config file")
		_ = fs.Parse(args[1:])
		cfg, err := runner.LoadConfig(*cfgPath)
		if err != nil {
			return 1, err
		}
		s, err := runner.New(cfg, versionString())
		if err != nil {
			return 1, err
		}
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		log.Printf("offload runner %s (%s) starting", cfg.Name, versionString())
		return 0, s.Serve(ctx)
	case "pair-code", "reset-pairing":
		fs := flag.NewFlagSet(args[0], flag.ExitOnError)
		cfgPath := fs.String("config", runner.DefaultConfigPath(), "config file")
		_ = fs.Parse(args[1:])
		cfg, err := runner.LoadConfig(*cfgPath)
		if err != nil {
			return 1, err
		}
		tokens := cfg.TokensFile // "" is the default the runner uses too
		newCode := runner.NewPairCode
		if args[0] == "reset-pairing" {
			newCode = runner.ResetPairingLocal
		}
		code, err := newCode(tokens)
		if err == nil && args[0] == "reset-pairing" {
			fmt.Println("forgot every paired client")
		}
		if err != nil {
			return 1, err
		}
		fmt.Printf("pairing code (one use, valid 24h): %s\non the laptop: offload host pair %s --code %s\n", code, cfg.Name, code)
		return 0, nil
	case "install":
		fs := flag.NewFlagSet("install", flag.ExitOnError)
		name := fs.String("name", "", "runner name (default: hostname)")
		force := fs.Bool("force", false, "overwrite runner.toml")
		profiles := fs.Bool("profiles", false, "overwrite the built-in profiles")
		_ = fs.Parse(args[1:])
		return 0, runner.Install(runner.InstallOptions{Name: *name, Force: *force, Profiles: *profiles, Out: os.Stdout})
	case "install-system":
		fs := flag.NewFlagSet("install-system", flag.ExitOnError)
		user := fs.String("user", os.Getenv("SUDO_USER"), "user that runs the runner")
		port := fs.Int("port", api.DefaultPort, "runner port")
		_ = fs.Parse(args[1:])
		if *user == "" {
			die("--user is required")
		}
		return 0, runner.InstallSystem(runner.SystemOptions{User: *user, Port: *port, Out: os.Stdout})
	case "exec":
		fs := flag.NewFlagSet("exec", flag.ExitOnError)
		dir := fs.String("job-dir", "", "")
		wb := fs.String("write-back", "", "")
		secrets := fs.String("secrets", "", "")
		record := fs.String("record", "", "")
		var arts multi
		fs.Var(&arts, "artifact", "")
		fl, rest := splitArgs(args[1:])
		_ = fs.Parse(fl)
		if *dir == "" || len(rest) == 0 {
			die("internal: runner exec --job-dir DIR -- CMD")
		}
		return runner.RunExec(runner.ExecOptions{JobDir: *dir, Artifacts: arts, WriteBack: *wb, Record: *record, Secrets: *secrets, Argv: rest}), nil
	case "inner":
		fs := flag.NewFlagSet("inner", flag.ExitOnError)
		secrets := fs.String("secrets", "", "")
		stdin := fs.Bool("stdin", false, "")
		clientNet := fs.Bool("net-client", false, "")
		fl, rest := splitArgs(args[1:])
		_ = fs.Parse(fl)
		if len(rest) == 0 {
			die("internal: runner inner [--secrets DIR] [--stdin] -- CMD")
		}
		return runner.RunInner(*secrets, *stdin, *clientNet, rest), nil
	case "credential":
		op := ""
		if len(args) > 1 {
			op = args[1]
		}
		return runner.CredentialHelper(op, os.Stdin, os.Stdout), nil
	case "stopped": // ExecStopPost hook of job and session units
		if len(args) < 2 {
			die("internal: runner stopped PATH")
		}
		return runner.RunStopped(args[1]), nil
	case "pipe":
		fs := flag.NewFlagSet("pipe", flag.ExitOnError)
		dir := fs.String("dir", "", "")
		sock := fs.String("sock", "", "")
		secrets := fs.String("secrets", "", "")
		noStdin := fs.Bool("no-stdin", false, "")
		fl, rest := splitArgs(args[1:])
		_ = fs.Parse(fl)
		if *dir == "" || *sock == "" || len(rest) == 0 {
			die("internal: runner pipe --dir DIR --sock PATH -- CMD")
		}
		return runner.RunPipe(*dir, *sock, *secrets, *noStdin, rest), nil
	}
	die("usage: offload runner serve|install|install-system|pair-code|reset-pairing")
	return 2, nil
}
