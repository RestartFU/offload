package runner

import (
	"bufio"
	"encoding/json"
	"errors"
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
	"strings"
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

	relay *relay // client networking requests, when the session uses network = "client"

	inMu  sync.Mutex // serializes stdin writes
	conn  net.Conn
	connR *bufio.Reader
}

func (se *session) save() error {
	err := writeFileJSON(filepath.Join(se.dir, "session.json"), se.Session)
	if err != nil {
		log.Printf("session %s: save: %v", se.ID, err)
	}
	return err
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
			if se.StopRequested {
				stopUnit(se.Unit)
			}
			if se.Network == "client" {
				se.relay = newRelay() // sockets: ensureRelays
			}
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
	var out reply // written after s.mu is released (deferred first, so it runs last)
	defer out.send(w)
	var spec api.SessionSpec
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&spec); err != nil {
		out.err(400, "bad session spec: "+err.Error())
		return
	}
	prof := s.cfg.loadProfiles()[spec.Profile]
	if spec.Def != nil {
		// Defined by the client: build a profile from it. Data sets must have been synced.
		if len(spec.Def.Command) == 0 {
			out.err(400, "session definition has no command")
			return
		}
		prof = &Profile{Name: spec.Profile, Kind: "session", Command: spec.Def.Command, Env: spec.Def.Env,
			Devices: spec.Def.Devices, Runtime: "container", NestedSandbox: spec.Def.NestedSandbox, Network: spec.Def.Network}
		for name, ctr := range spec.Def.Data {
			if !validKey(name) {
				out.err(400, "invalid data set name "+strconv.Quote(name))
				return
			}
			if _, err := os.Stat(s.dataPath(name)); err != nil {
				out.err(400, "data set "+name+" was never synced to this runner")
				return
			}
			prof.Volumes = append(prof.Volumes, s.dataPath(name)+":"+ctr)
		}
		for name, ctr := range spec.Def.Volumes {
			if !validKey(name) {
				out.err(400, "invalid volume name "+strconv.Quote(name))
				return
			}
			prof.Volumes = append(prof.Volumes, s.volumePath(name)+":"+ctr)
		}
	}
	if prof == nil || prof.Kind != "session" || len(prof.Command) == 0 {
		out.err(400, "no session profile named "+strconv.Quote(spec.Profile))
		return
	}
	if spec.Client == "" {
		spec.Client = r.Header.Get("X-Offload-Caller")
	}
	if err := validSecrets(spec.Secrets, spec.SecretFiles); err != nil {
		out.err(400, err.Error())
		return
	}
	res := s.cfg.effective(spec.Resources, prof)
	rt := spec.Runtime
	if rt == "" {
		rt = prof.Runtime
	}
	if rt == "" {
		rt = "host"
		if s.containersAvailable() {
			rt = "container"
		}
	}
	if rt == "container" && !s.containersAvailable() {
		out.err(400, "containers are not available on this runner (install podman)")
		return
	}
	def := spec.Environment
	if def == nil && prof.Containerfile != "" && rt == "container" { // host sessions use no image
		b, err := os.ReadFile(filepath.Join(s.cfg.ProfilesDir, prof.Containerfile))
		if err != nil {
			out.err(500, "profile environment: "+err.Error())
			return
		}
		def = &api.EnvDef{Name: prof.Containerfile, Containerfile: string(b)}
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.retireErr != "" {
		out.err(503, "runner not ready: "+s.retireErr)
		return
	}
	if s.upgrading {
		out.err(503, "runner is upgrading; try again in a few seconds")
		return
	}
	if why := s.neverFitsLocked(res); why != "" { // not a matter of waiting
		out.err(http.StatusUnprocessableEntity, why)
		return
	}
	// Capacity first: a full runner says so at once (503), and the client tries another.
	live := 0
	for _, x := range s.sessions {
		if !api.Terminal(x.State) {
			live++
		}
	}
	if live >= s.cfg.Policy.MaxSessions {
		out.err(http.StatusServiceUnavailable, fmt.Sprintf("session limit reached (%d)", live))
		return
	}
	if ok, why := s.fitsLocked(res); !ok {
		out.err(http.StatusServiceUnavailable, "no capacity: "+why)
		return
	}
	image := ""
	if rt == "container" {
		tag, ready, err := s.sessionImageLocked(def)
		if err != nil {
			out.err(500, "environment build failed: "+err.Error())
			return
		}
		if !ready {
			if s.builds[tag] == nil { // no room to build it here now: another runner may have
				out.err(http.StatusServiceUnavailable, "no memory free to build environment "+strings.TrimPrefix(tag, imageRepo+"/"))
				return
			}
			out.err(http.StatusConflict, "building environment "+strings.TrimPrefix(tag, imageRepo+"/")+"; retry shortly")
			return
		}
		image = tag
	}
	now := time.Now()
	se := &session{Session: api.Session{
		ID: newID("s-"), Runner: s.cfg.Name, Profile: spec.Profile, State: api.StateRunning,
		Resources: res, CreatedAt: now, LastSeen: now, Runtime: rt, Image: image, Proto: api.Proto,
	}}
	if spec.Def != nil {
		for name := range spec.Def.Data {
			se.DataSets = append(se.DataSets, name)
		}
		sort.Strings(se.DataSets)
	}
	if prof.Network == "client" {
		if rt != "container" {
			out.err(400, "network = \"client\" needs the container runtime")
			return
		}
		se.relay = newRelay()
		se.relay.lastPoll = now // the client starts polling right after creating the session
		se.Network = "client"
	}
	se.dir = filepath.Join(s.cfg.StateDir, "sessions", se.ID)
	if err := os.MkdirAll(filepath.Join(se.dir, "tmp"), 0o700); err != nil {
		out.err(500, err.Error())
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
	if spec.NoStdin {
		pipe = append(pipe, "--no-stdin")
	}
	secrets, err := writeSecrets(se.ID, spec.Secrets, spec.SecretFiles)
	if err != nil {
		s.releaseCPUsLocked(se.cpus)
		out.err(500, "secrets: "+err.Error())
		return
	}
	command := prof.Command
	var dnsSearch []string
	if spec.Def != nil {
		dnsSearch = spec.Def.DNSSearch
	}
	if len(dnsSearch) == 0 {
		dnsSearch = spec.DNSSearch // runner-defined profiles get it too
	}
	unitEnv := env
	if rt == "container" {
		cenv := parallelEnv(ncpu)
		for k, v := range stockInstallEnv(def) {
			cenv[k] = v
		}
		for k, v := range prof.Env {
			if k != "PATH" && k != "HOME" {
				cenv[k] = v
			}
		}
		if def != nil {
			for k, v := range def.Env {
				cenv[k] = v
			}
		}
		if s.tz != "" {
			cenv["TZ"] = s.tz
		}
		cenv["TMPDIR"], cenv["OFFLOAD_SESSION"] = env["TMPDIR"], se.ID
		same := []string{filepath.Join(se.dir, "tmp")} // not se.dir: it holds exit.json and the session record
		if secrets != "" {
			same = append(same, secrets)
		}
		if se.relay != nil {
			ctl, err := s.startControl(se.relay, "sessions", se.ID, true, false)
			if err != nil {
				s.releaseCPUsLocked(se.cpus)
				_ = os.RemoveAll(secretsDir(se.ID))
				out.err(500, "control socket: "+err.Error())
				return
			}
			cenv["OFFLOAD_RUNNER_SOCK"] = ctl
			same = append(same, workloadDir(se.ID))
		}
		devices := append([]string(nil), prof.Devices...)
		if def != nil {
			devices = append(devices, def.Devices...)
		}
		var volumes []string
		for _, v := range prof.Volumes {
			host, ctr, ok := strings.Cut(v, ":")
			if !ok {
				continue
			}
			host = expandHome(host)
			_ = os.MkdirAll(host, 0o755)
			volumes = append(volumes, host+":"+ctr)
		}
		command = s.podmanRun(runSpec{Name: "offload-" + se.ID, Image: image, Workdir: filepath.Join(se.dir, "tmp"), CPUs: allowed,
			Secrets: secrets, Mem: res.Mem, Env: cenv, Interactive: true, Argv: prof.Command, ClientNet: prof.Network == "client", DNSSearch: dnsSearch,
			Same: same, Volumes: volumes, Devices: devices, NestedSandbox: prof.NestedSandbox,
			CacheScope: "session:" + prof.Name})
		unitEnv = map[string]string{}
		s.touchImage(image)
	} else if secrets != "" {
		pipe = append(pipe, "--secrets", secrets)
	}
	u := unitSpec{
		Name: se.Unit, Slice: s.cfg.Slice, Dir: home, Env: unitEnv, Delegate: rt == "container",
		Argv:      append(append(pipe, "--"), command...),
		CPUWeight: cpuWeight(res.CPU), CPUQuotaPct: int(math.Ceil(res.CPUMax * 100)),
		AllowedCPUs: allowed, MemoryMax: res.Mem, OOMScoreAdj: s.cfg.Policy.OOMScoreAdj,
		Stdout: filepath.Join(se.dir, "stderr.log"),
	}
	if s.features["io"] {
		u.IOWeight = s.cfg.Policy.IOWeight
	}
	// Recorded before systemd starts the unit, so a runner that dies right after the start still
	// finds the session on restore (and keeps accounting for and reaping it).
	s.sessions[se.ID] = se
	err = se.save()
	if err == nil {
		if err = startUnit(contextBG(), u); err != nil {
			if active, known := unitActive(u.Name); !known || active {
				// It may be running: keep the record and resources; checkSessions settles it.
				log.Printf("session %s: systemd-run reported %v; keeping it (active=%v known=%v)", se.ID, err, active, known)
				err = nil
			}
		}
	}
	if err != nil {
		delete(s.sessions, se.ID)
		s.releaseCPUsLocked(se.cpus)
		if se.relay != nil {
			se.relay.close()
		}
		_ = os.RemoveAll(workloadDir(se.ID))
		_ = os.RemoveAll(secretsDir(se.ID))
		_ = os.RemoveAll(se.dir)
		out.err(500, err.Error())
		return
	}
	out.json(se.Session)
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
	var out reply // written after s.mu is released (deferred first, so it runs last)
	defer out.send(w)
	if se := s.getSession(w, r); se != nil {
		s.mu.Lock()
		defer s.mu.Unlock()
		out.json(se.Session)
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
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxStdinFrame)) // what the wrapper accepts
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
	seq, perr := strconv.ParseInt(r.URL.Query().Get("seq"), 10, 64)
	if perr != nil || seq <= 0 {
		writeErr(w, 400, "seq must be a positive sequence number")
		return
	}
	eof := r.URL.Query().Get("eof") != "" // the client's input ended (after every frame before it)
	se.inMu.Lock()
	defer se.inMu.Unlock()
	// The wrapper (which outlives runner restarts) deduplicates by seq and acknowledges each
	// frame once the process has it, so a retry after a lost acknowledgement is never replayed.
	// Deadlines keep a process that stopped reading from wedging this handler.
	for attempt := 0; ; attempt++ {
		if se.conn == nil {
			se.conn, err = dialRetry(sessionSock(se.ID), 15*time.Second)
			if err != nil {
				writeErr(w, http.StatusBadGateway, "session stdin: "+err.Error())
				return
			}
			se.connR = bufio.NewReader(se.conn)
			if !readGreeting(se.conn, se.connR) {
				// Not the framed protocol (sessions from older runners are stopped on restore):
				// send nothing rather than guess.
				se.conn.Close()
				se.conn, se.connR = nil, nil
				writeErr(w, http.StatusBadGateway, "session stdin: wrapper protocol mismatch")
				return
			}
		}
		if eof {
			err = sendFrame(se.conn, se.connR, seq, nil)
		} else {
			err = sendFrame(se.conn, se.connR, seq, body)
		}
		if err == nil {
			break
		}
		se.conn.Close()
		se.conn, se.connR = nil, nil
		if errors.Is(err, errBusy) {
			writeErr(w, http.StatusServiceUnavailable, "session is not reading its input; retry")
			return
		}
		if errors.Is(err, errStdinClosed) {
			writeErr(w, http.StatusGone, "the session's process closed its input")
			return
		}
		if attempt > 0 || errors.Is(err, os.ErrDeadlineExceeded) {
			writeErr(w, http.StatusGatewayTimeout, "session stdin: "+err.Error())
			return
		}
	}
	s.touch(se, 0)
	w.WriteHeader(http.StatusNoContent)
}

