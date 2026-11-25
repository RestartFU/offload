// Package runner implements the offload runner daemon: an HTTP API that runs jobs and stdio
// sessions as resource-limited systemd user units on this machine.
package runner

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base32"
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
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/RestartFU/offload/internal/api"
	"github.com/RestartFU/offload/internal/tsapi"
	"github.com/RestartFU/offload/internal/units"
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

	builds       map[string]*envBuild // image tag -> build in progress
	buildErrs    map[string]error     // last failed build per tag, reported to the next session
	readyImages  map[string]bool
	readyChecked map[string]time.Time // when readyImages was last confirmed against podman
	imageUse     map[string]time.Time

	tokensLock func() // releases the lifetime lock on the tokens file
	upgrading  bool   // a new binary is being swapped in: start nothing
	retireErr  string // set while workloads of another version couldn't be confirmed stopped

	whoisMu sync.Mutex
	whois   map[string]whoisEntry

	ownNames selfNames // our own DNS names, for the Host check

	started time.Time
	metrics metricsState // usage samples (metrics.go)
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
		builds: map[string]*envBuild{}, buildErrs: map[string]error{}, readyImages: map[string]bool{}, readyChecked: map[string]time.Time{}, tz: hostTimezone(),
	}
	ctrl := delegatedControllers()
	s.features = map[string]bool{"cpuset": ctrl["cpuset"], "io": ctrl["io"], "memory": ctrl["memory"], "cpu": ctrl["cpu"]}
	for _, d := range []string{"jobs", "sessions", "work"} {
		if err := os.MkdirAll(filepath.Join(cfg.StateDir, d), 0o700); err != nil {
			return nil, err
		}
	}
	s.loadHistory()
	s.loadMetrics()
	s.started = time.Now()
	s.loadImageUse()
	// Held for the daemon's lifetime: `runner reset-pairing` resets the file directly only when
	// no runner holds it.
	if release, err := tryLock(s.tokensPath() + ".lock"); err != nil {
		log.Printf("tokens file %s is locked by another runner process", s.tokensPath())
	} else {
		s.tokensLock = release // kept: the open file holds the lock (and must not be collected)
	}
	s.restore()
	if err := s.reconcile(); err != nil {
		log.Printf("startup: %v (admitting nothing until this succeeds)", err)
		s.retireErr = err.Error()
	}
	return s, nil
}

// reconcile settles what a previous runner process left behind before any new work starts:
// workloads of another protocol version, orphaned build/bootstrap units, then the cache layout
// (which both may be using).
func (s *Server) reconcile() error {
	if err := s.retireOtherVersions(); err != nil {
		return err
	}
	if err := s.stopOrphanBuilds(); err != nil {
		return err
	}
	return s.resetCachesIfOld()
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
	mux.HandleFunc("POST /v1/estimate", s.handleEstimate)
	mux.HandleFunc("POST /v1/admin/reset-pairing-checked", s.handleResetPairing)
	mux.HandleFunc("POST /v1/admin/pair", s.handlePair)
	mux.HandleFunc("GET /v1/metrics", s.handleMetrics)
	mux.HandleFunc("GET /v1/metrics/series", s.handleSeries)
	mux.HandleFunc("GET /v1/ledger", s.handleLedger)

	handler := s.auth(mux)
	errc := make(chan error, len(s.cfg.Listen))
	for _, addr := range s.cfg.Listen {
		go func(addr string) { errc <- s.listen(ctx, addr, handler) }(addr)
	}
	go s.loop(ctx)
	go s.prebuildBase()
	go s.sampleLoop(ctx)
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
			IdleTimeout:       2 * time.Minute, // clients that go away don't leave idle connections
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
		r.Header.Del(pairByCodeHeader) // only ever set by us, below
		r.Header.Del(callerTokenHeader)
		r.Header.Del("X-Offload-Caller")
		tok, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		next := checkClientProto(next)
		if tok != "" && s.tokenOK(tok) {
			sum := sha256.Sum256([]byte(tok))
			r.Header.Set(callerTokenHeader, hex.EncodeToString(sum[:]))
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
		case idOK && r.Method == "POST" && r.URL.Path == "/v1/admin/pair":
			r.Header.Set(pairByCodeHeader, "1") // handlePair requires the runner's one-time code
			next.ServeHTTP(w, r)
		case idOK:
			writeErr(w, http.StatusForbidden, "this client isn't paired with the runner: run `offload host pair "+s.cfg.Name+"`")
		default:
			writeErr(w, http.StatusForbidden, "not allowed")
		}
	})
}

