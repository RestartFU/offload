package runner

import (
	"fmt"
	"log"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/BurntSushi/toml"

	"github.com/RestartFU/offload/internal/api"
	"github.com/RestartFU/offload/internal/units"
)

// Config is the runner's TOML configuration (~/.config/offload/runner.toml).
type Config struct {
	Name       string   `toml:"name"`
	Listen     []string `toml:"listen"`      // "tailscale:7420", "unix", or "host:port"
	AllowUsers []string `toml:"allow_users"` // tailnet login names allowed to use this runner
	AllowTags  []string `toml:"allow_tags"`  // tailnet node tags allowed to use this runner
	TokensFile string   `toml:"tokens_file"` // optional: one bearer token per line
	// Hostnames are extra DNS names tailnet clients use to reach this runner (a [[host]] url).
	// IP addresses, localhost, this machine's hostname and tailnet name are always accepted.
	Hostnames   []string          `toml:"hostnames"`
	StateDir    string            `toml:"state_dir"`
	ProfilesDir string            `toml:"profiles_dir"`
	Slice       string            `toml:"slice"`
	Env         map[string]string `toml:"env"` // base environment for every job and session

	Capacity struct {
		CPU  float64 `toml:"cpu"`  // guaranteed CPU the runner hands out in total
		Mem  string  `toml:"mem"`  // memory the runner hands out in total
		CPUs string  `toml:"cpus"` // cpuset jobs may run on, e.g. "0-14,16-30"
	} `toml:"capacity"`

	Reserve struct {
		MemAvailable string `toml:"mem_available"` // keep at least this much host MemAvailable
	} `toml:"reserve"`

	Defaults struct {
		CPU float64 `toml:"cpu"`
		Mem string  `toml:"mem"`
	} `toml:"defaults"`

	Storage struct {
		Persistent     *bool `toml:"persistent"`
		MaxSlotsPerKey int   `toml:"max_slots_per_key"`
		Cache          struct {
			Retain string `toml:"retain"`
		} `toml:"cache"`
		Jobs struct {
			Retain string `toml:"retain"`
		} `toml:"jobs"`
		Ledger struct {
			Retain string `toml:"retain"`
		} `toml:"ledger"`
	} `toml:"storage"`

	Container struct {
		Runtime     string `toml:"runtime"`      // "container" (default when podman exists) or "host"
		BuildMem    string `toml:"build_mem"`    // memory limit for environment builds
		ImageRetain string `toml:"image_retain"` // drop environment images unused this long
	} `toml:"container"`

	Policy struct {
		MaxSessions  int    `toml:"max_sessions"`
		SessionGrace string `toml:"session_grace"`
		IOWeight     int    `toml:"io_weight"`
		OOMScoreAdj  int    `toml:"oom_score_adjust"`
		// CPUOvercommit lets guarantees add up to this many times the CPU capacity, for work
		// that starts while its share of cores is really idle (default 2; 1 turns it off).
		CPUOvercommit float64 `toml:"cpu_overcommit"`
		// MemOvercommit lets the memory limits of running work add up to this many times the
		// memory capacity; jobs are booked at their usual peak (default 2; 1 turns it off).
		MemOvercommit float64 `toml:"mem_overcommit"`
	} `toml:"policy"`

	// Derived values.
	memCap       int64
	memReserve   int64
	defMem       int64
	cpus         []int
	cacheRetain  time.Duration
	jobsRetain   time.Duration
	ledgerRetain time.Duration
	sessionGrace time.Duration
	persistent   bool
	buildMem     int64
	imageRetain  time.Duration
}

// Profile is a named job or session recipe (profiles_dir/<name>.toml).
type Profile struct {
	Name        string   `toml:"name"`
	Kind        string   `toml:"kind"`  // "job" (env + resource defaults) or "session"
	Match       []string `toml:"match"` // job profiles: commands (globs) that select it automatically
	Description string   `toml:"description"`
	Command     []string `toml:"command"` // required for sessions
	// Sessions: where to run. Runtime "host" or "container" (default: container if available);
	// Containerfile (relative to the profiles dir) is the environment; Devices are passed through
	// (e.g. /dev/dri); Volumes are "host:container" directories kept between sessions.
	Runtime       string            `toml:"runtime"`
	Containerfile string            `toml:"containerfile"`
	Devices       []string          `toml:"devices"`
	Volumes       []string          `toml:"volumes"`
	NestedSandbox bool              `toml:"nested_sandbox"` // allow bubblewrap/flatpak inside the container
	Network       string            `toml:"network"`        // "client": connections go through the attached client
	Env           map[string]string `toml:"env"`
	Resources     struct {
		CPU    float64 `toml:"cpu"`
		CPUMax float64 `toml:"cpu_max"`
		Mem    string  `toml:"mem"`
	} `toml:"resources"`
}

