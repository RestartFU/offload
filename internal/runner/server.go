// Package runner implements the offload runner daemon: an HTTP API that runs jobs and stdio
// sessions as resource-limited systemd user units on this machine.
package runner

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/RestartFU/offload/internal/api"
	"github.com/RestartFU/offload/internal/tsapi"
)

// Server is a running runner.
type Server struct {
	cfg     *Config
	version string
	self    string // path to this executable, used as the unit wrapper

	mu       sync.Mutex
	jobs     map[string]*job
	sessions map[string]*session
	slots    map[string][]*slot
	cpuUse   map[int]int
	features map[string]bool
	history  map[string]*histEntry
	wake     chan struct{}

	tz string

	builds      map[string]*envBuild // image tag -> build in progress
	buildErrs   map[string]error     // last failed build per tag, reported to the next session
	readyImages map[string]bool
	imageUse    map[string]time.Time

	startedAt time.Time
	upgrading bool   // a new binary is being swapped in: start nothing
	retireErr string // set while workloads of another version couldn't be confirmed stopped

	whoisMu sync.Mutex
	whois   map[string]whoisEntry

	ownNames selfNames // our own DNS names, for the Host check
}

type whoisEntry struct {
	ok  bool
	at  time.Time
	who string
}

// New prepares a runner from its config and restores state left by a previous run.
func New(cfg *Config, version string) (*Server, error) {
	self, err := os.Executable()
	if err != nil {
		return nil, err
	}
	s := &Server{
		cfg: cfg, version: version, self: self, startedAt: time.Now(),
		jobs: map[string]*job{}, sessions: map[string]*session{}, slots: map[string][]*slot{},
		cpuUse: map[int]int{}, wake: make(chan struct{}, 1), whois: map[string]whoisEntry{},
		builds: map[string]*envBuild{}, buildErrs: map[string]error{}, readyImages: map[string]bool{}, tz: hostTimezone(),
	}
	ctrl := delegatedControllers()
	s.features = map[string]bool{"cpuset": ctrl["cpuset"], "io": ctrl["io"], "memory": ctrl["memory"], "cpu": ctrl["cpu"]}
	for _, d := range []string{"jobs", "sessions", "work"} {
		if err := os.MkdirAll(filepath.Join(cfg.StateDir, d), 0o700); err != nil {
			return nil, err
		}
	}
	s.loadHistory()
	s.loadImageUse()
	s.resetCachesIfOld()
	s.restore()
	if err := s.retireOtherVersions(); err != nil {
		log.Printf("retiring old workloads: %v (admitting nothing until this succeeds)", err)
		s.retireErr = err.Error()
	}
	return s, nil
}

func (s *Server) kick() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// Serve listens on every configured address and runs the scheduler until ctx ends.
func (s *Server) Serve(ctx context.Context) error {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/status", s.handleStatus)
	mux.HandleFunc("GET /v1/jobs", s.handleListJobs)
	mux.HandleFunc("POST /v1/jobs", s.handleCreateJob)
	mux.HandleFunc("GET /v1/jobs/{id}", s.handleGetJob)
	mux.HandleFunc("PUT /v1/jobs/{id}/source", s.handleUpload)
	mux.HandleFunc("PUT /v1/jobs/{id}/git", s.handleGitBundle)
	mux.HandleFunc("GET /v1/jobs/{id}/logs", s.handleJobLogs)
	mux.HandleFunc("GET /v1/jobs/{id}/artifacts", s.handleArtifacts)
	mux.HandleFunc("GET /v1/jobs/{id}/writeback", s.handleWriteBack)
	mux.HandleFunc("DELETE /v1/jobs/{id}", s.handleCancelJob)
	mux.HandleFunc("GET /v1/jobs/{id}/callbacks", s.handleCallbacks)
	mux.HandleFunc("POST /v1/jobs/{id}/callbacks/{cid}", s.handleCallbackReply)
	mux.HandleFunc("POST /v1/jobs/{id}/credential", s.handleCredential)
	mux.HandleFunc("POST /v1/jobs/{id}/dial", s.handleDial)
	mux.HandleFunc("GET /v1/sessions/{id}/callbacks", s.handleCallbacks)
	mux.HandleFunc("POST /v1/sessions/{id}/callbacks/{cid}", s.handleCallbackReply)
	mux.HandleFunc("POST /v1/sessions/{id}/dial", s.handleDial)
	mux.HandleFunc("POST /v1/streams/{sid}", s.handleStream)
	mux.HandleFunc("GET /v1/sessions", s.handleListSessions)
	mux.HandleFunc("POST /v1/sessions", s.handleCreateSession)
	mux.HandleFunc("GET /v1/sessions/{id}", s.handleGetSession)
	mux.HandleFunc("POST /v1/sessions/{id}/stdin", s.handleSessionStdin)
	mux.HandleFunc("GET /v1/sessions/{id}/stdout", s.handleSessionStdout)
	mux.HandleFunc("GET /v1/sessions/{id}/stderr", s.handleSessionStderr)
	mux.HandleFunc("DELETE /v1/sessions/{id}", s.handleStopSession)
	mux.HandleFunc("POST /v1/data/{name}", s.handleDataPlan)
	mux.HandleFunc("PUT /v1/data/{name}", s.handleDataUpload)
	mux.HandleFunc("PUT /v1/admin/binary", s.handleUpgrade)
	mux.HandleFunc("POST /v1/admin/pair", s.handlePair)

	handler := s.auth(mux)
	errc := make(chan error, len(s.cfg.Listen))
	for _, addr := range s.cfg.Listen {
		go func(addr string) { errc <- s.listen(ctx, addr, handler) }(addr)
	}
	go s.loop(ctx)
	go s.prebuildBase()
	select {
	case <-ctx.Done():
		return nil
	case err := <-errc:
		return err
	}
}