// tokenHashPaired reports whether a token hash is still in the tokens file.
func (s *Server) tokenHashPaired(hash string) bool {
	for _, line := range s.pairedTokens() {
		h, hashed := strings.CutPrefix(line, "sha256:")
		if !hashed {
			sum := sha256.Sum256([]byte(line))
			h = hex.EncodeToString(sum[:])
		}
		if subtle.ConstantTimeCompare([]byte(h), []byte(hash)) == 1 {
			return true
		}
	}
	return false
}

// handleResetPairing forgets every paired client (local socket only) and returns a fresh code.
func (s *Server) handleResetPairing(w http.ResponseWriter, r *http.Request) {
	if r.Context().Value(unixMarker{}) == nil {
		writeErr(w, http.StatusForbidden, "local socket only: run `offload runner reset-pairing` on the runner")
		return
	}
	var req struct {
		Tokens string `json:"tokens"` // the tokens file the caller means: never reset another runner's
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&req); err != nil || req.Tokens == "" {
		writeErr(w, 400, "want {\"tokens\": \"<tokens file>\"}")
		return
	}
	mine, _ := filepath.Abs(s.tokensPath())
	if req.Tokens != mine {
		writeErr(w, http.StatusPreconditionFailed, "this runner uses "+mine+", not "+req.Tokens+": nothing was reset")
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	code, err := ResetPairing(s.tokensPath())
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	log.Printf("pairing reset: every client must pair again")
	writeJSON(w, map[string]string{"code": code, "tokens": s.tokensPath()})
}

// removeToken drops the tokens-file line for a token hash (stored hashed or, legacy, raw).
func (s *Server) removeToken(hash string) error {
	b, err := os.ReadFile(s.tokensPath())
	if err != nil {
		return err
	}
	var keep []string
	for _, line := range strings.Split(string(b), "\n") {
		t := strings.TrimSpace(line)
		h, hashed := strings.CutPrefix(t, "sha256:")
		if !hashed {
			sum := sha256.Sum256([]byte(t))
			h = hex.EncodeToString(sum[:])
		}
		if t != "" && h == hash {
			continue
		}
		if t != "" {
			keep = append(keep, t)
		}
	}
	tmp := s.tokensPath() + ".tmp"
	if err := os.WriteFile(tmp, []byte(strings.Join(keep, "\n")+"\n"), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.tokensPath())
}

// consumePairCode checks a one-time pairing code and uses it up. Callers hold s.mu.
func (s *Server) consumePairCode(code string) error {
	const help = "pairing this client needs the runner's one-time code: on the runner, run `offload runner pair-code`, then `offload host pair NAME --code CODE` here"
	b, err := os.ReadFile(s.pairCodePath())
	if code == "" || err != nil {
		return errors.New(help)
	}
	f := strings.Fields(string(b)) // "<sha256 hex> <unix expiry>"
	if len(f) != 2 {
		return errors.New(help)
	}
	exp, _ := strconv.ParseInt(f[1], 10, 64)
	sum := sha256.Sum256([]byte(strings.TrimSpace(code)))
	if time.Now().Unix() > exp {
		_ = os.Remove(s.pairCodePath())
		return errors.New("the pairing code expired; " + help)
	}
	if subtle.ConstantTimeCompare([]byte(f[0]), []byte(hex.EncodeToString(sum[:]))) != 1 {
		// One guess per code: a wrong one burns it, so it can't be brute-forced.
		_ = os.Remove(s.pairCodePath())
		return errors.New("wrong pairing code (it is now used up); " + help)
	}
	return os.Remove(s.pairCodePath())
}

// ResetPairing forgets every paired client and returns a fresh pairing code. With the runner
// running, `offload runner reset-pairing` asks it instead (handleResetPairing), so the reset
// is ordered with pair requests in flight.
func ResetPairing(tokensFile string) (string, error) {
	if tokensFile == "" {
		tokensFile = filepath.Join(filepath.Dir(DefaultConfigPath()), "tokens")
	}
	if err := os.WriteFile(tokensFile, nil, 0o600); err != nil && !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	return NewPairCode(tokensFile)
}

// NewPairCode makes a fresh one-time pairing code for the runner whose tokens file is
// tokensFile ("" for the default), valid for a day, and returns it.
func NewPairCode(tokensFile string) (string, error) {
	if tokensFile == "" {
		tokensFile = filepath.Join(filepath.Dir(DefaultConfigPath()), "tokens")
	}
	buf := make([]byte, 10)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	code := strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(buf))
	sum := sha256.Sum256([]byte(code))
	dir := filepath.Dir(tokensFile)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	line := fmt.Sprintf("%s %d\n", hex.EncodeToString(sum[:]), time.Now().Add(24*time.Hour).Unix())
	if err := os.WriteFile(filepath.Join(dir, "pair-code"), []byte(line), 0o600); err != nil {
		return "", err
	}
	return code, nil
}

