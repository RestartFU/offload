package runner

import (
	"context"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/RestartFU/offload/internal/api"
	"github.com/RestartFU/offload/internal/tsapi"
	"github.com/RestartFU/offload/internal/units"
	"github.com/RestartFU/offload/profiles"
)

// InstallOptions configures `offload runner install` (the unprivileged half).
type InstallOptions struct {
	Name     string
	Force    bool // overwrite an existing runner.toml
	Profiles bool // overwrite the built-in profiles with this version's
	Out      io.Writer
}

// reservedCPUs picks the hyperthread siblings of the highest-numbered CPU: one whole core left
// for the OS, sshd, tailscaled and the runner itself.
func reservedCPUs() []int {
	last := runtime.NumCPU() - 1
	b, err := os.ReadFile(fmt.Sprintf("/sys/devices/system/cpu/cpu%d/topology/thread_siblings_list", last))
	if err != nil {
		return []int{last}
	}
	c, err := units.ParseCPUList(string(b))
	if err != nil || len(c) == 0 {
		return []int{last}
	}
	return c
}

// defaultCapacity hands out everything except one core and a memory reserve.
func defaultCapacity() (cpus []int, mem, reserve int64) {
	skip := map[int]bool{}
	if runtime.NumCPU() > 2 {
		for _, c := range reservedCPUs() {
			skip[c] = true
		}
	}
	for i := 0; i < runtime.NumCPU(); i++ {
		if !skip[i] {
			cpus = append(cpus, i)
		}
	}
	total := memTotal()
	keep := max(int64(4)<<30, total/8)
	mem = (total - keep) &^ (1<<30 - 1) // whole GiB
	reserve = max(int64(2)<<30, total/10)
	return cpus, mem, reserve &^ (1<<28 - 1)
}

const runnerTemplate = `# offload runner configuration. Restart with: systemctl --user restart offload-runner
name = %q
listen = ["tailscale:%d", "unix"]   # tailnet addresses only, plus a local socket
allow_users = [%s]                  # tailnet logins allowed to use this runner
allow_tags = []
# tokens_file = "~/.config/offload/tokens"   # optional bearer tokens, one per line
# hostnames = ["box.example"]   # other DNS names tailnet clients use for this runner (IPs and its own names always work)

[capacity]
cpu = %d               # guaranteed CPU handed out in total
cpus = %q   # cores jobs may run on (one core is left to the system)
mem = %q            # memory handed out in total (also the hard cap of offload.slice)

[reserve]
mem_available = %q    # never start work that would leave the host with less MemAvailable

[defaults]
cpu = 2
mem = "4G"

[storage]
persistent = true      # keep workspaces (and their build output) between jobs
max_slots_per_key = 3  # concurrent jobs per repository
[storage.cache]
retain = "14d"         # drop workspaces unused this long
[storage.jobs]
retain = "24h"         # keep finished job logs and artifacts this long

[policy]
max_sessions = 4
session_grace = "5m"   # stop a session this long after its last client left
io_weight = 50
oom_score_adjust = 900 # the kernel kills offload work before anything else

[env]
PATH = "~/.cargo/bin:~/go/bin:~/.local/go/bin:~/.bun/bin:~/.local/bin:/usr/local/bin:/usr/bin:/bin"
`

const sliceTemplate = `[Unit]
Description=offload jobs and sessions

[Slice]
CPUWeight=50
IOWeight=50
AllowedCPUs=%s
MemoryMax=%d
MemorySwapMax=0
ManagedOOMMemoryPressure=kill
ManagedOOMMemoryPressureLimit=60%%
`

const serviceTemplate = `[Unit]
Description=offload runner
Wants=network-online.target
After=network-online.target

[Service]
ExecStart=%s runner serve
Restart=always
RestartSec=2
MemoryMax=512M

[Install]
WantedBy=default.target
`