// unixSocketPath is the local runner socket (used by a runner on the same machine).
func unixSocketPath() string {
	return filepath.Join(userRuntimeDir(), "offload-admin", "runner.sock")
}

// userRuntimeDir is $XDG_RUNTIME_DIR (tmpfs), or a per-user fallback.
func userRuntimeDir() string {
	if dir := os.Getenv("XDG_RUNTIME_DIR"); dir != "" {
		return dir
	}
	return filepath.Join(os.TempDir(), fmt.Sprintf("offload-%d", os.Getuid()))
}

type unixMarker struct{}

func (s *Server) listen(ctx context.Context, addr string, h http.Handler) error {
	var lns []net.Listener
	switch {
	case addr == "unix":
		p := unixSocketPath()
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			return err
		}
		_ = os.Remove(p)
		ln, err := net.Listen("unix", p)
		if err != nil {
			return err
		}
		_ = os.Chmod(p, 0o600)
		lns = append(lns, ln)
	case strings.HasPrefix(addr, "tailscale:"):
		port := strings.TrimPrefix(addr, "tailscale:")
		ips := s.waitTailscaleIPs(ctx)
		for _, ip := range ips {
			ln, err := net.Listen("tcp", net.JoinHostPort(ip, port))
			if err != nil {
				return err
			}
			lns = append(lns, ln)
		}
	default:
		ln, err := net.Listen("tcp", addr)
		if err != nil {
			return err
		}
		lns = append(lns, ln)
	}
	errc := make(chan error, len(lns))
	for _, ln := range lns {
		log.Printf("listening on %s", ln.Addr())
		isUnix := ln.Addr().Network() == "unix"
		srv := &http.Server{
			Handler:           h,
			ReadHeaderTimeout: 30 * time.Second,
			ConnContext: func(c context.Context, _ net.Conn) context.Context {
				if isUnix {
					return context.WithValue(c, unixMarker{}, true)
				}
				return c
			},
		}
		go func() { <-ctx.Done(); _ = srv.Close() }()
		go func(ln net.Listener) { errc <- srv.Serve(ln) }(ln)
	}
	err := <-errc
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

// waitTailscaleIPs blocks until tailscaled reports this node's addresses (it may start after us).
func (s *Server) waitTailscaleIPs(ctx context.Context) []string {
	for {
		st, err := tsapi.GetStatus(ctx)
		if err == nil && st.Self != nil && len(st.Self.TailscaleIPs) > 0 {
			return st.Self.TailscaleIPs
		}
		log.Printf("waiting for tailscale: %v", err)
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(3 * time.Second):
		}
	}
}

