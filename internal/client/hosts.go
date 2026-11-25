// Package client implements the offload CLI: finding runners, sending jobs, following output
// and bridging MCP sessions.
package client

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
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
	if h.sendsToken(path) {
		req.Header.Set("Authorization", "Bearer "+h.Token)
	}
	req.Header.Set(api.ProtoHeader, fmt.Sprint(api.Proto))
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
	sock     string         // unix socket path for a local runner
	derived  bool           // Token is the one stored for this URL by `host pair`, not configured
	named    bool           // Name comes from client.toml
	estimate *api.Resources // what this runner said the job at hand would get (see estimate)
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

// publishOnce returns the content of a small private file, creating it with gen() first if it
// doesn't exist. Creation never replaces a file: racing first runs all end up with one value.
func publishOnce(path string, valid func(string) bool, gen func() (string, error)) (string, error) {
	read := func() (string, bool, error) {
		b, err := os.ReadFile(path)
		if err != nil {
			return "", false, err
		}
		v := strings.TrimSpace(string(b))
		if !valid(v) {
			return "", true, fmt.Errorf("%s is damaged; delete it to start over (runners must then be paired again)", path)
		}
		return v, true, nil
	}
	if v, exists, err := read(); err == nil || exists {
		return v, err
	}
	v, err := gen()
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".publish-*")
	if err != nil {
		return "", err
	}
	defer os.Remove(tmp.Name())
	_, err = tmp.WriteString(v + "\n")
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return "", err
	}
	if err := os.Link(tmp.Name(), path); err != nil && !errors.Is(err, os.ErrExist) {
		return "", err
	}
	v, _, err = read() // ours, or the one that won
	return v, err
}

func randomHex(n int) (string, error) {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}

// clientID is a stable, non-secret identity for this client machine. It namespaces what only
// makes sense per client on a shared runner: data sets, and workspaces of local-path origins.
func clientID() string {
	id, err := publishOnce(filepath.Join(configDir(), "client.id"),
		func(v string) bool { return len(v) == 16 && validHex(v) },
		func() (string, error) { return randomHex(8) })
	if err != nil {
		h, _ := os.Hostname()
		sum := sha256.Sum256([]byte(h))
		return hex.EncodeToString(sum[:8]) // degraded, but stable
	}
	return id
}

func validHex(s string) bool {
	_, err := hex.DecodeString(s)
	return err == nil
}

// runnerTokenPath stores this client's token for one runner (a fresh random one per pairing).
func runnerTokenPath(url string) string {
	sum := sha256.Sum256([]byte(url))
	return filepath.Join(configDir(), "runner-tokens", hex.EncodeToString(sum[:16]))
}

func storedToken(url string) string {
	b, err := os.ReadFile(runnerTokenPath(url))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// legacyToken is the token older clients derived from client.token; only `host pair` uses it,
// to authenticate once and replace it with a stored random token.
func legacyToken(url string) string {
	b, err := os.ReadFile(filepath.Join(configDir(), "client.token"))
	secret := strings.TrimSpace(string(b))
	if err != nil || len(secret) < 32 {
		return ""
	}
	m := hmac.New(sha256.New, []byte(secret))
	m.Write([]byte("offload-runner\x00" + url))
	return hex.EncodeToString(m.Sum(nil))
}

// sendsToken says whether a request carries the token. A discovery probe of a peer that may not
// be a runner at all never carries one.
func (h *Host) sendsToken(path string) bool {
	return h.Token != "" && !(h.derived && path == "/v1/status")
}

func newHost(name, url, token string, prio int) *Host {
	url = strings.TrimRight(url, "/")
	derived := false
	if token == "" && !strings.HasPrefix(url, "unix://") {
		token, derived = storedToken(url), true
	}
	h := &Host{Name: name, URL: url, Token: token, Priority: prio, derived: derived}
	if sock, ok := strings.CutPrefix(url, "unix://"); ok {
		h.URL, h.sock = "http://local", sock
		h.http = &http.Client{CheckRedirect: noRedirects, Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, "unix", sock)
			},
			IdleConnTimeout: 60 * time.Second,
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
			fh := newHost(hc.Name, hc.URL, hc.Token, hc.Priority)
			fh.named = hc.Name != ""
			fixed = append(fixed, fh)
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
			h.Status = nil // only what this probe sees counts
			if err := h.do(pctx, "GET", "/v1/status", nil, &st); err == nil {
				h.Status = &st
				if st.Name != "" && h.Name != "local" && !h.named {
					h.Name = st.Name // a name from client.toml stays; the advertised one also finds it
				}
			}
		}(h)
	}
	wg.Wait()
	for _, h := range cands { // probes are one-off: don't keep their connections around
		h.http.CloseIdleConnections()
	}
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
	if h.sendsToken(path) {
		req.Header.Set("Authorization", "Bearer "+h.Token)
	}
	req.Header.Set(api.ProtoHeader, fmt.Sprint(api.Proto))
	resp, err := h.http.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 300 {
		defer resp.Body.Close()
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
		return nil, &APIError{Code: resp.StatusCode, Msg: apiMessage(b)}
	}
	return resp, nil
}

