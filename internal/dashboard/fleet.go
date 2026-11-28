package dashboard

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strconv"
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
	runners  map[string]*runnerState // by runner ID (URL for runners too old to have one)
	lastSeen time.Time               // last time a page asked for the fleet
	found    time.Time               // last discovery
	scanning bool
	wake     chan struct{}
}

type runnerState struct {
	host     *client.Host
	key      string        // its key in fleet.runners: the runner's ID, or its URL
	Name     string        `json:"name"` // unique here: a runner's own name, or name (address) for a second one
	URL      string        `json:"url"`
	Online   bool          `json:"online"`
	Current  bool          `json:"current"` // online, on our protocol, with metrics, ledger and the log viewer
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

// addLocked adds a runner found by discovery, or points a known one at its fresh client. A
// runner reachable at two addresses (pinned in client.toml and found on the tailnet) is one row.
// Discovery lists pinned hosts first, so that address is used. While it isn't working (not
// answering, or answering status but refusing the rest), a usable one discovery just reached
// stands in: paired (it has a token), or the local socket.
func (f *fleet) addLocked(h *client.Host) {
	key := h.URL
	if h.Status != nil && h.Status.ID != "" {
		key = h.Status.ID
		if old := f.runners[h.URL]; old != nil && old.URL == h.URL { // seen before it had an ID
			delete(f.runners, h.URL)
		}
	}
	if r := f.runners[key]; r != nil {
		usable := h.Token != "" || h.URL == "http://local"
		if r.URL == h.URL || ((!r.Online || r.Error != "") && usable) {
			r.host, r.URL = h, h.URL
		}
		return
	}
	// Names identify runners in the page and its requests, so they must be unique: a second
	// runner with a taken name gets its address added (and a number, should that be taken too).
	addr := h.URL
	if u, err := url.Parse(h.URL); err == nil && u.Host != "" {
		addr = u.Host
	}
	name := h.Name
	for i := 1; f.byNameLocked(name) != nil; i++ {
		name = h.Name + " (" + addr + ")"
		if i > 1 {
			name += " " + strconv.Itoa(i)
		}
	}
	f.runners[key] = &runnerState{host: h, key: key, Name: name, URL: h.URL, Status: h.Status, Online: true, LastSeen: time.Now(),
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
		errs     [2]error
	)
	if err := h.Do(ctx, "GET", "/v1/status", nil, &st); err != nil {
		f.mu.Lock()
		if r.host == h { // discovery may have moved it to another address meanwhile
			r.Online, r.Error = false, err.Error()
		}
		f.mu.Unlock()
		return
	}
	if st.ID != "" && st.ID != r.key && r.key != h.URL {
		// This address now answers as another runner (its state was moved or reset): drop the
		// row, and the next discovery lists it under its new ID.
		f.mu.Lock()
		if f.runners[r.key] == r && r.host == h {
			delete(f.runners, r.key)
		}
		f.found = time.Time{} // rediscover on the next round
		f.mu.Unlock()
		f.poke()
		return
	}
	// A runner on another protocol answers status but refuses the rest: show it, with why.
	var werr error
	if st.Proto != api.Proto {
		werr = fmt.Errorf("it speaks protocol %d and this machine %d, so its jobs and sessions can't be shown: run offload host upgrade", st.Proto, api.Proto)
	} else {
		wg.Add(2)
		go func() { defer wg.Done(); errs[0] = h.Do(ctx, "GET", "/v1/jobs", nil, &jobs) }()
		go func() { defer wg.Done(); errs[1] = h.Do(ctx, "GET", "/v1/sessions", nil, &sessions) }()
		if st.Metrics {
			wg.Add(1)
			go func() {
				defer wg.Done()
				var m api.Metrics
				if h.Do(ctx, "GET", "/v1/metrics", nil, &m) == nil {
					met = &m
				}
			}()
		}
		wg.Wait()
		werr = errors.Join(errs[0], errs[1])
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.host != h {
		return // moved to another address while this poll ran: its next poll publishes
	}
	r.Online, r.LastSeen, r.Status, r.Metrics = true, time.Now(), &st, met
	if werr != nil {
		r.Error, r.Jobs, r.Sessions = werr.Error(), []api.Job{}, []api.Session{}
		return
	}
	r.Error = ""
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
		c.Current = r.currentLocked()
		v.Runners = append(v.Runners, c)
	}
	sort.Slice(v.Runners, func(a, b int) bool { return v.Runners[a].Name < v.Runners[b].Name })
	return v
}

func (f *fleet) host(name string) *client.Host {
	h, _ := f.runner(name)
	return h
}

// runner returns a runner's client and whether it is current enough for the log viewer.
func (f *fleet) runner(name string) (h *client.Host, current bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r := f.byNameLocked(name); r != nil {
		return r.host, r.currentLocked()
	}
	return nil, false
}

// currentLocked: the runner answers, on our protocol, and is new enough for metrics, the ledger
// and observe-only log streams (they came together).
func (r *runnerState) currentLocked() bool {
	return r.Online && r.Status != nil && r.Status.Metrics && r.Status.Proto == api.Proto
}

// ledgerHosts lists the runners whose ledger can be read now, and why each other one's can't.
func (f *fleet) ledgerHosts() (map[string]*client.Host, map[string]string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out, missing := map[string]*client.Host{}, map[string]string{}
	for _, r := range f.runners {
		switch {
		case r.currentLocked():
			out[r.Name] = r.host
		case !r.Online:
			missing[r.Name] = "not answering, so its history is left out"
		case r.Status != nil && !r.Status.Metrics:
			// predates the ledger: nothing is missing
		default:
			missing[r.Name] = "its history can't be read: " + r.Error
		}
	}
	return out, missing
}

// setStatus records a status the runner just answered with, so the page shows a change it
// made at once rather than on the next poll.
func (f *fleet) setStatus(name string, h *client.Host, st *api.Status) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r := f.byNameLocked(name); r != nil && r.host == h {
		r.Status = st
	}
}