// auth admits the local socket, and over the network only a paired client: one holding a token
// whose hash is in the tokens file. A tailnet identity alone is not enough, because runners are
// devices of the same tailnet user and their jobs share their network identity; a job must never be
// able to use a runner's API. Allowed tailnet identities may only read /v1/status (discovery) and,
// while no client is paired yet, pair the first one. Web pages are refused whatever the caller's
// identity (see browser.go).
func (s *Server) auth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Context().Value(unixMarker{}) != nil {
			checkClientProto(next).ServeHTTP(w, r)
			return
		}
		if browserRequest(r) {
			writeErr(w, http.StatusForbidden, "not allowed: the runner API does not accept requests from web pages")
			return
		}
		r.Header.Del(firstPairHeader) // only ever set by us, below
		r.Header.Del("X-Offload-Caller")
		tok, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		next := checkClientProto(next)
		if tok != "" && s.tokenOK(tok) {
			next.ServeHTTP(w, r)
			return
		}
		if !s.hostOK(r.Host) {
			writeErr(w, http.StatusForbidden, fmt.Sprintf("not allowed: unknown Host %q (if clients reach this runner by that name, add it to hostnames in runner.toml)", r.Host))
			return
		}
		who, idOK := s.tailnetOK(r.Context(), r.RemoteAddr)
		if idOK {
			r.Header.Set("X-Offload-Caller", who)
		}
		switch {
		case idOK && r.Method == "GET" && r.URL.Path == "/v1/status":
			next.ServeHTTP(w, r)
		case idOK && r.Method == "POST" && r.URL.Path == "/v1/admin/pair" && s.noClientPaired() && time.Since(s.startedAt) < pairWindow:
			r.Header.Set(firstPairHeader, "1") // handlePair re-checks under the lock
			next.ServeHTTP(w, r)               // first pairing of a fresh runner
		case idOK:
			writeErr(w, http.StatusForbidden, "this client isn't paired with the runner: run `offload host pair "+s.cfg.Name+"`")
		default:
			writeErr(w, http.StatusForbidden, "not allowed")
		}
	})
}

// pairWindow is how long after it starts a runner with no paired client accepts its first
// pairing by tailnet identity alone (restart it to open the window again).
const pairWindow = 30 * time.Minute