// apiMessage is the error message of a runner's error body.
func apiMessage(b []byte) string {
	var e api.Error
	if json.Unmarshal(b, &e) != nil || e.Error == "" {
		return strings.TrimSpace(string(b))
	}
	return e.Error
}

// Request sends a request to the runner. A non-2xx answer comes back as an *APIError.
func (h *Host) Request(ctx context.Context, method, path string, body io.Reader) (*http.Response, error) {
	return h.request(ctx, method, path, body)
}

// Do sends a JSON request (in may be nil) and decodes the JSON answer into out (may be nil).
func (h *Host) Do(ctx context.Context, method, path string, in, out any) error {
	return h.do(ctx, method, path, in, out)
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
	// Jobs still uploading or queued have claimed their share: two submits a few seconds apart
	// must not both see the same idle runner.
	used := api.Resources{CPU: st.Allocated.CPU + st.Pending.CPU, Mem: st.Allocated.Mem + st.Pending.Mem}
	cpu := st.Capacity.CPU - used.CPU
	if st.CPUOvercommit > 1 {
		// As the runner admits: past capacity, up to the overbooking limit while cores are idle.
		cpu = max(cpu, min(st.Capacity.CPU*st.CPUOvercommit-used.CPU, st.CPUIdle-st.Pending.CPU))
	}
	return api.Resources{CPU: cpu, Mem: min(st.Capacity.Mem-used.Mem, st.MemAvailable-st.MemReserve-st.Pending.Mem)}
}

// PickFor chooses a runner for a job spec: like Pick, plus platform and runtime constraints.
// Without an explicit platform, runners matching this machine's OS/arch are preferred (files
// fetched with --get then run here).
func PickFor(hosts []*Host, spec api.JobSpec, kind string) (*Host, error) {
	ok, why := eligible(hosts, spec)
	if len(ok) == 0 && len(hosts) > 0 {
		return nil, fmt.Errorf("no runner fits: %s", strings.Join(why, "; "))
	}
	if kind == "job" && len(ok) > 1 {
		estimate(ok, spec)
	}
	return pick(ok, spec.Resources, spec.Profile, spec.WorkspaceKey, kind, spec.Argv)
}

// estimate asks each candidate what the job would get there (including memory it learned from
// earlier runs of this command in this workspace), in parallel and briefly; pick then scores
// with that instead of guessing from defaults.
func estimate(hosts []*Host, spec api.JobSpec) {
	q := spec
	q.Manifest, q.Secrets, q.SecretFiles = nil, nil, nil
	var wg sync.WaitGroup
	for _, h := range hosts {
		wg.Add(1)
		go func(h *Host) {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			var r api.Resources
			if err := h.do(ctx, "POST", "/v1/estimate", q, &r); err == nil && r.Mem > 0 {
				h.estimate = &r
			}
		}(h)
	}
	wg.Wait()
}

// eligible keeps the runners that can run spec at all (platform, container runtime), with the
// reasons the others can't.
func eligible(hosts []*Host, spec api.JobSpec) ([]*Host, []string) {
	var ok []*Host
	var why []string
	needContainer := spec.Runtime != "host" && (spec.Runtime == "container" || spec.Environment != nil || spec.Network == "client")
	for _, h := range hosts {
		st := h.Status
		plat := st.OS + "/" + st.Arch
		switch {
		case spec.Platform != "" && plat != spec.Platform:
			why = append(why, h.Name+": is "+plat)
		case needContainer && st.Runtime != "container":
			why = append(why, h.Name+": no container runtime")
		default:
			ok = append(ok, h)
		}
	}
	return ok, why
}

// Pick chooses the best runner for a request: it must be accepting, have the profile and be able
// to fit the job eventually. Free capacity now, a warm workspace and priority decide among them.
func Pick(hosts []*Host, want api.Resources, profile, key, kind string) (*Host, error) {
	return pick(hosts, want, profile, key, kind, nil)
}