func expandHome(p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") {
		home, _ := os.UserHomeDir()
		return filepath.Join(home, strings.TrimPrefix(p, "~"))
	}
	return p
}

// expandEnv expands a leading ~ in every colon-separated element.
func expandEnv(v string) string {
	parts := strings.Split(v, ":")
	for i, p := range parts {
		parts[i] = expandHome(p)
	}
	return strings.Join(parts, ":")
}

// DefaultConfigPath is where install writes the runner config.
func DefaultConfigPath() string {
	dir, _ := os.UserConfigDir()
	return filepath.Join(dir, "offload", "runner.toml")
}

// LoadConfig reads and validates a runner config, filling defaults.
func LoadConfig(path string) (*Config, error) {
	var c Config
	if _, err := toml.DecodeFile(path, &c); err != nil {
		return nil, err
	}
	return &c, c.finish()
}

func (c *Config) finish() error {
	var err error
	if c.Name == "" {
		c.Name, _ = os.Hostname()
	}
	if len(c.Listen) == 0 {
		c.Listen = []string{fmt.Sprintf("tailscale:%d", api.DefaultPort), "unix"}
	}
	if c.StateDir == "" {
		c.StateDir = "~/.local/share/offload"
	}
	c.StateDir = expandHome(c.StateDir)
	if c.ProfilesDir == "" {
		dir, _ := os.UserConfigDir()
		c.ProfilesDir = filepath.Join(dir, "offload", "profiles")
	}
	c.ProfilesDir = expandHome(c.ProfilesDir)
	c.TokensFile = expandHome(c.TokensFile)
	if c.Slice == "" {
		c.Slice = "offload.slice"
	}
	for k, v := range c.Env {
		c.Env[k] = expandEnv(v)
	}
	if c.Capacity.CPUs != "" {
		if c.cpus, err = units.ParseCPUList(c.Capacity.CPUs); err != nil {
			return err
		}
	} else {
		for i := 0; i < runtime.NumCPU(); i++ {
			c.cpus = append(c.cpus, i)
		}
	}
	sort.Ints(c.cpus)
	if c.Capacity.CPU <= 0 {
		c.Capacity.CPU = float64(len(c.cpus))
	}
	if c.memCap, err = units.ParseSize(c.Capacity.Mem); err != nil {
		return err
	}
	if c.memCap == 0 {
		c.memCap = memTotal() * 3 / 4
	}
	if c.memReserve, err = units.ParseSize(c.Reserve.MemAvailable); err != nil {
		return err
	}
	if c.Reserve.MemAvailable == "" {
		c.memReserve = 2 << 30
	}
	if c.Defaults.CPU <= 0 {
		c.Defaults.CPU = 2
	}
	if c.defMem, err = units.ParseSize(c.Defaults.Mem); err != nil {
		return err
	}
	if c.defMem == 0 {
		c.defMem = 4 << 30
	}
	c.persistent = c.Storage.Persistent == nil || *c.Storage.Persistent
	if c.Storage.MaxSlotsPerKey <= 0 {
		c.Storage.MaxSlotsPerKey = 3
	}
	if c.cacheRetain, err = durationOr(c.Storage.Cache.Retain, 14*24*time.Hour); err != nil {
		return err
	}
	if c.jobsRetain, err = durationOr(c.Storage.Jobs.Retain, 24*time.Hour); err != nil {
		return err
	}
	if c.ledgerRetain, err = durationOr(c.Storage.Ledger.Retain, 30*24*time.Hour); err != nil {
		return err
	}
	if c.sessionGrace, err = durationOr(c.Policy.SessionGrace, 5*time.Minute); err != nil {
		return err
	}
	if c.buildMem, err = units.ParseSize(c.Container.BuildMem); err != nil {
		return err
	}
	// A build must fit the budget it is counted in: the slice, and what the host can really
	// spare above its reserve (the kernel and services use some of MemTotal; half of what's
	// left over the reserve is a limit an idle host can always admit).
	limit := c.memCap
	if total := meminfo("MemTotal"); total > c.memReserve && (total-c.memReserve)/2 < limit {
		limit = (total - c.memReserve) / 2
	}
	switch {
	case c.buildMem == 0:
		c.buildMem = min(8<<30, limit)
	case c.buildMem > limit:
		log.Printf("container.build_mem %s can never be admitted on this host; using %s", units.FormatSize(c.buildMem), units.FormatSize(limit))
		c.buildMem = limit
	}
	if c.imageRetain, err = durationOr(c.Container.ImageRetain, 14*24*time.Hour); err != nil {
		return err
	}
	if c.Policy.MaxSessions <= 0 {
		c.Policy.MaxSessions = 4
	}
	if c.Policy.IOWeight <= 0 {
		c.Policy.IOWeight = 50
	}
	if c.Policy.CPUOvercommit == 0 {
		c.Policy.CPUOvercommit = 2
	}
	c.Policy.CPUOvercommit = max(c.Policy.CPUOvercommit, 1)
	if c.Policy.MemOvercommit == 0 {
		c.Policy.MemOvercommit = 2
	}
	c.Policy.MemOvercommit = max(c.Policy.MemOvercommit, 1)
	if c.Policy.OOMScoreAdj == 0 {
		c.Policy.OOMScoreAdj = 900
	}
	return nil
}

