package dashboard

import (
	"context"
	"net/url"
	"sort"
	"sync"
	"time"

	"github.com/RestartFU/offload/internal/api"
	"github.com/RestartFU/offload/internal/client"
)

// The fleet poller keeps the latest status, usage, jobs and sessions of every runner. It
// polls every two seconds while a page is open and every thirty when none has asked lately,
// and looks for new runners every half minute (every five seconds while it knows none).

const (
	pollActive   = 2 * time.Second
	pollIdle     = 30 * time.Second
	rediscover   = 30 * time.Second
	watchTimeout = 20 * time.Second
)

type fleet struct {
	cfg *client.Config

	mu       sync.Mutex
	runners  map[string]*runnerState // by URL: two runners may report the same name
	lastSeen time.Time               // last time a page asked for the fleet
	found    time.Time               // last discovery
	scanning bool
	wake     chan struct{}
}

type runnerState struct {
	host     *client.Host
	Name     string        `json:"name"` // unique here: a runner's own name, or name (address) for a second one
	URL      string        `json:"url"`
	Online   bool          `json:"online"`
	Error    string        `json:"error,omitempty"`
	LastSeen time.Time     `json:"last_seen"`
	Status   *api.Status   `json:"status,omitempty"`
	Metrics  *api.Metrics  `json:"metrics,omitempty"`
	Jobs     []api.Job     `json:"jobs"`
	Sessions []api.Session `json:"sessions"`
}

func newFleet(cfg *client.Config) *fleet {
	return &fleet{cfg: cfg, runners: map[string]*runnerState{}, wake: make(chan struct{}, 1), lastSeen: time.Now()}
}

func (f *fleet) watched() {
	f.mu.Lock()
	idle := time.Since(f.lastSeen) > watchTimeout
	f.lastSeen = time.Now()
	f.mu.Unlock()
	if idle {
		f.poke() // back from idle: refresh now rather than in up to thirty seconds
	}
}

func (f *fleet) poke() {
	select {
	case f.wake <- struct{}{}:
	default:
	}
}

func (f *fleet) run(ctx context.Context) {
	f.discover(ctx)
	for {
		f.poll(ctx)
		f.mu.Lock()
		every := pollActive
		if time.Since(f.lastSeen) > watchTimeout {
			every = pollIdle
		}
		again := rediscover
		if len(f.runners) == 0 {
			again = 5 * time.Second // nothing found yet (tailnet still coming up?): look again soon
		}
		stale := time.Since(f.found) > again && !f.scanning
		if stale {
			f.scanning = true
		}
		f.mu.Unlock()
		if stale {
			go f.discover(ctx) // a full tailnet scan can take seconds; polling goes on meanwhile
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(every):
		case <-f.wake:
		}
	}
}

// discover adds runners that answer; known ones stay listed (offline) when they stop answering.
func (f *fleet) discover(ctx context.Context) {
	dctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	hosts := client.Discover(dctx, f.cfg)
	cancel()
	f.mu.Lock()
	defer f.mu.Unlock()
	f.found, f.scanning = time.Now(), false
	for _, h := range hosts {
		f.addLocked(h)
	}
}

// addLocked adds a runner found by discovery, or points a known one at its fresh client.
func (f *fleet) addLocked(h *client.Host) {
	if r := f.runners[h.URL]; r != nil {
		r.host = h
		return
	}
	name := h.Name
	if f.byNameLocked(name) != nil { // another runner already has it: tell them apart by address
		addr := h.URL
		if u, err := url.Parse(h.URL); err == nil && u.Host != "" {
			addr = u.Host
		}
		name += " (" + addr + ")"
	}
	f.runners[h.URL] = &runnerState{host: h, Name: name, URL: h.URL, Status: h.Status, Online: true, LastSeen: time.Now(),
		Jobs: []api.Job{}, Sessions: []api.Session{}}
}

func (f *fleet) byNameLocked(name string) *runnerState {
	for _, r := range f.runners {
		if r.Name == name {
			return r
		}
	}
	return nil
}

func (f *fleet) poll(ctx context.Context) {
	f.mu.Lock()
	var list []*runnerState
	for _, r := range f.runners {
		list = append(list, r)
	}
	f.mu.Unlock()
	var wg sync.WaitGroup
	for _, r := range list {
		wg.Add(1)
		go func() {
			defer wg.Done()
			f.pollOne(ctx, r)
		}()
	}
	wg.Wait()
}

func (f *fleet) pollOne(ctx context.Context, r *runnerState) {
	f.mu.Lock()
	h := r.host
	f.mu.Unlock()
	ctx, cancel := context.WithTimeout(ctx, 4*time.Second)
	defer cancel()
	var (
		st       api.Status
		met      *api.Metrics
		jobs     []api.Job
		sessions []api.Session
		wg       sync.WaitGroup
		errs     [4]error
	)
	wg.Add(3)
	go func() { defer wg.Done(); errs[0] = h.Do(ctx, "GET", "/v1/status", nil, &st) }()
	go func() { defer wg.Done(); errs[1] = h.Do(ctx, "GET", "/v1/jobs", nil, &jobs) }()
	go func() { defer wg.Done(); errs[2] = h.Do(ctx, "GET", "/v1/sessions", nil, &sessions) }()
	wg.Wait()
	if errs[0] == nil && st.Metrics {
		var m api.Metrics
		if err := h.Do(ctx, "GET", "/v1/metrics", nil, &m); err == nil {
			met = &m
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, err := range errs {
		if err != nil {
			r.Online, r.Error = false, err.Error()
			return
		}
	}
	r.Online, r.Error, r.LastSeen = true, "", time.Now()
	r.Status, r.Metrics = &st, met
	if jobs == nil {
		jobs = []api.Job{}
	}
	if sessions == nil {
		sessions = []api.Session{}
	}
	r.Jobs, r.Sessions = jobs, sessions
}

// fleetView is the body of GET /api/fleet.
type fleetView struct {
	Version string        `json:"version"`
	At      time.Time     `json:"at"`
	Runners []runnerState `json:"runners"`
}

func (f *fleet) snapshot(version string) fleetView {
	f.mu.Lock()
	defer f.mu.Unlock()
	v := fleetView{Version: version, At: time.Now(), Runners: []runnerState{}}
	for _, r := range f.runners {
		c := *r
		c.host = nil
		v.Runners = append(v.Runners, c)
	}
	sort.Slice(v.Runners, func(a, b int) bool { return v.Runners[a].Name < v.Runners[b].Name })
	return v
}

func (f *fleet) host(name string) *client.Host {
	h, _ := f.runner(name)
	return h
}

// runner returns a runner's client and whether it is new enough for the dashboard's log
// viewer: observe-only session streams and tailing came with usage metrics.
func (f *fleet) runner(name string) (h *client.Host, current bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r := f.byNameLocked(name); r != nil {
		return r.host, r.Status != nil && r.Status.Metrics
	}
	return nil, false
}

// ledgerHosts lists online runners that keep a ledger.
func (f *fleet) ledgerHosts() map[string]*client.Host {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := map[string]*client.Host{}
	for _, r := range f.runners {
		if r.Online && r.Status != nil && r.Status.Metrics {
			out[r.Name] = r.host
		}
	}
	return out
}
