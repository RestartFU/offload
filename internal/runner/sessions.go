package runner

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/RestartFU/offload/internal/api"
	"github.com/RestartFU/offload/internal/units"
)

// session is a long-lived process whose stdin/stdout are bridged over HTTP. The unit wrapper
// ("runner pipe") owns the process, appends its stdout to a file and accepts stdin on a unix
// socket, so sessions survive runner restarts and client reconnects.
type session struct {
	api.Session
	dir      string
	cpus     []int
	inactive int

	inMu    sync.Mutex // serializes stdin writes
	conn    net.Conn
	lastSeq int64
}

func (se *session) save() {
	if err := writeFileJSON(filepath.Join(se.dir, "session.json"), se.Session); err != nil {
		log.Printf("session %s: save: %v", se.ID, err)
	}
}

func (s *Server) restoreSessions() {
	files, _ := filepath.Glob(filepath.Join(s.cfg.StateDir, "sessions", "*", "session.json"))
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		se := &session{dir: filepath.Dir(f)}
		if json.Unmarshal(b, &se.Session) != nil {
			continue
		}
		se.Attached = 0 // no client connection survives a runner restart
		if !api.Terminal(se.State) {
			se.LastSeen = time.Now()
			if c, ok := s.unitCPUs(se.dir); ok {
				se.cpus = c
				for _, x := range c {
					s.cpuUse[x]++
				}
			}
		}
		s.sessions[se.ID] = se
	}
}

func (s *Server) unitCPUs(dir string) ([]int, bool) {
	b, err := os.ReadFile(filepath.Join(dir, "cpus"))
	if err != nil {
		return nil, false
	}
	c, err := units.ParseCPUList(string(b))
	return c, err == nil
}

func (s *Server) handleCreateSession(w http.ResponseWriter, r *http.Request) {
	var spec api.SessionSpec
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&spec); err != nil {
		writeErr(w, 400, "bad session spec: "+err.Error())
		return
	}
	prof := s.cfg.loadProfiles()[spec.Profile]
	if prof == nil || prof.Kind != "session" || len(prof.Command) == 0 {
		writeErr(w, 400, "no session profile named "+strconv.Quote(spec.Profile))
		return
	}
	if spec.Client == "" {
		spec.Client = r.Header.Get("X-Offload-Caller")
	}
	res := s.cfg.effective(spec.Resources, prof)

	s.mu.Lock()
	defer s.mu.Unlock()
	live := 0
	for _, x := range s.sessions {
		if !api.Terminal(x.State) {
			live++
		}
	}
	if live >= s.cfg.Policy.MaxSessions {
		writeErr(w, http.StatusServiceUnavailable, fmt.Sprintf("session limit reached (%d)", live))
		return
	}
	if ok, why := s.fitsLocked(res); !ok {
		writeErr(w, http.StatusServiceUnavailable, "no capacity: "+why)
		return
	}
	now := time.Now()
	se := &session{Session: api.Session{
		ID: newID("s-"), Runner: s.cfg.Name, Profile: spec.Profile, State: api.StateRunning,
		Resources: res, CreatedAt: now, LastSeen: now,
	}}
	se.dir = filepath.Join(s.cfg.StateDir, "sessions", se.ID)
	if err := os.MkdirAll(filepath.Join(se.dir, "tmp"), 0o700); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	for _, f := range []string{"stdout", "stderr.log"} {
		_ = os.WriteFile(filepath.Join(se.dir, f), nil, 0o600)
	}
	ncpu := int(math.Ceil(res.CPUMax))
	se.cpus = s.pickCPUsLocked(ncpu)
	env := s.baseEnv(prof, ncpu)
	env["TMPDIR"] = filepath.Join(se.dir, "tmp")
	env["OFFLOAD_SESSION"] = se.ID
	se.Unit = "offload-" + se.ID + ".service"
	allowed := ""
	if len(se.cpus) > 0 {
		allowed = units.FormatCPUList(se.cpus)
		_ = os.WriteFile(filepath.Join(se.dir, "cpus"), []byte(allowed), 0o600)
	}
	home, _ := os.UserHomeDir()
	pipe := []string{s.self, "runner", "pipe", "--dir", se.dir, "--sock", sessionSock(se.ID)}
	if dir, err := writeSecrets(se.ID, spec.Secrets, spec.SecretFiles); err != nil {
		s.releaseCPUsLocked(se.cpus)
		writeErr(w, 500, "secrets: "+err.Error())
		return
	} else if dir != "" {
		pipe = append(pipe, "--secrets", dir)
	}
	u := unitSpec{
		Name: se.Unit, Slice: s.cfg.Slice, Dir: home, Env: env,
		Argv:      append(append(pipe, "--"), prof.Command...),
		CPUWeight: cpuWeight(res.CPU), CPUQuotaPct: int(math.Ceil(res.CPUMax * 100)),
		AllowedCPUs: allowed, MemoryMax: res.Mem, OOMScoreAdj: s.cfg.Policy.OOMScoreAdj,
		Stdout: filepath.Join(se.dir, "stderr.log"),
	}
	if s.features["io"] {
		u.IOWeight = s.cfg.Policy.IOWeight
	}
	if err := startUnit(contextBG(), u); err != nil {
		s.releaseCPUsLocked(se.cpus)
		_ = os.RemoveAll(secretsDir(se.ID))
		_ = os.RemoveAll(se.dir)
		writeErr(w, 500, err.Error())
		return
	}
	s.sessions[se.ID] = se
	se.save()
	writeJSON(w, se.Session)
}

func (s *Server) getSession(w http.ResponseWriter, r *http.Request) *session {
	s.mu.Lock()
	se := s.sessions[r.PathValue("id")]
	s.mu.Unlock()
	if se == nil {
		writeErr(w, 404, "no such session")
	}
	return se
}