// Install sets up the runner for the current user: binary, config, profiles, systemd units.
func Install(o InstallOptions) error {
	out := o.Out
	home, _ := os.UserHomeDir()
	bin := filepath.Join(home, ".local", "bin", "offload")
	self, err := os.Executable()
	if err != nil {
		return err
	}
	if self != bin {
		if err := copyFile(self, bin, 0o755); err != nil {
			return fmt.Errorf("install binary: %w", err)
		}
		fmt.Fprintf(out, "installed %s\n", bin)
	}

	cfgPath := DefaultConfigPath()
	if _, err := os.Stat(cfgPath); err != nil || o.Force {
		name := o.Name
		if name == "" {
			name, _ = os.Hostname()
		}
		login := ""
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		if st, err := tsapi.GetStatus(ctx); err == nil {
			login = st.SelfLogin()
		}
		cancel()
		users := ""
		if login != "" {
			users = strconv.Quote(login)
		}
		cpus, mem, reserve := defaultCapacity()
		body := fmt.Sprintf(runnerTemplate, name, api.DefaultPort, users, len(cpus),
			units.FormatCPUList(cpus), units.FormatSize(mem), units.FormatSize(reserve))
		if err := os.MkdirAll(filepath.Dir(cfgPath), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(cfgPath, []byte(body), 0o644); err != nil {
			return err
		}
		fmt.Fprintf(out, "wrote %s (%d cpus, %s memory)\n", cfgPath, len(cpus), units.FormatSize(mem))
		if login == "" {
			fmt.Fprintln(out, "warning: could not read the tailnet login; set allow_users in runner.toml")
		}
	}
	cfg, err := LoadConfig(cfgPath)
	if err != nil {
		return fmt.Errorf("%s: %w", cfgPath, err)
	}

	if err := os.MkdirAll(cfg.ProfilesDir, 0o755); err != nil {
		return err
	}
	entries, _ := fs.ReadDir(profiles.Defaults, ".")
	for _, e := range entries {
		dst := filepath.Join(cfg.ProfilesDir, e.Name())
		if _, err := os.Stat(dst); err == nil && !o.Profiles {
			continue
		}
		b, _ := fs.ReadFile(profiles.Defaults, e.Name())
		if err := os.WriteFile(dst, b, 0o644); err != nil {
			return err
		}
		fmt.Fprintf(out, "wrote profile %s\n", dst)
	}

	unitDir := filepath.Join(home, ".config", "systemd", "user")
	if err := os.MkdirAll(unitDir, 0o755); err != nil {
		return err
	}
	files := map[string]string{
		cfg.Slice:                fmt.Sprintf(sliceTemplate, units.FormatCPUList(cfg.cpus), cfg.memCap),
		"offload-runner.service": fmt.Sprintf(serviceTemplate, bin),
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(unitDir, name), []byte(body), 0o644); err != nil {
			return err
		}
		fmt.Fprintf(out, "wrote %s\n", filepath.Join(unitDir, name))
	}
	for _, args := range [][]string{{"daemon-reload"}, {"enable", "offload-runner.service"}, {"restart", "offload-runner.service"}} {
		if msg, err := systemctl(context.Background(), args...); err != nil {
			return fmt.Errorf("systemctl --user %s: %v: %s", strings.Join(args, " "), err, msg)
		}
	}
	fmt.Fprintln(out, "started offload-runner.service")

	ctrl := delegatedControllers()
	var missing []string
	for _, c := range []string{"cpu", "cpuset", "io", "memory", "pids"} {
		if !ctrl[c] {
			missing = append(missing, c)
		}
	}
	linger, _ := os.Stat(filepath.Join("/var/lib/systemd/linger", currentUser()))
	if len(missing) > 0 || linger == nil {
		fmt.Fprintf(out, "\nOne root step is left (missing controllers: %s; linger: %v):\n  sudo %s runner install-system --user %s\n",
			strings.Join(missing, " "), linger != nil, bin, currentUser())
	}
	return nil
}