// sendFrame writes "<seq> <len>\n<payload>" to the wrapper and waits for its "ok".
func sendFrame(c net.Conn, r *bufio.Reader, seq int64, body []byte) error {
	_ = c.SetDeadline(time.Now().Add(30 * time.Second))
	defer c.SetDeadline(time.Time{})
	if body == nil { // end of input
		if _, err := fmt.Fprintf(c, "%d eof\n", seq); err != nil {
			return err
		}
	} else {
		if _, err := fmt.Fprintf(c, "%d %d\n", seq, len(body)); err != nil {
			return err
		}
		if _, err := c.Write(body); err != nil {
			return err
		}
	}
	ack, err := r.ReadString('\n')
	if err != nil {
		return err
	}
	switch ack {
	case "ok\n":
		return nil
	case "busy\n":
		return errBusy
	case "stdin closed\n":
		return errStdinClosed
	}
	return fmt.Errorf("wrapper: %s", strings.TrimSpace(ack))
}

// maxStdinFrame is the largest stdin message a session accepts (the wrapper queues at most this).
const maxStdinFrame = 16 << 20

var (
	errBusy        = errors.New("busy")
	errStdinClosed = errors.New("stdin closed")
)

// readGreeting reports whether the wrapper on c speaks the framed protocol.
func readGreeting(c net.Conn, r *bufio.Reader) bool {
	_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))
	defer c.SetReadDeadline(time.Time{})
	line, err := r.ReadString('\n')
	return err == nil && line == pipeGreeting
}

// sessionSock is where a session's wrapper accepts stdin. It lives in the runtime directory
// because unix socket paths are limited to ~108 bytes.
func sessionSock(id string) string {
	return filepath.Join(runtimeDir(), id+".sock")
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
	var out reply // written after s.mu is released (deferred first, so it runs last)
	defer out.send(w)
	se := s.getSession(w, r)
	if se == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !api.Terminal(se.State) {
		se.Reason, se.StopRequested = "stopped by client", true
		if err := se.save(); err != nil {
			out.err(500, "can't record the stop: "+err.Error())
			return
		}
		stopUnit(se.Unit)
	}
	out.json(se.Session)
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
		active, known := unitActive(se.Unit) // an exit record only counts once the unit is gone
		s.mu.Lock()
		if active {
			se.inactive = 0
		}
		switch {
		case !known || (err == nil && active):
			// systemd didn't answer, or the wrapper is still exiting: no conclusion yet.
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
				se.StopRequested = true
				_ = se.save()
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
	if se.relay != nil {
		se.relay.close()
	}
	_ = os.RemoveAll(workloadDir(se.ID))
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