// pick is Pick; with argv and no profile, each runner is judged by the profile it would match
// for that command (as it will when it gets the job), not by its global defaults.
func pick(hosts []*Host, want api.Resources, profile, key, kind string, argv []string) (*Host, error) {
	var best *Host
	bestScore := -1e18
	var reasons []string
	for _, h := range hosts {
		if err := checkProto(h); err != nil {
			reasons = append(reasons, err.Error())
			continue
		}
		if h.derived && h.Token == "" { // discovered, but this client can't use it yet
			reasons = append(reasons, h.Name+": not paired (offload host pair "+h.Name+" --code CODE)")
			continue
		}
		st := h.Status
		r := want
		profile := profile
		if profile == "" && len(argv) > 0 {
			profile = api.MatchProfile(st.Profiles, argv)
		}
		if h.estimate != nil { // the runner's own answer beats any guess from its defaults
			if r.CPU == 0 {
				r.CPU = h.estimate.CPU
			}
			if r.Mem == 0 {
				r.Mem = h.estimate.Mem
			}
		}
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
		if want.CPU == 0 && want.CPUMax > 0 && r.CPU > want.CPUMax {
			r.CPU = want.CPUMax // as the runner will: an inherited guarantee never exceeds --cpu-max
		}
		if r.Mem == 0 {
			r.Mem = st.Defaults.Mem
		}
		// A job may queue on a runner briefly below its memory reserve (admission waits for
		// memory); sessions start at once, so they need one that is accepting.
		if !st.Accepting && !(kind == "job" && st.Reason == "host memory below reserve") {
			reasons = append(reasons, h.Name+": "+st.Reason)
			continue
		}
		if want.Mem > st.Capacity.Mem { // only an explicit request can rule a runner out
			reasons = append(reasons, h.Name+": too little memory")
			continue
		}
		if want.CPU > st.Capacity.CPU+1e-9 { // an explicit guarantee is never quietly shrunk
			reasons = append(reasons, fmt.Sprintf("%s: guarantees at most %.0f cpu", h.Name, st.Capacity.CPU))
			continue
		}
		r.Mem = min(r.Mem, st.Capacity.Mem)
		f := h.free()
		score := float64(h.Priority) * 1000
		unbooked := st.Capacity.CPU - st.Allocated.CPU - st.Pending.CPU
		if f.CPU >= r.CPU && f.Mem >= r.Mem {
			score += 400 // can start right away
			if unbooked >= r.CPU {
				score += 100 // without overbooking: its guarantee really is free there
			}
		}
		// A warm workspace saves the upload and a cold build: worth one bonus, however many
		// image variants have a slot there (it used to count per slot and pinned every repo
		// to the runner that ran it first).
		if key != "" {
			for _, k := range st.WarmKeys {
				if k == key || strings.HasPrefix(k, key+"--") {
					score += 150
					break
				}
			}
		}
		// Then how much CPU the job would really get: builds use every core they're allowed,
		// so idle cores up to its ceiling are worth more than capacity on paper.
		idle := st.CPUIdle
		if st.CPUOvercommit == 0 { // an older runner: estimate from load
			idle = st.Capacity.CPU - st.Load1
		}
		idle -= st.Pending.CPU
		ceiling := r.CPUMax
		if ceiling <= 0 {
			ceiling = max(st.Capacity.CPUMax, st.Capacity.CPU)
		}
		score += 8*min(max(idle, 0), ceiling) + min(f.CPU, max(unbooked, 0)+f.CPU/4)/4
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

// RefName is how to name h in a job reference so FindHost finds h again: its name if that is
// unambiguous, its runner ID otherwise.
func RefName(hosts []*Host, h *Host) string {
	if f, err := FindHost(hosts, h.Name); err == nil && f.URL == h.URL {
		return h.Name
	}
	if h.Status != nil && h.Status.ID != "" {
		return h.Status.ID
	}
	return h.Name
}

func FindHost(hosts []*Host, name string) (*Host, error) {
	// A name from client.toml is the user's own: it wins, if it's unique.
	var named []*Host
	for _, h := range hosts {
		if h.named && strings.EqualFold(h.Name, name) {
			named = append(named, h)
		}
	}
	if len(named) == 1 {
		return named[0], nil
	}
	// Otherwise every runner that answers to it (its advertised name or hostname), one entry
	// per runner ID (a runner listed under two addresses), preferring a pinned entry.
	byID := map[string]*Host{}
	var order []string
	for _, h := range hosts {
		if h.Status == nil {
			continue
		}
		if !strings.EqualFold(h.Name, name) && !strings.EqualFold(h.Status.Name, name) && !strings.EqualFold(h.Status.Hostname, name) &&
			!(h.Status.ID != "" && strings.EqualFold(h.Status.ID, name)) {
			continue
		}
		k := h.Status.ID
		if k == "" {
			k = h.URL
		}
		if cur, ok := byID[k]; !ok {
			byID[k] = h
			order = append(order, k)
		} else if h.named && !cur.named {
			byID[k] = h
		}
	}
	switch len(order) {
	case 0:
		return nil, fmt.Errorf("runner %q not found", name)
	case 1:
		return byID[order[0]], nil
	}
	var names []string
	for _, k := range order {
		h := byID[k]
		names = append(names, h.URL)
	}
	return nil, fmt.Errorf("%q matches several runners (%s): name them in client.toml ([[host]] name = …)", name, strings.Join(names, ", "))
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
	h, id, err := locateJobIn(ctx, hosts, ref)
	if err != nil { // the runner may be back since discovery was cached: look again, once
		forgetHosts()
		if cfg, cerr := LoadConfig(); cerr == nil {
			return locateJobIn(ctx, Discover(ctx, cfg), ref)
		}
	}
	return h, id, err
}

func locateJobIn(ctx context.Context, hosts []*Host, ref string) (*Host, string, error) {
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
				if h.URL == r.URL { // the address it ran at, not a name another runner may now carry
					return h, id, nil
				}
			}
		}
	}
	kind := "jobs"
	if strings.HasPrefix(id, "s-") {
		kind = "sessions"
	}
	for _, h := range hosts {
		if err := h.do(ctx, "GET", "/v1/"+kind+"/"+id, nil, nil); err == nil {
			return h, id, nil
		}
	}
	return nil, "", fmt.Errorf("%s not found on any runner", id)
}
