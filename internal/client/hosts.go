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
	goruntime "runtime"
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
	Discover *bool `toml:"discover"` // probe tailnet peers for runners (default true)
	// TrustTags are tailnet tags whose devices may receive work. Untagged devices of your own
	// tailnet user are always trusted; everyone else's never are.
	TrustTags []string     `toml:"trust_tags"`
	Port      int          `toml:"port"`
	Local     *bool        `toml:"local"` // use a runner on this machine if one is running (default true)
	Hosts     []HostConfig `toml:"host"`

	Credentials CredentialsConfig        `toml:"credentials"`
	Data        map[string]DataSetConfig `toml:"data"`
}

func encodeJSON(w io.Writer, v any) error { return json.NewEncoder(w).Encode(v) }

func newRequest(ctx context.Context, h *Host, method, path string, body io.Reader) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, method, h.URL+path, body)
	if err != nil {
		return nil, err
	}
	if h.Token != "" {
		req.Header.Set("Authorization", "Bearer "+h.Token)
	}
	return req, nil
}

func decodeJSON(r io.Reader, v any) error { return json.NewDecoder(r).Decode(v) }

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
	sock     string // unix socket path for a local runner
}

// dialRaw opens a plain connection to the runner (for stream upgrades).
func (h *Host) dialRaw(ctx context.Context) (net.Conn, error) {
	var d net.Dialer
	d.Timeout = 10 * time.Second
	if h.sock != "" {
		return d.DialContext(ctx, "unix", h.sock)
	}
	return d.DialContext(ctx, "tcp", strings.TrimPrefix(h.URL, "http://"))
}

func (h *Host) hostHeader() string {
	if h.sock != "" {
		return "local"
	}
	return strings.TrimPrefix(h.URL, "http://")
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
	return filepath.Join(dir, "offload-admin", "runner.sock")
}

// noRedirects keeps requests (whose bodies can carry source and secrets) on the runner that was
// vetted: a redirect is returned as an error, never followed.
func noRedirects(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }

func newHost(name, url, token string, prio int) *Host {
	h := &Host{Name: name, URL: strings.TrimRight(url, "/"), Token: token, Priority: prio}
	if sock, ok := strings.CutPrefix(url, "unix://"); ok {
		h.URL, h.sock = "http://local", sock
		h.http = &http.Client{CheckRedirect: noRedirects, Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, "unix", sock)
			},
		}}
	} else {
		h.http = &http.Client{CheckRedirect: noRedirects, Transport: &http.Transport{
			DialContext:           (&net.Dialer{Timeout: 5 * time.Second, KeepAlive: 15 * time.Second}).DialContext,
			ResponseHeaderTimeout: 0,
			IdleConnTimeout:       60 * time.Second,
		}}
	}
	return h
}

// Runner discovery is cached so a command starts immediately: known runners are probed
// directly, and the whole tailnet is scanned only when the cache is old or none of them answer.
type hostCache struct {
	At    time.Time    `json:"at"`
	Hosts []cachedHost `json:"hosts"`
}

type cachedHost struct {
	Name string `json:"name"`
	URL  string `json:"url"`
}

const hostCacheTTL = 2 * time.Minute

func hostCachePath() string {
	dir, _ := os.UserCacheDir()
	return filepath.Join(dir, "offload", "runners.json")
}

// forgetHosts drops the runner cache so the next command rescans the tailnet.
func forgetHosts() { _ = os.Remove(hostCachePath()) }

func loadHostCache() *hostCache {
	b, err := os.ReadFile(hostCachePath())
	if err != nil {
		return nil
	}
	var c hostCache
	if json.Unmarshal(b, &c) != nil || time.Since(c.At) > hostCacheTTL {
		return nil
	}
	return &c
}

func saveHostCache(hosts []*Host) {
	c := hostCache{At: time.Now()}
	for _, h := range hosts {
		c.Hosts = append(c.Hosts, cachedHost{Name: h.Name, URL: h.URL})
	}
	b, _ := json.Marshal(c)
	_ = os.MkdirAll(filepath.Dir(hostCachePath()), 0o700)
	_ = os.WriteFile(hostCachePath(), b, 0o600)
}

// trustedPeers lists online tailnet peers that may receive work: devices of this node's own
// user, plus tagged devices whose tags are in trust_tags. Another user's device that happens to
// answer on the runner port never gets source or secrets.
func trustedPeers(ctx context.Context, cfg *Config) map[string]string {
	tctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	st, err := tsapi.GetStatus(tctx)
	cancel()
	out := map[string]string{} // tailscale IP -> name
	if err != nil || st.Self == nil {
		return out
	}
	for _, p := range st.Peer {
		if !p.Online || len(p.TailscaleIPs) == 0 {
			continue
		}
		trusted := len(p.Tags) == 0 && p.UserID == st.Self.UserID
		for _, t := range p.Tags {
			for _, want := range cfg.TrustTags {
				trusted = trusted || t == want
			}
		}
		if !trusted {
			continue
		}
		name := strings.Split(p.DNSName, ".")[0]
		if name == "" {
			name = p.HostName
		}
		out[p.TailscaleIPs[0]] = name
	}
	return out
}