// runnerID identifies this runner across addresses and names: its machine and state dir.
func runnerID(stateDir string) string {
	mid, _ := os.ReadFile("/etc/machine-id")
	return hash12(strings.TrimSpace(string(mid)), stateDir)
}

// checkClientProto refuses requests from clients of another protocol version, except the ones
// that bring versions back in step (status, pairing, binary upgrades).
func checkClientProto(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/status", "/v1/admin/pair", "/v1/admin/binary", "/v1/admin/reset-pairing-checked":
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

const pairByCodeHeader = "X-Offload-Pair-By-Code"

// callerOf names how a request was admitted, for the job's record: "local" (the unix socket),
// "token" (a paired client), or the tailnet identity. auth strips the headers it relies on.
func callerOf(r *http.Request) string {
	switch {
	case r.Context().Value(unixMarker{}) != nil:
		return "local"
	case r.Header.Get(callerTokenHeader) != "":
		return "token"
	}
	return r.Header.Get("X-Offload-Caller")
}

// callerTokenHeader carries the sha256 of the token a request authenticated with (set by auth).
const callerTokenHeader = "X-Offload-Caller-Token"

// pairCodePath holds the hash of the runner's one-time pairing code (see NewPairCode).
func (s *Server) pairCodePath() string {
	return filepath.Join(filepath.Dir(s.tokensPath()), "pair-code")
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

// handlePair adds a client's token hash. A paired client (or the local socket) may pair another;
// anyone else needs the runner's one-time pairing code, which only someone on the runner itself
// can read (`offload runner pair-code`). Tailnet identity alone never pairs: jobs on other
// runners share it.
func (s *Server) handlePair(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Hash   string `json:"hash"`
		Code   string `json:"code"`
		Rotate bool   `json:"rotate"` // replace the token this request authenticated with
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
	// Re-check the caller's token under the lock: a reset that ran while this request was on its
	// way must not be undone by it.
	if old := r.Header.Get(callerTokenHeader); old != "" && !s.tokenHashPaired(old) {
		writeErr(w, http.StatusForbidden, "this client isn't paired with the runner any more")
		return
	}
	if r.Header.Get(pairByCodeHeader) == "1" {
		if err := s.consumePairCode(req.Code); err != nil {
			writeErr(w, http.StatusForbidden, err.Error())
			return
		}
	}
	status := "paired"
	for _, line := range s.pairedTokens() {
		if line == "sha256:"+req.Hash {
			status = "already paired"
		}
	}
	if status == "paired" {
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
	}
	if old := r.Header.Get(callerTokenHeader); req.Rotate && old != "" && old != req.Hash {
		if err := s.removeToken(old); err != nil {
			writeErr(w, 500, "paired, but the old token is still valid: "+err.Error())
			return
		}
		status += "; old token retired"
	}
	log.Printf("pair request from %s: %s", r.Header.Get("X-Offload-Caller"), status)
	writeJSON(w, map[string]string{"status": status})
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
				err := s.reconcile()
				s.mu.Lock()
				if err != nil {
					s.retireErr = err.Error()
				} else {
					s.retireErr = ""
					log.Printf("startup reconciliation done; admitting work")
				}
				s.mu.Unlock()
				if err == nil {
					go s.prebuildBase()
				}
			}
		}
		s.reapUploads()
		if time.Since(lastRelays) > 5*time.Second {
			lastRelays = time.Now()
			s.ensureRelays()
			retryStops()
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
		ID: runnerID(s.cfg.StateDir), Name: s.cfg.Name, Version: s.version,
		Capacity:     api.Resources{CPU: s.cfg.Capacity.CPU, CPUMax: float64(len(s.cfg.cpus)), Mem: s.cfg.memCap},
		Defaults:     api.Resources{CPU: s.cfg.Defaults.CPU, Mem: s.cfg.defMem},
		MemAvailable: memAvailable(), MemReserve: s.cfg.memReserve, Load1: load1(),
		Profiles: map[string]api.ProfileInfo{}, Persistent: s.cfg.persistent, Accepting: true,
	}
	st.Hostname, _ = os.Hostname()
	st.Started, st.HostCPUs, st.Metrics = s.started, runtime.NumCPU(), true
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
		st.Accepting, st.Reason = false, "not ready: "+s.retireErr
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
	held := s.heldLocked()
	a.CPU += held.CPU
	a.Mem += held.Mem
	return a
}

// heldLocked is what live sessions and environment builds hold: capacity that doesn't come back
// when running jobs finish (sessions live for hours; builds take build_mem each).
func (s *Server) heldLocked() api.Resources {
	var a api.Resources
	for _, se := range s.sessions {
		if !api.Terminal(se.State) {
			a.CPU += se.Resources.CPU
			a.Mem += se.Resources.Mem
		}
	}
	a.Mem += int64(len(s.builds)) * s.cfg.buildMem
	return a
}

