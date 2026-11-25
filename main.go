// Command offload sends heavy work (builds, tests, headless clients, MCP servers) to runner
// machines on your tailnet. The same binary is the runner daemon: `offload runner serve`.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"runtime/debug"
	"strings"
	"syscall"

	"github.com/RestartFU/offload/internal/api"
	"github.com/RestartFU/offload/internal/client"
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
      --no-sync        don't upload the current directory
      --fresh          start from an empty workspace (cold build)
      --ephemeral      drop the workspace afterwards
      --detach, -d     print the job id and return
      --quiet, -q      no offload status lines
  offload attach ID                  follow a job to the end (Ctrl-C detaches)
  offload cancel ID                  stop a job or session
  offload get ID [DIR]               download a job's --get artifacts
  offload ps [--all]                 jobs and sessions on every runner
  offload hosts                      runners, capacity and load
  offload mcp PROFILE [--on NAME]    stdio MCP server backed by a runner session
  offload host upgrade NAME|--all    push this binary to runners

Runner (on the server):
  offload runner install [--name N]              config, profiles, systemd user units
  sudo offload runner install-system --user U    linger, cgroup delegation, keep-alive, firewall
  offload runner serve [--config PATH]           the daemon (run by systemd)

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
	case "mcp":
		err = cmdMCP(args)
	case "host":
		err = cmdHost(args)
	case "runner":
		code, err = cmdRunner(args)
	case "hook":
		if len(args) == 1 && args[0] == "claude" {
			os.Exit(hook.Claude(os.Stdin, os.Stdout))
		}
		die("usage: offload hook claude")
	case "version", "--version":
		fmt.Println(versionString())
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
	var env, get multi
	fs.StringVar(&o.On, "on", "", "runner name")
	fs.StringVar(&o.Profile, "profile", "", "runner profile")
	res := resourceFlags(fs)
	fs.Var(&env, "env", "K=V")
	fs.Var(&get, "get", "artifact path")
	fs.BoolVar(&o.NoSync, "no-sync", false, "")
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
	o.Argv = rest
	return client.Run(o)
}

func cmdMCP(args []string) error {
	if len(args) < 1 || strings.HasPrefix(args[0], "-") {
		die("usage: offload mcp PROFILE [--on NAME] [--cpu N] [--mem SIZE]")
	}
	profile := args[0]
	fs := flag.NewFlagSet("mcp", flag.ExitOnError)
	on := fs.String("on", "", "runner name")
	res := resourceFlags(fs)
	_ = fs.Parse(args[1:])
	r, err := res()
	if err != nil {
		return err
	}
	return client.MCP(profile, *on, r)
}

func cmdHost(args []string) error {
	if len(args) < 1 || args[0] != "upgrade" {
		die("usage: offload host upgrade NAME...|--all")
	}
	fs := flag.NewFlagSet("upgrade", flag.ExitOnError)
	all := fs.Bool("all", false, "upgrade every runner")
	_ = fs.Parse(args[1:])
	return client.Upgrade(fs.Args(), *all)
}

func cmdRunner(args []string) (int, error) {
	if len(args) < 1 {
		die("usage: offload runner serve|install|install-system")
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
	case "install":
		fs := flag.NewFlagSet("install", flag.ExitOnError)
		name := fs.String("name", "", "runner name (default: hostname)")
		force := fs.Bool("force", false, "overwrite runner.toml")
		_ = fs.Parse(args[1:])
		return 0, runner.Install(runner.InstallOptions{Name: *name, Force: *force, Out: os.Stdout})
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
		var arts multi
		fs.Var(&arts, "artifact", "")
		fl, rest := splitArgs(args[1:])
		_ = fs.Parse(fl)
		if *dir == "" || len(rest) == 0 {
			die("internal: runner exec --job-dir DIR -- CMD")
		}
		return runner.RunExec(*dir, arts, rest), nil
	case "pipe":
		fs := flag.NewFlagSet("pipe", flag.ExitOnError)
		dir := fs.String("dir", "", "")
		sock := fs.String("sock", "", "")
		fl, rest := splitArgs(args[1:])
		_ = fs.Parse(fl)
		if *dir == "" || *sock == "" || len(rest) == 0 {
			die("internal: runner pipe --dir DIR --sock PATH -- CMD")
		}
		return runner.RunPipe(*dir, *sock, rest), nil
	}
	die("usage: offload runner serve|install|install-system")
	return 2, nil
}
