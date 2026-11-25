package runner

import (
	"bufio"
	"context"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// meminfo reads one field of /proc/meminfo in bytes.
func meminfo(field string) int64 {
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return 0
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		name, rest, ok := strings.Cut(sc.Text(), ":")
		if !ok || name != field {
			continue
		}
		v, _ := strconv.ParseInt(strings.Fields(rest)[0], 10, 64)
		return v * 1024
	}
	return 0
}

func memTotal() int64     { return meminfo("MemTotal") }
func memAvailable() int64 { return meminfo("MemAvailable") }

func load1() float64 {
	b, err := os.ReadFile("/proc/loadavg")
	if err != nil {
		return 0
	}
	v, _ := strconv.ParseFloat(strings.Fields(string(b))[0], 64)
	return v
}

// ownCgroup returns this process's cgroup v2 directory under /sys/fs/cgroup.
func ownCgroup() string {
	b, err := os.ReadFile("/proc/self/cgroup")
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		if p, ok := strings.CutPrefix(line, "0::"); ok {
			return filepath.Join("/sys/fs/cgroup", p)
		}
	}
	return ""
}

// delegatedControllers lists the cgroup controllers the user manager may use, by walking up from
// this process's cgroup to the user@UID.service cgroup.
func delegatedControllers() map[string]bool {
	out := map[string]bool{}
	dir := ownCgroup()
	for dir != "" && dir != "/sys/fs/cgroup" {
		if strings.HasPrefix(filepath.Base(dir), "user@") {
			b, err := os.ReadFile(filepath.Join(dir, "cgroup.controllers"))
			if err == nil {
				for _, c := range strings.Fields(string(b)) {
					out[c] = true
				}
			}
			return out
		}
		dir = filepath.Dir(dir)
	}
	// Not under a user manager (tests, foreground runs): report what our own cgroup offers.
	if b, err := os.ReadFile(filepath.Join(ownCgroup(), "cgroup.controllers")); err == nil {
		for _, c := range strings.Fields(string(b)) {
			out[c] = true
		}
	}
	return out
}

func systemctl(ctx context.Context, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "systemctl", append([]string{"--user"}, args...)...).CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

// unitActive reports whether a user unit is still loaded and not finished.
// unitActive reports whether a user unit is still running. known is false when systemd could
// not be asked: callers must not treat that as an exit (it would release a live job's resources).
func unitActive(unit string) (active, known bool) {
	out, err := systemctl(context.Background(), "show", "-p", "ActiveState", "--value", unit)
	if err != nil {
		return false, false
	}
	switch out {
	case "active", "activating", "deactivating", "reloading":
		return true, true
	case "inactive", "failed":
		return false, true // includes units systemd no longer knows (they show as inactive)
	}
	return false, false
}

// unitSpec is what we hand to systemd-run.
type unitSpec struct {
	Name        string
	Slice       string
	CPUWeight   int
	CPUQuotaPct int
	AllowedCPUs string
	MemoryMax   int64
	IOWeight    int
	OOMScoreAdj int
	Dir         string
	Env         map[string]string
	Stdout      string // file path; stdout and stderr are appended here
	Stderr      string // optional separate stderr file
	Delegate    bool   // let podman create sub-cgroups (--cgroups=split)
	Argv        []string
}

