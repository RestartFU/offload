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
	tz       string

	builds      map[string]*envBuild // image tag -> build in progress
	buildErrs   map[string]error     // last failed build per tag, reported to the next session
	readyImages map[string]bool
	imageUse    map[string]time.Time

	whoisMu sync.Mutex
	whois   map[string]whoisEntry
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
		cfg: cfg, version: version, self: self,
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
	s.restore()
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
	mux.HandleFunc("GET /v1/sessions", s.handleListSessions)
	mux.HandleFunc("POST /v1/sessions", s.handleCreateSession)
	mux.HandleFunc("GET /v1/sessions/{id}", s.handleGetSession)
	mux.HandleFunc("POST /v1/sessions/{id}/stdin", s.handleSessionStdin)
	mux.HandleFunc("GET /v1/sessions/{id}/stdout", s.handleSessionStdout)
	mux.HandleFunc("GET /v1/sessions/{id}/stderr", s.handleSessionStderr)
	mux.HandleFunc("DELETE /v1/sessions/{id}", s.handleStopSession)
	mux.HandleFunc("PUT /v1/admin/binary", s.handleUpgrade)

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
	dir := os.Getenv("XDG_RUNTIME_DIR")
	if dir == "" {
		dir = filepath.Join(os.TempDir(), fmt.Sprintf("offload-%d", os.Getuid()))
	}
	return filepath.Join(dir, "offload", "runner.sock")
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

// auth admits local socket callers, bearer-token holders, and allowed tailnet identities.
func (s *Server) auth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Context().Value(unixMarker{}) != nil {
			next.ServeHTTP(w, r)
			return
		}
		if tok, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer "); ok && s.tokenOK(tok) {
			next.ServeHTTP(w, r)
			return
		}
		if who, ok := s.tailnetOK(r.Context(), r.RemoteAddr); ok {
			r.Header.Set("X-Offload-Caller", who)
			next.ServeHTTP(w, r)
			return
		}
		writeErr(w, http.StatusForbidden, "not allowed: unknown tailnet identity and no valid token")
	})
}

func (s *Server) tokenOK(tok string) bool {
	if s.cfg.TokensFile == "" || tok == "" {
		return false
	}
	b, err := os.ReadFile(s.cfg.TokensFile)
	if err != nil {
		return false
	}
	sum := sha256.Sum256([]byte(tok))
	want := hex.EncodeToString(sum[:])
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
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
	lastGC := time.Time{}
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		case <-s.wake:
		}
		s.checkRunning()
		s.checkSessions()
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
	if err == nil {
		err = checkBinary(tmp)
	}
	if err == nil {
		err = os.Rename(tmp, s.self)
	}
	if err != nil {
		_ = os.Remove(tmp)
		writeErr(w, 400, "upgrade: "+err.Error())
		return
	}
	writeJSON(w, map[string]string{"status": "upgraded; restarting"})
	log.Printf("binary replaced; exiting so systemd restarts the new version")
	go func() { time.Sleep(300 * time.Millisecond); os.Exit(0) }()
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