func (s *Server) handleGetSession(w http.ResponseWriter, r *http.Request) {
	if se := s.getSession(w, r); se != nil {
		s.mu.Lock()
		defer s.mu.Unlock()
		writeJSON(w, se.Session)
	}
}

func (s *Server) handleListSessions(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	out := []api.Session{}
	for _, se := range s.sessions {
		out = append(out, se.Session)
	}
	s.mu.Unlock()
	sort.Slice(out, func(a, b int) bool { return out[a].CreatedAt.Before(out[b].CreatedAt) })
	writeJSON(w, out)
}

// handleSessionStdin forwards one chunk to the process. ?seq= makes retries idempotent: a
// chunk with a sequence number we've already written is acknowledged and dropped.
func (s *Server) handleSessionStdin(w http.ResponseWriter, r *http.Request) {
	se := s.getSession(w, r)
	if se == nil {
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 64<<20))
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	s.mu.Lock()
	ended := api.Terminal(se.State)
	s.mu.Unlock()
	if ended {
		writeErr(w, http.StatusGone, "session has ended")
		return
	}
	seq, _ := strconv.ParseInt(r.URL.Query().Get("seq"), 10, 64)
	se.inMu.Lock()
	defer se.inMu.Unlock()
	if seq > 0 && seq <= se.lastSeq {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	for attempt := 0; ; attempt++ {
		if se.conn == nil {
			se.conn, err = dialRetry(sessionSock(se.ID), 15*time.Second)
			if err != nil {
				writeErr(w, http.StatusBadGateway, "session stdin: "+err.Error())
				return
			}
		}
		if _, err = se.conn.Write(body); err == nil {
			break
		}
		se.conn.Close()
		se.conn = nil
		if attempt > 0 {
			writeErr(w, http.StatusBadGateway, "session stdin: "+err.Error())
			return
		}
	}
	if seq > 0 {
		se.lastSeq = seq
	}
	s.touch(se, 0)
	w.WriteHeader(http.StatusNoContent)
}

// sessionSock is where a session's wrapper accepts stdin. It lives in the runtime directory
// because unix socket paths are limited to ~108 bytes.
func sessionSock(id string) string {
	return filepath.Join(filepath.Dir(unixSocketPath()), id+".sock")
}

func dialRetry(path string, wait time.Duration) (net.Conn, error) {
	deadline := time.Now().Add(wait)
	for {
		c, err := net.Dial("unix", path)
		if err == nil || time.Now().After(deadline) {
			return c, err
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func (s *Server) touch(se *session, attachDelta int) {
	s.mu.Lock()
	se.LastSeen = time.Now()
	se.Attached += attachDelta
	s.mu.Unlock()
}

func (s *Server) handleSessionStdout(w http.ResponseWriter, r *http.Request) {
	s.streamSession(w, r, "stdout")
}

func (s *Server) handleSessionStderr(w http.ResponseWriter, r *http.Request) {
	s.streamSession(w, r, "stderr.log")
}

func (s *Server) streamSession(w http.ResponseWriter, r *http.Request, file string) {
	se := s.getSession(w, r)
	if se == nil {
		return
	}
	var offset int64
	fmt.Sscan(r.URL.Query().Get("offset"), &offset)
	follow := r.URL.Query().Get("follow") != ""
	s.touch(se, 1)
	defer s.touch(se, -1)
	done := func() bool {
		s.mu.Lock()
		defer s.mu.Unlock()
		return api.Terminal(se.State)
	}
	streamFile(w, r, filepath.Join(se.dir, file), offset, follow, done)
}

func (s *Server) handleStopSession(w http.ResponseWriter, r *http.Request) {
	se := s.getSession(w, r)
	if se == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !api.Terminal(se.State) {
		se.Reason = "stopped by client"
		stopUnit(se.Unit)
	}
	writeJSON(w, se.Session)
}

// checkSessions finishes sessions whose process ended and stops ones idle past the grace period.
func (s *Server) checkSessions() {
	s.mu.Lock()
	var live []*session
	for _, se := range s.sessions {
		if !api.Terminal(se.State) {
			live = append(live, se)
		}
	}
	s.mu.Unlock()
	for _, se := range live {
		rec, err := readExit(filepath.Join(se.dir, "exit.json"))
		active := err != nil && unitActive(se.Unit)
		s.mu.Lock()
		switch {
		case err == nil:
			code := rec.Code
			reason := se.Reason
			if rec.OOM {
				reason = fmt.Sprintf("out of memory (limit %s)", units.FormatSize(se.Resources.Mem))
			}
			s.finishSessionLocked(se, &code, reason)
		case !active:
			if se.inactive++; se.inactive >= 3 {
				s.finishSessionLocked(se, nil, "process ended without an exit status")
			}
		case se.Attached == 0 && time.Since(se.LastSeen) > s.cfg.sessionGrace:
			if se.Reason == "" {
				se.Reason = "idle: no client for " + s.cfg.sessionGrace.String()
				log.Printf("session %s: %s", se.ID, se.Reason)
				stopUnit(se.Unit)
			}
		}
		s.mu.Unlock()
	}
}

func (s *Server) finishSessionLocked(se *session, code *int, reason string) {
	now := time.Now()
	se.State, se.ExitCode, se.Reason, se.FinishedAt = api.StateExited, code, reason, &now
	_ = os.RemoveAll(secretsDir(se.ID))
	s.releaseCPUsLocked(se.cpus)
	se.cpus = nil
	go func() {
		se.inMu.Lock()
		defer se.inMu.Unlock()
		if se.conn != nil {
			se.conn.Close()
			se.conn = nil
		}
	}()
	se.save()
	s.kick()
}