// startUnit launches argv as a transient user service, so it outlives the runner process.
func startUnit(ctx context.Context, u unitSpec) error {
	args := []string{
		"--user", "--quiet", "--collect",
		"--unit=" + u.Name,
		"--slice=" + u.Slice,
		"--working-directory=" + u.Dir,
		"-p", "Type=exec",
		"-p", fmt.Sprintf("CPUWeight=%d", u.CPUWeight),
		"-p", fmt.Sprintf("CPUQuota=%d%%", u.CPUQuotaPct),
		"-p", fmt.Sprintf("MemoryMax=%d", u.MemoryMax),
		"-p", "MemorySwapMax=0",
		"-p", fmt.Sprintf("OOMScoreAdjust=%d", u.OOMScoreAdj),
		"-p", "OOMPolicy=continue", // the wrapper survives an OOM kill of the command and records it
		"-p", "TimeoutStopSec=15",
		"-p", "StandardInput=null",
		"-p", "StandardOutput=append:" + u.Stdout,
	}
	stderr := u.Stderr
	if stderr == "" {
		stderr = u.Stdout
	}
	args = append(args, "-p", "StandardError=append:"+stderr)
	if u.AllowedCPUs != "" {
		args = append(args, "-p", "AllowedCPUs="+u.AllowedCPUs)
	}
	if u.Delegate {
		args = append(args, "-p", "Delegate=yes")
	}
	if u.IOWeight > 0 {
		args = append(args, "-p", fmt.Sprintf("IOWeight=%d", u.IOWeight))
	}
	for k, v := range u.Env {
		args = append(args, "--setenv="+k+"="+v)
	}
	// systemd expands $VAR and ${VAR} in a unit's command line; commands must reach the job
	// literally (`sh -c 'x=1; echo ${x}'`).
	argv := u.Argv
	if systemdVersion() >= 254 {
		args = append(args, "--expand-environment=no")
	} else {
		argv = make([]string, len(u.Argv))
		for i, a := range u.Argv {
			argv[i] = strings.ReplaceAll(a, "$", "$$")
		}
	}
	args = append(args, "--")
	args = append(args, argv...)
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "systemd-run", args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("systemd-run: %v: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

var systemdVer = sync.OnceValue(func() int {
	out, err := exec.Command("systemctl", "--version").Output()
	if err != nil {
		return 0
	}
	f := strings.Fields(string(out)) // "systemd 255 (255.4-1ubuntu8)"
	if len(f) < 2 {
		return 0
	}
	n, _ := strconv.Atoi(f[1])
	return n
})

func systemdVersion() int { return systemdVer() }

// stopUnit asks systemd to stop a unit and keeps asking (retryStops) until it confirms the unit
// is gone: a user manager that is briefly unavailable must not leave work running for good.
func stopUnit(unit string) {
	pendingStops.Lock()
	pendingStops.m[unit] = time.Now()
	pendingStops.Unlock()
	go systemctl(context.Background(), "stop", "--no-block", unit)
}

var pendingStops = struct {
	sync.Mutex
	m       map[string]time.Time
	running atomic.Bool
}{m: map[string]time.Time{}}

// retryStops re-sends stops that systemd hasn't confirmed yet. Called from the server loop.
func retryStops() {
	if !pendingStops.running.CompareAndSwap(false, true) {
		return
	}
	go func() {
		defer pendingStops.running.Store(false)
		pendingStops.Lock()
		units := make(map[string]time.Time, len(pendingStops.m))
		for u, at := range pendingStops.m {
			units[u] = at
		}
		pendingStops.Unlock()
		for u, at := range units {
			if active, known := unitActive(u); known && !active {
				pendingStops.Lock()
				delete(pendingStops.m, u)
				pendingStops.Unlock()
				continue
			}
			if time.Since(at) > 5*time.Second {
				_, _ = systemctl(context.Background(), "stop", "--no-block", u)
			}
		}
	}()
}

// cpuWeight maps a guaranteed CPU share to a cgroup weight (100 per CPU, clamped).
func cpuWeight(cpu float64) int {
	return int(math.Max(1, math.Min(10000, math.Round(cpu*100))))
}

// parallelEnv tells self-sizing tools how many cores they really have.
func parallelEnv(n int) map[string]string {
	s := strconv.Itoa(n)
	return map[string]string{
		"CARGO_BUILD_JOBS":           s,
		"MAKEFLAGS":                  "-j" + s,
		"GOMAXPROCS":                 s,
		"RAYON_NUM_THREADS":          s,
		"NUM_JOBS":                   s,
		"CMAKE_BUILD_PARALLEL_LEVEL": s,
		"OFFLOAD_CPUS":               s,
		"NEXTEST_TEST_THREADS":       s,
		"RUST_TEST_THREADS":          s,
		"UV_CONCURRENT_BUILDS":       s,
	}
}

func contextBG() context.Context { return context.Background() }