func durationOr(s string, def time.Duration) (time.Duration, error) {
	if s == "" {
		return def, nil
	}
	return units.ParseDuration(s)
}

// loadProfiles reads every profile in the profiles directory. Edits apply without a restart.
func (c *Config) loadProfiles() map[string]*Profile {
	out := map[string]*Profile{}
	files, _ := filepath.Glob(filepath.Join(c.ProfilesDir, "*.toml"))
	for _, f := range files {
		var p Profile
		if _, err := toml.DecodeFile(f, &p); err != nil {
			continue
		}
		if p.Name == "" {
			p.Name = strings.TrimSuffix(filepath.Base(f), ".toml")
		}
		if p.Kind == "" {
			p.Kind = "job"
			if len(p.Command) > 0 {
				p.Kind = "session"
			}
		}
		for k, v := range p.Env {
			p.Env[k] = expandEnv(v)
		}
		for i, a := range p.Command {
			p.Command[i] = expandHome(a)
		}
		out[p.Name] = &p
	}
	return out
}

func (p *Profile) resources() api.Resources {
	mem, _ := units.ParseSize(p.Resources.Mem)
	return api.Resources{CPU: p.Resources.CPU, CPUMax: p.Resources.CPUMax, Mem: mem}
}

// effective fills a request's zero values from the profile, then the runner defaults, and clamps
// it to what this runner can ever give.
func (c *Config) effective(req api.Resources, p *Profile) api.Resources {
	r := req
	if p != nil {
		pr := p.resources()
		if r.CPU == 0 {
			r.CPU = pr.CPU
			if req.CPUMax > 0 && r.CPU > req.CPUMax {
				r.CPU = req.CPUMax // an inherited guarantee never lifts an explicit ceiling
			}
		}
		if r.CPUMax == 0 {
			r.CPUMax = pr.CPUMax
		}
		if r.Mem == 0 {
			r.Mem = pr.Mem
		}
	}
	if r.CPU <= 0 {
		r.CPU = c.Defaults.CPU
		if req.CPUMax > 0 && r.CPU > req.CPUMax {
			r.CPU = req.CPUMax
		}
	}
	if r.Mem <= 0 {
		r.Mem = c.defMem
	}
	if req.Mem == 0 {
		r.Mem = min(r.Mem, c.memCap) // defaults never exceed what this runner has
	}
	maxCPU := float64(len(c.cpus))
	if r.CPUMax <= 0 {
		r.CPUMax = maxCPU // burst into idle cores by default
	}
	r.CPU = math.Min(r.CPU, c.Capacity.CPU)
	r.CPUMax = math.Min(math.Max(r.CPUMax, r.CPU), maxCPU)
	return r
}