// checkClientProto refuses requests from clients of another protocol version, except the ones
// that bring versions back in step (status, pairing, binary upgrades).
func checkClientProto(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/status", "/v1/admin/pair", "/v1/admin/binary":
		default:
			if got := r.Header.Get(api.ProtoHeader); got != fmt.Sprint(api.Proto) {
				if got == "" {
					got = "unknown"
				}
				writeErr(w, http.StatusPreconditionFailed, fmt.Sprintf("client protocol %s, runner protocol %d: upgrade the older side (`offload host upgrade`)", got, api.Proto))
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

// tokensPath is the file of paired client token hashes ("sha256:<hex>" per line).
func (s *Server) tokensPath() string {
	if s.cfg.TokensFile != "" {
		return s.cfg.TokensFile
	}
	return filepath.Join(filepath.Dir(DefaultConfigPath()), "tokens")
}

const firstPairHeader = "X-Offload-First-Pair"

// noClientPaired reports whether no client is paired yet: the tokens file is missing or empty
// (an unreadable file counts as paired, so an error never reopens first pairing).
func (s *Server) noClientPaired() bool {
	_, err := os.Stat(s.tokensPath())
	if errors.Is(err, os.ErrNotExist) {
		return true
	}
	return err == nil && len(s.pairedTokens()) == 0
}

func (s *Server) pairedTokens() []string {
	b, err := os.ReadFile(s.tokensPath())
	if err != nil {
		return nil
	}
	var out []string
	for _, line := range strings.Split(string(b), "\n") {
		if line = strings.TrimSpace(line); line != "" && !strings.HasPrefix(line, "#") {
			out = append(out, line)
		}
	}
	return out
}

// handlePair adds a client's token hash. A fresh runner accepts the first pairing from an
// allowed tailnet identity; after that only an already paired client can pair another.
func (s *Server) handlePair(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Hash string `json:"hash"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&req); err != nil || len(req.Hash) != 64 {
		writeErr(w, 400, "want {\"hash\": \"<sha256 hex of the client token>\"}")
		return
	}
	if _, err := hex.DecodeString(req.Hash); err != nil {
		writeErr(w, 400, "hash must be hex")
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if r.Header.Get(firstPairHeader) == "1" && !s.noClientPaired() {
		writeErr(w, http.StatusForbidden, "another client paired first: pair from it (`offload host pair "+s.cfg.Name+"`)")
		return
	}
	for _, line := range s.pairedTokens() {
		if line == "sha256:"+req.Hash {
			writeJSON(w, map[string]string{"status": "already paired"})
			return
		}
	}
	_ = os.MkdirAll(filepath.Dir(s.tokensPath()), 0o700)
	f, err := os.OpenFile(s.tokensPath(), os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	_, err = fmt.Fprintf(f, "sha256:%s\n", req.Hash)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	log.Printf("paired a client (%s)", r.Header.Get("X-Offload-Caller"))
	writeJSON(w, map[string]string{"status": "paired"})
}

func (s *Server) tokenOK(tok string) bool {
	if tok == "" {
		return false
	}
	sum := sha256.Sum256([]byte(tok))
	want := hex.EncodeToString(sum[:])
	for _, line := range s.pairedTokens() {
		// Lines hold either the token itself or "sha256:<hex>".
		if h, ok := strings.CutPrefix(line, "sha256:"); ok {
			if subtle.ConstantTimeCompare([]byte(h), []byte(want)) == 1 {
				return true
			}
		} else if line != "" && subtle.ConstantTimeCompare([]byte(line), []byte(tok)) == 1 {
			return true
		}
	}
	return false
}

func (s *Server) tailnetOK(ctx context.Context, remote string) (string, bool) {
	host, _, err := net.SplitHostPort(remote)
	if err != nil {
		return "", false
	}
	s.whoisMu.Lock()
	e, ok := s.whois[host]
	s.whoisMu.Unlock()
	if ok && time.Since(e.at) < time.Minute {
		return e.who, e.ok
	}
	w, err := tsapi.LookupWhoIs(ctx, remote)
	e = whoisEntry{at: time.Now()}
	if err == nil {
		e.who = w.UserProfile.LoginName
		if len(w.Node.Tags) > 0 {
			e.who = strings.Join(w.Node.Tags, ",")
		}
		for _, u := range s.cfg.AllowUsers {
			if len(w.Node.Tags) == 0 && strings.EqualFold(u, w.UserProfile.LoginName) {
				e.ok = true
			}
		}
		for _, t := range s.cfg.AllowTags {
			for _, nt := range w.Node.Tags {
				if t == nt {
					e.ok = true
				}
			}
		}
	}
	s.whoisMu.Lock()
	s.whois[host] = e
	s.whoisMu.Unlock()
	return e.who, e.ok
}

// loop runs the scheduler: finishes jobs whose units ended, admits queued work, reaps idle
// sessions, and collects garbage.
func (s *Server) loop(ctx context.Context) {
	tick := time.NewTicker(200 * time.Millisecond) // exit records are a cheap stat; see checkRunning
	defer tick.Stop()
	lastGC, lastRelays, lastRetire := time.Time{}, time.Time{}, time.Time{}
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		case <-s.wake:
		}
		s.checkRunning()
		s.checkSessions()
		if time.Since(lastRetire) > 10*time.Second {
			s.mu.Lock()
			failed := s.retireErr != ""
			s.mu.Unlock()
			if failed {
				lastRetire = time.Now()
				err := s.retireOtherVersions()
				s.mu.Lock()
				if err != nil {
					s.retireErr = err.Error()
				} else {
					s.retireErr = ""
					log.Printf("old workloads retired; admitting work")
				}
				s.mu.Unlock()
			}
		}
		s.reapUploads()
		if time.Since(lastRelays) > 5*time.Second {
			lastRelays = time.Now()
			s.ensureRelays()
		}
		s.admit()
		if time.Since(lastGC) > 10*time.Minute {
			lastGC = time.Now()
			s.gc()
		}
	}
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, s.status())
}

func (s *Server) status() api.Status {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := api.Status{
		Name: s.cfg.Name, Version: s.version,
		Capacity:     api.Resources{CPU: s.cfg.Capacity.CPU, CPUMax: float64(len(s.cfg.cpus)), Mem: s.cfg.memCap},
		Defaults:     api.Resources{CPU: s.cfg.Defaults.CPU, Mem: s.cfg.defMem},
		MemAvailable: memAvailable(), MemReserve: s.cfg.memReserve, Load1: load1(),
		Profiles: map[string]api.ProfileInfo{}, Persistent: s.cfg.persistent, Accepting: true,
	}
	st.Hostname, _ = os.Hostname()
	st.Proto = api.Proto
	st.OS, st.Arch, st.Runtime = runtime.GOOS, runtime.GOARCH, "host"
	if s.containersAvailable() {
		st.Runtime = "container"
		st.Images = len(s.imageUse)
	}
	st.Allocated = s.allocatedLocked()
	for _, j := range s.jobs {
		switch j.State {
		case api.StateRunning:
			st.Running++
		case api.StateQueued, api.StateUploading:
			st.Queued++
		}
	}
	for _, se := range s.sessions {
		if !api.Terminal(se.State) {
			st.Sessions++
		}
	}
	for k := range s.slots {
		st.WarmKeys = append(st.WarmKeys, k) // container slots are "<workspace>--<variant>"
	}
	sort.Strings(st.WarmKeys)
	if recs, err := filepath.Glob(filepath.Join(s.cfg.StateDir, "data", "*.json")); err == nil {
		for _, r := range recs {
			st.DataSets = append(st.DataSets, strings.TrimSuffix(filepath.Base(r), ".json"))
		}
	}
	for name, p := range s.cfg.loadProfiles() {
		st.Profiles[name] = api.ProfileInfo{Kind: p.Kind, Match: p.Match, Description: p.Description, Resources: p.resources()}
	}
	for f, on := range s.features {
		if on {
			st.Features = append(st.Features, f)
		}
	}
	sort.Strings(st.Features)
	if st.MemAvailable < s.cfg.memReserve {
		st.Accepting, st.Reason = false, "host memory below reserve"
	}
	if s.retireErr != "" {
		st.Accepting, st.Reason = false, "workloads of an older version still running: "+s.retireErr
	}
	if s.upgrading {
		st.Accepting, st.Reason = false, "upgrading"
	}

	return st
}

// allocatedLocked sums the guarantees of running jobs and live sessions.
func (s *Server) allocatedLocked() api.Resources {
	var a api.Resources
	for _, j := range s.jobs {
		if j.State == api.StateRunning {
			a.CPU += j.Resources.CPU
			a.Mem += j.Resources.Mem
		}
	}
	for _, se := range s.sessions {
		if !api.Terminal(se.State) {
			a.CPU += se.Resources.CPU
			a.Mem += se.Resources.Mem
		}
	}
	return a
}

// fitsLocked reports whether r can start now, or why not.
func (s *Server) fitsLocked(r api.Resources) (bool, string) {
	a := s.allocatedLocked()
	if a.CPU+r.CPU > s.cfg.Capacity.CPU+1e-9 {
		return false, fmt.Sprintf("cpu: %.1f of %.1f allocated", a.CPU, s.cfg.Capacity.CPU)
	}
	if a.Mem+r.Mem > s.cfg.memCap {
		return false, "memory: runner capacity allocated"
	}
	if memAvailable()-r.Mem < s.cfg.memReserve {
		return false, "memory: host MemAvailable would drop below the reserve"
	}
	return true, ""
}

// pickCPUs chooses the n least-used CPUs for a job and marks them used.
func (s *Server) pickCPUsLocked(n int) []int {
	if !s.features["cpuset"] {
		return nil
	}
	cpus := append([]int(nil), s.cfg.cpus...)
	sort.SliceStable(cpus, func(i, j int) bool { return s.cpuUse[cpus[i]] < s.cpuUse[cpus[j]] })
	if n > len(cpus) {
		n = len(cpus)
	}
	out := append([]int(nil), cpus[:n]...)
	sort.Ints(out)
	for _, c := range out {
		s.cpuUse[c]++
	}
	return out
}

func (s *Server) releaseCPUsLocked(cpus []int) {
	for _, c := range cpus {
		if s.cpuUse[c] > 0 {
			s.cpuUse[c]--
		}
	}
}

func (s *Server) handleUpgrade(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	if s.upgrading {
		s.mu.Unlock()
		writeErr(w, 503, "an upgrade is already in progress")
		return
	}
	s.upgrading = true // no new work starts until the new binary takes over (or this fails)
	s.mu.Unlock()
	done := false
	defer func() {
		if !done {
			s.mu.Lock()
			s.upgrading = false
			s.mu.Unlock()
			s.kick()
		}
	}()
	tmp := s.self + ".new"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o755)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	_, err = f.ReadFrom(http.MaxBytesReader(w, r.Body, 512<<20))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	proto := 0
	if err == nil {
		proto, err = checkBinary(tmp)
	}
	if err == nil && proto != api.Proto && r.URL.Query().Get("force") != "1" {
		// The new version would stop every workload this one started: only when none run, or
		// when asked to.
		s.mu.Lock()
		busy := s.allocatedBusyLocked()
		s.mu.Unlock()
		if busy > 0 {
			err = fmt.Errorf("the new binary speaks protocol %d (this one %d) and would stop %d running job(s)/session(s); retry when idle, or with --force", proto, api.Proto, busy)
		}
	}
	if err == nil {
		err = os.Rename(tmp, s.self)
	}
	if err != nil {
		_ = os.Remove(tmp)
		writeErr(w, 400, "upgrade: "+err.Error())
		return
	}
	done = true
	writeJSON(w, map[string]string{"status": "upgraded; restarting"})
	log.Printf("binary replaced; exiting so systemd restarts the new version")
	go func() { time.Sleep(300 * time.Millisecond); os.Exit(0) }()
}

// allocatedBusyLocked counts running jobs and live sessions.
func (s *Server) allocatedBusyLocked() int {
	n := 0
	for _, j := range s.jobs {
		if j.State == api.StateRunning {
			n++
		}
	}
	for _, se := range s.sessions {
		if !api.Terminal(se.State) {
			n++
		}
	}
	return n
}

// hostTimezone returns the IANA zone of /etc/localtime, so containers keep the host's clock zone.
func hostTimezone() string {
	if tz := os.Getenv("TZ"); tz != "" {
		return tz
	}
	if p, err := os.Readlink("/etc/localtime"); err == nil {
		if i := strings.Index(p, "zoneinfo/"); i >= 0 {
			return p[i+len("zoneinfo/"):]
		}
	}
	return ""
}

func newID(prefix string) string {
	b := make([]byte, 5)
	_, _ = rand.Read(b)
	return prefix + hex.EncodeToString(b)
}

// uploadIdle is how long an upload may stall before it is abandoned.
const uploadIdle = 2 * time.Minute

// idleBody reads a request body with a deadline that is pushed forward while data flows, and
// stops early when stop reports true (the job was cancelled).
type idleBody struct {
	r    io.Reader
	rc   *http.ResponseController
	stop func() bool
}

func (b idleBody) Read(p []byte) (int, error) {
	// Arm the idle deadline first, then check for cancellation: a DELETE arriving in between sets
	// the deadline to now afterwards, so it can never be overwritten by this one.
	_ = b.rc.SetReadDeadline(time.Now().Add(uploadIdle))
	if b.stop != nil && b.stop() {
		return 0, errors.New("cancelled")
	}
	return b.r.Read(p)
}

func uploadBody(w http.ResponseWriter, r *http.Request, limit int64, stop func() bool) io.Reader {
	return idleBody{r: http.MaxBytesReader(w, r.Body, limit), rc: http.NewResponseController(w), stop: stop}
}

// reply holds a handler's response until s.mu is released, so a client that stops reading can
// never stall the scheduler while it holds the lock.
type reply struct {
	code  int
	body  any
	empty bool
}

func (r *reply) json(v any) { r.code, r.body, r.empty = http.StatusOK, v, false }
func (r *reply) err(code int, msg string) {
	r.code, r.body, r.empty = code, api.Error{Error: msg}, false
}
func (r *reply) noContent() { r.code, r.empty = http.StatusNoContent, true }

func (r *reply) send(w http.ResponseWriter) {
	switch {
	case r.code == 0:
	case r.empty:
		w.WriteHeader(r.code)
	default:
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(r.code)
		_ = json.NewEncoder(w).Encode(r.body)
	}
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(api.Error{Error: msg})
}

func writeFileJSON(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