// fitsLocked reports whether r can start now, or why not.
// neverFitsLocked says why a request can't run here even on an idle runner ("" if it can).
func (s *Server) neverFitsLocked(r api.Resources) string {
	if r.CPU > s.cfg.Capacity.CPU+1e-9 {
		return fmt.Sprintf("needs %.1f cpu; this runner's capacity is %.1f", r.CPU, s.cfg.Capacity.CPU)
	}
	if r.Mem > s.cfg.memCap {
		return fmt.Sprintf("needs %s of memory; this runner's capacity is %s", units.FormatSize(r.Mem), units.FormatSize(s.cfg.memCap))
	}
	if total := meminfo("MemTotal"); total > 0 && r.Mem > total-s.cfg.memReserve {
		return fmt.Sprintf("needs %s of memory; this host has %s, %s of it reserved", units.FormatSize(r.Mem), units.FormatSize(total), units.FormatSize(s.cfg.memReserve))
	}
	return ""
}

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
	// Receive and check the candidate first, with an idle deadline and a total one: a stalled
	// upload must never hold admission.
	f, err := os.CreateTemp(filepath.Dir(s.self), ".offload-upgrade-*")
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	tmp := f.Name()
	defer os.Remove(tmp) // a no-op once renamed into place
	deadline := time.Now().Add(10 * time.Minute)
	_, err = f.ReadFrom(uploadBody(w, r, 512<<20, func() bool { return time.Now().After(deadline) }))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Chmod(tmp, 0o755)
	}
	proto := 0
	if err == nil {
		proto, err = checkBinary(tmp)
	}
	if err != nil {
		writeErr(w, 400, "upgrade: "+err.Error())
		return
	}
	// Then, briefly: no new work starts between the compatibility check and the restart.
	s.mu.Lock()
	if s.upgrading {
		s.mu.Unlock()
		writeErr(w, 503, "an upgrade is already in progress")
		return
	}
	if busy := s.allocatedBusyLocked(); proto != api.Proto && busy > 0 && r.URL.Query().Get("force") != "1" {
		s.mu.Unlock()
		writeErr(w, 400, fmt.Sprintf("upgrade: the new binary speaks protocol %d (this one %d) and would stop %d running job(s)/session(s); retry when idle, or with --force", proto, api.Proto, busy))
		return
	}
	s.upgrading = true
	s.mu.Unlock()
	if err := os.Rename(tmp, s.self); err != nil {
		s.mu.Lock()
		s.upgrading = false
		s.mu.Unlock()
		s.kick()
		writeErr(w, 500, "upgrade: "+err.Error())
		return
	}
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

// ResetPairingLocal resets pairing through the runner on this machine if it is running (so the
// reset is ordered with pair requests in flight), or directly in the tokens file otherwise.
func ResetPairingLocal(tokensFile string) (string, error) {
	if tokensFile == "" {
		tokensFile = filepath.Join(filepath.Dir(DefaultConfigPath()), "tokens")
	}
	if abs, err := filepath.Abs(tokensFile); err == nil {
		tokensFile = abs
	}
	// Only a runner confirmed not running (nothing listening on its socket) is reset directly;
	// any other failure leaves everything as it was.
	conn, err := net.DialTimeout("unix", unixSocketPath(), 2*time.Second)
	if err != nil {
		// Nothing on the socket. A runner serving this tokens file anyway (TCP only) holds its
		// lock; only without one is the file ours to reset.
		release, lerr := tryLock(tokensFile + ".lock")
		if lerr != nil {
			return "", fmt.Errorf("a runner is using %s but not answering on its local socket; stop it (systemctl --user stop offload-runner) and reset again", tokensFile)
		}
		defer release()
		return ResetPairing(tokensFile)
	}
	c := &http.Client{Timeout: 30 * time.Second, Transport: &http.Transport{
		DialContext: func(context.Context, string, string) (net.Conn, error) {
			if conn != nil {
				cc := conn
				conn = nil
				return cc, nil
			}
			var d net.Dialer
			return d.Dial("unix", unixSocketPath())
		}}}
	body, _ := json.Marshal(map[string]string{"tokens": tokensFile})
	resp, err := c.Post("http://local/v1/admin/reset-pairing-checked", "application/json", bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("the runner is running but didn't answer the reset (nothing changed?): %w", err)
	}
	defer resp.Body.Close()
	var out struct{ Code, Error string }
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil || resp.StatusCode != 200 {
		return "", fmt.Errorf("runner refused the reset: %s %s", resp.Status, out.Error)
	}
	return out.Code, nil
}

// tryLock takes an exclusive lock on path without waiting.
func tryLock(path string) (func(), error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return nil, err
	}
	return func() { _ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN); f.Close() }, nil
}