func hostIP(u string) string {
	host, _, err := net.SplitHostPort(strings.TrimPrefix(u, "http://"))
	if err != nil {
		return ""
	}
	return strings.Trim(host, "[]")
}

// Discover returns every reachable runner with its status, highest priority first. Explicitly
// configured hosts are always candidates; tailnet runners only when trustedPeers allows them.
func Discover(ctx context.Context, cfg *Config) []*Host {
	var fixed []*Host
	for _, hc := range cfg.Hosts {
		if !hc.Disabled {
			fixed = append(fixed, newHost(hc.Name, hc.URL, hc.Token, hc.Priority))
		}
	}
	if cfg.Local == nil || *cfg.Local {
		if _, err := os.Stat(localSocket()); err == nil {
			fixed = append(fixed, newHost("local", "unix://"+localSocket(), "", -10))
		}
	}
	scan := cfg.Discover == nil || *cfg.Discover
	var trusted map[string]string
	if scan {
		trusted = trustedPeers(ctx, cfg)
		if c := loadHostCache(); c != nil {
			var cached []*Host
			for _, ch := range c.Hosts {
				if _, ok := trusted[hostIP(ch.URL)]; ok { // re-checked every time, not just when cached
					cached = append(cached, newHost(ch.Name, ch.URL, "", 0))
				}
			}
			found := probe(ctx, append(append([]*Host(nil), fixed...), cached...))
			for _, h := range found {
				if h.Priority == 0 && !isFixed(h, fixed) {
					return found // at least one cached tailnet runner answered
				}
			}
		}
	}
	cands := append([]*Host(nil), fixed...)
	for ip, name := range trusted {
		cands = append(cands, newHost(name, fmt.Sprintf("http://%s", net.JoinHostPort(ip, fmt.Sprint(cfg.Port))), "", 0))
	}
	found := probe(ctx, cands)
	if scan {
		var tailnet []*Host
		for _, h := range found {
			if !isFixed(h, fixed) {
				tailnet = append(tailnet, h)
			}
		}
		saveHostCache(tailnet)
	}
	return found
}

func isFixed(h *Host, fixed []*Host) bool {
	for _, f := range fixed {
		if f == h {
			return true
		}
	}
	return false
}

// probe fetches every candidate's status in parallel and keeps the ones that answered.
func probe(ctx context.Context, cands []*Host) []*Host {
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
	seen := map[string]bool{}
	for _, h := range cands {
		if h.Status != nil && !seen[h.URL] {
			seen[h.URL] = true
			out = append(out, h)
		}
	}
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

// PickFor chooses a runner for a job spec: like Pick, plus platform and runtime constraints.
// Without an explicit platform, runners matching this machine's OS/arch are preferred (files
// fetched with --get then run here).
func PickFor(hosts []*Host, spec api.JobSpec, kind string) (*Host, error) {
	var ok []*Host
	var why []string
	for _, h := range hosts {
		st := h.Status
		plat := st.OS + "/" + st.Arch
		switch {
		case spec.Platform != "" && plat != spec.Platform:
			why = append(why, h.Name+": is "+plat)
		case spec.Environment != nil && st.Runtime != "container":
			why = append(why, h.Name+": no container runtime")
		default:
			ok = append(ok, h)
		}
	}
	if len(ok) == 0 && len(hosts) > 0 {
		return nil, fmt.Errorf("no runner fits: %s", strings.Join(why, "; "))
	}
	return Pick(ok, spec.Resources, spec.Profile, spec.WorkspaceKey, kind)
}

// Pick chooses the best runner for a request: it must be accepting, have the profile and be able
// to fit the job eventually. Free capacity now, a warm workspace and priority decide among them.
func Pick(hosts []*Host, want api.Resources, profile, key, kind string) (*Host, error) {
	var best *Host
	bestScore := -1e18
	var reasons []string
	for _, h := range hosts {
		if err := checkProto(h); err != nil {
			reasons = append(reasons, err.Error())
			continue
		}
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
		if want.Mem > st.Capacity.Mem { // only an explicit request can rule a runner out
			reasons = append(reasons, h.Name+": too little memory")
			continue
		}
		r.Mem = min(r.Mem, st.Capacity.Mem)
		f := h.free()
		score := float64(h.Priority) * 1000
		if f.CPU >= r.CPU && f.Mem >= r.Mem {
			score += 500 // can start right away
		}
		if key != "" {
			for _, k := range st.WarmKeys {
				if k == key || strings.HasPrefix(k, key+"--") {
					score += 200
				}
			}
		}
		score += f.CPU - st.Load1/4
		if st.OS == goruntime.GOOS && st.Arch == goruntime.GOARCH {
			score += 50
		}
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
// checkProto refuses a runner of another protocol version: mixing versions is never safe.
func checkProto(h *Host) error {
	if h.Status != nil && h.Status.Proto != api.Proto {
		return fmt.Errorf("runner %s speaks protocol %d, this client %d: run `offload host upgrade %s`", h.Name, h.Status.Proto, api.Proto, h.Name)
	}
	return nil
}

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
