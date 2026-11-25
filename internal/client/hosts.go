// Package client implements the offload CLI: finding runners, sending jobs, following output
// and bridging MCP sessions.
package client

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/BurntSushi/toml"

	"github.com/RestartFU/offload/internal/api"
	"github.com/RestartFU/offload/internal/tsapi"
)

// Config is ~/.config/offload/client.toml.
type Config struct {
	Discover *bool        `toml:"discover"` // probe tailnet peers for runners (default true)
	Port     int          `toml:"port"`
	Local    *bool        `toml:"local"` // use a runner on this machine if one is running (default true)
	Hosts    []HostConfig `toml:"host"`
}

// HostConfig pins a runner by URL, optionally with a token and a priority.
type HostConfig struct {
	Name     string `toml:"name"`
	URL      string `toml:"url"`
	Token    string `toml:"token"`
	Priority int    `toml:"priority"` // higher wins; local runners default to -10
	Disabled bool   `toml:"disabled"`
}

// Host is a reachable runner.
type Host struct {
	Name     string
	URL      string
	Token    string
	Priority int
	Status   *api.Status
	http     *http.Client
}

func configDir() string {
	dir, _ := os.UserConfigDir()
	return filepath.Join(dir, "offload")
}

func stateDir() string {
	if d := os.Getenv("XDG_STATE_HOME"); d != "" {
		return filepath.Join(d, "offload")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".local", "state", "offload")
}

// LoadConfig reads the client config; a missing file means defaults.
func LoadConfig() (*Config, error) {
	var c Config
	path := filepath.Join(configDir(), "client.toml")
	if _, err := toml.DecodeFile(path, &c); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if c.Port == 0 {
		c.Port = api.DefaultPort
	}
	return &c, nil
}

func localSocket() string {
	dir := os.Getenv("XDG_RUNTIME_DIR")
	if dir == "" {
		dir = filepath.Join(os.TempDir(), fmt.Sprintf("offload-%d", os.Getuid()))
	}
	return filepath.Join(dir, "offload", "runner.sock")
}

func newHost(name, url, token string, prio int) *Host {
	h := &Host{Name: name, URL: strings.TrimRight(url, "/"), Token: token, Priority: prio}
	if sock, ok := strings.CutPrefix(url, "unix://"); ok {
		h.URL = "http://local"
		h.http = &http.Client{Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, "unix", sock)
			},
		}}
	} else {
		h.http = &http.Client{Transport: &http.Transport{
			DialContext:           (&net.Dialer{Timeout: 5 * time.Second, KeepAlive: 15 * time.Second}).DialContext,
			ResponseHeaderTimeout: 0,
			IdleConnTimeout:       60 * time.Second,
		}}
	}
	return h
}

// Discover returns every reachable runner with its status, best candidates first.
func Discover(ctx context.Context, cfg *Config) []*Host {
	var cands []*Host
	seen := map[string]bool{}
	add := func(h *Host) {
		if !seen[h.URL+h.Name] {
			seen[h.URL+h.Name] = true
			cands = append(cands, h)
		}
	}
	for _, hc := range cfg.Hosts {
		if !hc.Disabled {
			add(newHost(hc.Name, hc.URL, hc.Token, hc.Priority))
		}
	}
	if cfg.Local == nil || *cfg.Local {
		if _, err := os.Stat(localSocket()); err == nil {
			add(newHost("local", "unix://"+localSocket(), "", -10))
		}
	}
	if cfg.Discover == nil || *cfg.Discover {
		tctx, cancel := context.WithTimeout(ctx, 3*time.Second)
		st, err := tsapi.GetStatus(tctx)
		cancel()
		if err == nil {
			for _, p := range st.Peer {
				if !p.Online || len(p.TailscaleIPs) == 0 {
					continue
				}
				name := strings.Split(p.DNSName, ".")[0]
				if name == "" {
					name = p.HostName
				}
				add(newHost(name, fmt.Sprintf("http://%s", net.JoinHostPort(p.TailscaleIPs[0], fmt.Sprint(cfg.Port))), "", 0))
			}
		}
	}
	var wg sync.WaitGroup
	for _, h := range cands {
		wg.Add(1)
		go func(h *Host) {
			defer wg.Done()
			pctx, cancel := context.WithTimeout(ctx, 2*time.Second)
			defer cancel()
			var st api.Status
			if err := h.do(pctx, "GET", "/v1/status", nil, &st); err == nil {
				h.Status = &st
				if st.Name != "" && h.Name != "local" {
					h.Name = st.Name
				}
			}
		}(h)
	}
	wg.Wait()
	var out []*Host
	for _, h := range cands {
		if h.Status != nil {
			out = append(out, h)
		}
	}
	// A local runner and a tailnet runner can be the same machine; keep the first.
	sort.SliceStable(out, func(i, j int) bool { return out[i].Priority > out[j].Priority })
	return out
}

// APIError is a non-2xx response from a runner.
type APIError struct {
	Code int
	Msg  string
}

func (e *APIError) Error() string { return fmt.Sprintf("%d: %s", e.Code, e.Msg) }