func currentUser() string {
	if u, err := user.Current(); err == nil {
		return u.Username
	}
	return os.Getenv("USER")
}

// SystemOptions configures `offload runner install-system` (run as root).
type SystemOptions struct {
	User string
	Port int
	Out  io.Writer
}

// keepAlive protects the services you need to reach the box when it is under load.
const keepAlive = `[Service]
OOMScoreAdjust=-1000
MemoryMin=64M
CPUWeight=1000
`

// InstallSystem does the root-only setup: linger, cgroup delegation to the user manager, OOM
// protection for sshd and tailscaled, and a firewall opening for the tailnet only.
func InstallSystem(o SystemOptions) error {
	out := o.Out
	if os.Geteuid() != 0 {
		return fmt.Errorf("install-system must run as root")
	}
	u, err := user.Lookup(o.User)
	if err != nil {
		return err
	}
	run := func(name string, args ...string) error {
		b, err := exec.Command(name, args...).CombinedOutput()
		if err != nil {
			return fmt.Errorf("%s %s: %v: %s", name, strings.Join(args, " "), err, strings.TrimSpace(string(b)))
		}
		return nil
	}
	if err := run("loginctl", "enable-linger", o.User); err != nil {
		return err
	}
	fmt.Fprintf(out, "enabled linger for %s (runner starts at boot, survives logout)\n", o.User)

	drop := map[string]string{
		fmt.Sprintf("/etc/systemd/system/user@%s.service.d/offload.conf", u.Uid): "[Service]\nDelegate=cpu cpuset io memory pids\n",
	}
	for _, svc := range []string{"sshd.service", "tailscaled.service"} {
		if _, err := os.Stat(filepath.Join("/usr/lib/systemd/system", svc)); err == nil {
			drop["/etc/systemd/system/"+svc+".d/offload-keepalive.conf"] = keepAlive
		} else if _, err := os.Stat(filepath.Join("/etc/systemd/system", svc)); err == nil {
			drop["/etc/systemd/system/"+svc+".d/offload-keepalive.conf"] = keepAlive
		}
	}
	for path, body := range drop {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			return err
		}
		fmt.Fprintf(out, "wrote %s\n", path)
	}
	if err := run("systemctl", "daemon-reload"); err != nil {
		return err
	}
	// Apply the drop-ins to running services without restarting them.
	for _, svc := range []string{"sshd.service", "tailscaled.service"} {
		if _, ok := drop["/etc/systemd/system/"+svc+".d/offload-keepalive.conf"]; ok {
			_ = run("systemctl", "set-property", "--runtime", svc, "MemoryMin=64M", "CPUWeight=1000")
			if pid, err := exec.Command("systemctl", "show", "-p", "MainPID", "--value", svc).Output(); err == nil {
				p := strings.TrimSpace(string(pid))
				if p != "" && p != "0" {
					_ = os.WriteFile("/proc/"+p+"/oom_score_adj", []byte("-1000"), 0o644)
				}
			}
		}
	}

	if _, err := exec.LookPath("firewall-cmd"); err == nil && run("firewall-cmd", "--state") == nil {
		for _, src := range []string{"100.64.0.0/10", "fd7a:115c:a1e0::/48"} {
			fam := "ipv4"
			if strings.Contains(src, ":") {
				fam = "ipv6"
			}
			rule := fmt.Sprintf(`rule family="%s" source address="%s" port port="%d" protocol="tcp" accept`, fam, src, o.Port)
			_ = run("firewall-cmd", "--permanent", "--add-rich-rule="+rule)
			_ = run("firewall-cmd", "--add-rich-rule="+rule)
		}
		fmt.Fprintf(out, "firewalld: opened tcp/%d to tailnet addresses only\n", o.Port)
	}
	fmt.Fprintf(out, "\nNow, as %s, run:  systemctl --user daemon-reexec && systemctl --user restart offload-runner\n", o.User)
	return nil
}

func copyFile(src, dst string, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	tmp := dst + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, mode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(f, in); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, dst)
}