func (h *Host) request(ctx context.Context, method, path string, body io.Reader) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, h.URL+path, body)
	if err != nil {
		return nil, err
	}
	if h.Token != "" {
		req.Header.Set("Authorization", "Bearer "+h.Token)
	}
	resp, err := h.http.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 300 {
		defer resp.Body.Close()
		var e api.Error
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
		if json.Unmarshal(b, &e) != nil || e.Error == "" {
			e.Error = strings.TrimSpace(string(b))
		}
		return nil, &APIError{Code: resp.StatusCode, Msg: e.Error}
	}
	return resp, nil
}

// do sends a JSON request (in may be nil, or an io.Reader for raw bodies) and decodes JSON into out.
func (h *Host) do(ctx context.Context, method, path string, in, out any) error {
	var body io.Reader
	switch v := in.(type) {
	case nil:
	case io.Reader:
		body = v
	default:
		b, err := json.Marshal(v)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	resp, err := h.request(ctx, method, path, body)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if out == nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// free returns what the runner could hand out right now.
func (h *Host) free() api.Resources {
	st := h.Status
	return api.Resources{CPU: st.Capacity.CPU - st.Allocated.CPU, Mem: min(st.Capacity.Mem-st.Allocated.Mem, st.MemAvailable-st.MemReserve)}
}

// Pick chooses the best runner for a request: it must be accepting, have the profile and be able
// to fit the job eventually. Free capacity now, a warm workspace and priority decide among them.
func Pick(hosts []*Host, want api.Resources, profile, key, kind string) (*Host, error) {
	var best *Host
	bestScore := -1e18
	var reasons []string
	for _, h := range hosts {
		st := h.Status
		r := want
		if p, ok := st.Profiles[profile]; profile != "" {
			if !ok || (kind != "" && p.Kind != kind) {
				reasons = append(reasons, h.Name+": no "+profile+" profile")
				continue
			}
			if r.CPU == 0 {
				r.CPU = p.Resources.CPU
			}
			if r.Mem == 0 {
				r.Mem = p.Resources.Mem
			}
		}
		if r.CPU == 0 {
			r.CPU = st.Defaults.CPU
		}
		if r.Mem == 0 {
			r.Mem = st.Defaults.Mem
		}
		if !st.Accepting {
			reasons = append(reasons, h.Name+": "+st.Reason)
			continue
		}
		if r.Mem > st.Capacity.Mem {
			reasons = append(reasons, h.Name+": too little memory")
			continue
		}
		f := h.free()
		score := float64(h.Priority) * 1000
		if f.CPU >= r.CPU && f.Mem >= r.Mem {
			score += 500 // can start right away
		}
		if key != "" {
			for _, k := range st.WarmKeys {
				if k == key {
					score += 200
				}
			}
		}
		score += f.CPU - st.Load1/4
		if score > bestScore {
			best, bestScore = h, score
		}
	}
	if best == nil {
		if len(hosts) == 0 {
			return nil, errors.New("no runners found (is tailscale up and a runner online?)")
		}
		return nil, fmt.Errorf("no runner fits: %s", strings.Join(reasons, "; "))
	}
	return best, nil
}

// FindHost returns the named runner from hosts.
func FindHost(hosts []*Host, name string) (*Host, error) {
	for _, h := range hosts {
		if strings.EqualFold(h.Name, name) || (h.Status != nil && strings.EqualFold(h.Status.Hostname, name)) {
			return h, nil
		}
	}
	return nil, fmt.Errorf("runner %q not found", name)
}

// recent remembers which runner holds a job, so `offload logs j-…` works without --on.
type recentJob struct {
	Host string    `json:"host"`
	URL  string    `json:"url"`
	At   time.Time `json:"at"`
}

func recentPath() string { return filepath.Join(stateDir(), "jobs.json") }

func rememberJob(id string, h *Host) {
	m := map[string]recentJob{}
	if b, err := os.ReadFile(recentPath()); err == nil {
		_ = json.Unmarshal(b, &m)
	}
	m[id] = recentJob{Host: h.Name, URL: h.URL, At: time.Now()}
	for k, v := range m {
		if time.Since(v.At) > 7*24*time.Hour {
			delete(m, k)
		}
	}
	_ = os.MkdirAll(stateDir(), 0o700)
	if b, err := json.Marshal(m); err == nil {
		_ = os.WriteFile(recentPath(), b, 0o600)
	}
}

// locateJob finds the runner that holds a job id ("j-…" or "runner:j-…").
func locateJob(ctx context.Context, hosts []*Host, ref string) (*Host, string, error) {
	name, id, ok := strings.Cut(ref, ":")
	if ok {
		h, err := FindHost(hosts, name)
		return h, id, err
	}
	id = ref
	if b, err := os.ReadFile(recentPath()); err == nil {
		m := map[string]recentJob{}
		_ = json.Unmarshal(b, &m)
		if r, ok := m[id]; ok {
			for _, h := range hosts {
				if h.Name == r.Host {
					return h, id, nil
				}
			}
		}
	}
	for _, h := range hosts {
		var j api.Job
		if err := h.do(ctx, "GET", "/v1/jobs/"+id, nil, &j); err == nil {
			return h, id, nil
		}
	}
	return nil, "", fmt.Errorf("job %s not found on any runner", id)
}
