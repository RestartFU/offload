package runner

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"golang.org/x/sys/unix"

	"github.com/RestartFU/offload/internal/api"
)

// Credentials reach a job in two ways, and only while it runs:
//
//   - Relays: the job's SSH agent socket and git credential helper forward each request to the
//     attached client, which answers from the laptop (its ssh-agent, `git credential fill`).
//     Keys never leave the laptop; if no client is attached, requests fail.
//   - Secrets: named env vars and files the client resolved locally and sent with the job. They
//     live in a tmpfs directory under $XDG_RUNTIME_DIR, are never written to job records, and are
//     deleted when the job ends.

var errNoClient = errors.New("no offload client attached to answer (was the job detached?)")

type callback struct {
	ID      string `json:"id"`
	Kind    string `json:"kind"` // "ssh-agent" or "git-credential"
	Data    []byte `json:"data"`
	resp    chan []byte
	expires time.Time
}

// relay queues a job's credential requests for its client to answer.
type relay struct {
	mu       sync.Mutex
	queue    []*callback
	pending  map[string]*callback
	notify   chan struct{}
	polling  int
	lastPoll time.Time
	agent    net.Listener
	ctl      net.Listener // the workload's own control socket (see startControl)
	srv      *http.Server // serves ctl; closing it also closes its open connections
	conns    map[net.Conn]struct{}
	closed   bool
	done     chan struct{} // closed by close(): wakes waiting askers and pollers
}

// track registers a workload's live stream legs so close can end them; false once closed.
func (r *relay) track(cs ...net.Conn) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return false
	}
	if r.conns == nil {
		r.conns = map[net.Conn]struct{}{}
	}
	for _, c := range cs {
		r.conns[c] = struct{}{}
	}
	return true
}

func (r *relay) untrack(cs ...net.Conn) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, c := range cs {
		delete(r.conns, c)
	}
}

func newRelay() *relay {
	return &relay{pending: map[string]*callback{}, notify: make(chan struct{}, 1), done: make(chan struct{})}
}

func (r *relay) attached() bool {
	return r.polling > 0 || time.Since(r.lastPoll) < 10*time.Second
}

// ask sends a request to the client and waits for its answer.
func (r *relay) ask(kind string, data []byte, timeout time.Duration) ([]byte, error) {
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return nil, errors.New("the workload has ended")
	}
	if !r.attached() {
		r.mu.Unlock()
		return nil, errNoClient
	}
	cb := &callback{ID: newID("c-"), Kind: kind, Data: data, resp: make(chan []byte, 1), expires: time.Now().Add(timeout)}
	r.queue = append(r.queue, cb)
	r.pending[cb.ID] = cb
	r.mu.Unlock()
	select {
	case r.notify <- struct{}{}:
	default:
	}
	defer func() {
		// Whatever happened, this request is over: never hand it to a client later.
		r.mu.Lock()
		delete(r.pending, cb.ID)
		for i, q := range r.queue {
			if q == cb {
				r.queue = append(r.queue[:i], r.queue[i+1:]...)
				break
			}
		}
		r.mu.Unlock()
	}()
	select {
	case b := <-cb.resp:
		return b, nil
	case <-r.done:
		return nil, errors.New("the workload has ended")
	case <-time.After(timeout):
		return nil, fmt.Errorf("client did not answer %s request in %s", kind, timeout)
	}
}

func (r *relay) close() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.closed {
		close(r.done)
	}
	r.closed = true
	r.queue, r.pending = nil, map[string]*callback{} // nothing queued may reach a client later
	if r.srv != nil {
		r.srv.Close() // the listener and every open connection on it
		r.srv = nil
	}
	for _, ln := range []*net.Listener{&r.agent, &r.ctl} {
		if *ln != nil {
			(*ln).Close()
			_ = os.Remove((*ln).Addr().String())
			*ln = nil
		}
	}
	for c := range r.conns {
		c.Close()
	}
	r.conns = nil
}

// startControl gives one job or session its own unix socket that serves only its own credential
// and dial requests. Containers get this socket mounted, never the runner's main socket (whose
// callers are fully trusted) or the shared runtime directory (other workloads' sockets and secrets).
func (s *Server) startControl(rl *relay, kind, id string, dial, credential bool) (string, error) {
	path := filepath.Join(workloadDir(id), "ctl")
	_ = os.MkdirAll(filepath.Dir(path), 0o700)
	_ = os.Remove(path)
	ln, err := net.Listen("unix", path)
	if err != nil {
		return "", err
	}
	_ = os.Chmod(path, 0o600)
	own := func(h http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if r.PathValue("id") != id {
				writeErr(w, 403, "this socket only serves "+id)
				return
			}
			h(w, r)
		}
	}
	// Only the features this workload enabled are reachable from inside it.
	mux := http.NewServeMux()
	if credential {
		mux.HandleFunc("POST /v1/"+kind+"/{id}/credential", own(s.handleCredential))
	}
	if dial {
		mux.HandleFunc("POST /v1/"+kind+"/{id}/dial", own(s.handleDial))
	}
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 30 * time.Second,
		ConnContext: func(c context.Context, _ net.Conn) context.Context { return context.WithValue(c, unixMarker{}, true) }}
	go srv.Serve(ln)
	rl.mu.Lock()
	rl.ctl, rl.srv = ln, srv
	rl.mu.Unlock()
	return path, nil
}

// runtimeDir holds per-workload sockets and secrets. The runner's own (fully trusted) socket lives
// in a separate directory (unixSocketPath), so no container mount of this tree can reach it.
func runtimeDir() string { return filepath.Join(userRuntimeDir(), "offload") }

func secretsDir(id string) string { return filepath.Join(runtimeDir(), "secrets", id) }

// workloadDir holds one job's or session's relay sockets. Containers mount the directory (not
// the socket files), so sockets re-created after a runner restart are visible inside.
func workloadDir(id string) string { return filepath.Join(runtimeDir(), "w", id) }

// startAgent serves an SSH agent socket for the job that relays every message to the client.
// The agent protocol is strict request/response with 4-byte length framing.
func (r *relay) startAgent(id string) (string, error) {
	path := filepath.Join(workloadDir(id), "agent")
	_ = os.MkdirAll(filepath.Dir(path), 0o700)
	_ = os.Remove(path)
	ln, err := net.Listen("unix", path)
	if err != nil {
		return "", err
	}
	_ = os.Chmod(path, 0o600)
	r.mu.Lock()
	r.agent = ln
	r.mu.Unlock()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go r.serveAgent(c)
		}
	}()
	return path, nil
}

func (r *relay) serveAgent(c net.Conn) {
	defer c.Close()
	if !r.track(c) { // closed with the relay when the workload ends
		return
	}
	defer r.untrack(c)
	failure := []byte{0, 0, 0, 1, 5} // SSH_AGENT_FAILURE
	for {
		var hdr [4]byte
		if _, err := io.ReadFull(c, hdr[:]); err != nil {
			return
		}
		n := binary.BigEndian.Uint32(hdr[:])
		if n == 0 || n > 256<<10 {
			return
		}
		msg := make([]byte, 4+n)
		copy(msg, hdr[:])
		if _, err := io.ReadFull(c, msg[4:]); err != nil {
			return
		}
		if !api.AgentRequestAllowed(msg) { // listing keys and signing only: nothing that changes the agent
			if _, err := c.Write(failure); err != nil {
				return
			}
			continue
		}
		resp, err := r.ask("ssh-agent", msg, 2*time.Minute)
		if err != nil || len(resp) < 5 {
			resp = failure
		}
		if _, err := c.Write(resp); err != nil {
			return
		}
	}
}

// setupCredentials prepares relays and secrets for a job about to start and adds their env.
// It returns the secrets directory to hand to the wrapper ("" if none).
// For a containerized job it also records in j.mounts the only runtime paths the container may see.
func (s *Server) setupCredentials(j *job, env map[string]string) (string, error) {
	container := j.Runtime == "container"
	sock := unixSocketPath()
	if j.relay != nil && container && (j.Spec.Network == "client" || slices.Contains(j.Spec.Relay, "git")) {
		ctl, err := s.startControl(j.relay, "jobs", j.ID, j.Spec.Network == "client", slices.Contains(j.Spec.Relay, "git"))
		if err != nil {
			return "", fmt.Errorf("control socket: %w", err)
		}
		sock = ctl
	}
	if j.Spec.Network == "client" {
		env["OFFLOAD_RUNNER_SOCK"] = sock
	}
	if j.relay != nil {
		for _, kind := range j.Spec.Relay {
			switch kind {
			case "ssh-agent":
				path, err := j.relay.startAgent(j.ID)
				if err != nil {
					return "", fmt.Errorf("ssh-agent relay: %w", err) // it was asked for: don't run without it
				}
				env["SSH_AUTH_SOCK"] = path
				if _, ok := env["GIT_SSH_COMMAND"]; !ok {
					env["GIT_SSH_COMMAND"] = "ssh -o StrictHostKeyChecking=accept-new"
				}
			case "git":
				if !container && !s.listensUnix() {
					s.note(j, "git credential relay needs the runner's unix socket (listen = [..., \"unix\"])")
					continue
				}
				addGitConfig(env, "credential.helper", "") // drop the runner's own helpers
				addGitConfig(env, "credential.helper", "!'"+s.self+"' runner credential")
				env["OFFLOAD_RUNNER_SOCK"] = sock
			}
		}
	}
	for _, kv := range j.Spec.GitConfig {
		addGitConfig(env, kv[0], kv[1])
	}
	dir, err := writeSecrets(j.ID, j.secrets, j.secretFiles)
	j.secrets, j.secretFiles = nil, nil // from here on they only exist in the tmpfs directory
	if err != nil {
		return "", fmt.Errorf("secrets: %w", err) // never run a job without the secrets it asked for
	}
	if dir != "" {
		j.mounts = append(j.mounts, dir)
	}
	if container {
		if _, err := os.Stat(workloadDir(j.ID)); err == nil {
			j.mounts = append(j.mounts, workloadDir(j.ID))
		}
	}
	return dir, nil
}

var envNameRE = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// validSecrets rejects secrets that could not be exported faithfully (bad names, NUL bytes).
func validSecrets(vars map[string]string, files map[string][]byte) error {
	for k, v := range vars {
		if !envNameRE.MatchString(k) {
			return fmt.Errorf("secret name %q is not a valid environment variable name", k)
		}
		if strings.ContainsRune(v, 0) {
			return fmt.Errorf("secret %s contains a NUL byte", k)
		}
	}
	for k := range files {
		if !envNameRE.MatchString(k) {
			return fmt.Errorf("secret file name %q is not a valid environment variable name", k)
		}
	}
	return nil
}

// writeSecrets puts secrets in a tmpfs directory for the unit wrapper to load ("" if none).
func writeSecrets(id string, vars map[string]string, files map[string][]byte) (string, error) {
	if len(vars) == 0 && len(files) == 0 {
		return "", nil
	}
	dir := secretsDir(id)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	if !memoryBacked(dir) {
		_ = os.RemoveAll(dir)
		return "", fmt.Errorf("%s is not on tmpfs/ramfs; refusing to write secrets to disk (run the runner as a systemd user service so XDG_RUNTIME_DIR is set)", dir)
	}
	ok := false
	defer func() {
		if !ok {
			_ = os.RemoveAll(dir) // no partial secrets left behind
		}
	}()
	all := map[string]string{}
	for k, v := range vars {
		all[k] = v
	}
	i := 0
	for k, content := range files {
		p := filepath.Join(dir, fmt.Sprintf("file-%d", i))
		i++
		if err := os.WriteFile(p, content, 0o600); err != nil {
			return "", fmt.Errorf("secret file %s: %w", k, err)
		}
		all[k] = p
	}
	b, _ := json.Marshal(all)
	if err := os.WriteFile(filepath.Join(dir, "env.json"), b, 0o600); err != nil {
		return "", err
	}
	ok = true
	return dir, nil
}

// memoryBacked reports whether path lives on tmpfs or ramfs (never written to disk).
func memoryBacked(path string) bool {
	var st unix.Statfs_t
	if err := unix.Statfs(path, &st); err != nil {
		return false
	}
	return st.Type == unix.TMPFS_MAGIC || st.Type == unix.RAMFS_MAGIC
}

// addGitConfig appends one GIT_CONFIG_KEY_n/VALUE_n pair (git's env-based config).
func addGitConfig(env map[string]string, key, value string) {
	n := 0
	fmt.Sscan(env["GIT_CONFIG_COUNT"], &n)
	env[fmt.Sprintf("GIT_CONFIG_KEY_%d", n)] = key
	env[fmt.Sprintf("GIT_CONFIG_VALUE_%d", n)] = value
	env["GIT_CONFIG_COUNT"] = fmt.Sprint(n + 1)
}

// ensureRelays (re)creates missing relay sockets of live workloads: after a runner restart, or
// when binding failed. Called every few seconds, so a transient failure doesn't disable a relay
// for good.
func (s *Server) ensureRelays() {
	type want struct {
		rl                     *relay
		kind, id               string
		agent, ctl, dial, cred bool
	}
	var todo []want
	s.mu.Lock()
	for _, j := range s.jobs {
		if j.State != api.StateRunning || j.relay == nil {
			continue
		}
		w := want{rl: j.relay, kind: "jobs", id: j.ID, agent: slices.Contains(j.Spec.Relay, "ssh-agent"),
			dial: j.Spec.Network == "client", cred: slices.Contains(j.Spec.Relay, "git")}
		w.ctl = j.Runtime == "container" && (w.dial || w.cred)
		todo = append(todo, w)
	}
	for _, se := range s.sessions {
		if !api.Terminal(se.State) && se.relay != nil {
			todo = append(todo, want{rl: se.relay, kind: "sessions", id: se.ID, ctl: true, dial: true})
		}
	}
	s.mu.Unlock()
	for _, w := range todo {
		w.rl.mu.Lock()
		needAgent := w.agent && w.rl.agent == nil && !w.rl.closed
		needCtl := w.ctl && w.rl.ctl == nil && !w.rl.closed
		w.rl.mu.Unlock()
		if needAgent {
			if _, err := w.rl.startAgent(w.id); err != nil {
				log.Printf("%s %s: ssh-agent socket: %v (retrying)", w.kind, w.id, err)
			}
		}
		if needCtl {
			if _, err := s.startControl(w.rl, w.kind, w.id, w.dial, w.cred); err != nil {
				log.Printf("%s %s: control socket: %v (retrying)", w.kind, w.id, err)
			}
		}
	}
}

// cleanupCredentials closes relays and deletes the job's secrets.
func (s *Server) cleanupCredentials(j *job) {
	if j.relay != nil {
		j.relay.close()
	}
	_ = os.RemoveAll(workloadDir(j.ID))
	j.secrets, j.secretFiles = nil, nil
	_ = os.RemoveAll(secretsDir(j.ID))
}

func (s *Server) listensUnix() bool {
	for _, l := range s.cfg.Listen {
		if l == "unix" {
			return true
		}
	}
	return false
}

// relayOwner is the job or session a relay request is about.
type relayOwner struct {
	id        string
	rl        *relay
	done      func() bool
	note      func(string)
	allowDial bool // the workload uses network = "client"
	allowCred bool // the workload enabled the git credential relay
}

// relayOf finds the relay of /v1/jobs/{id}/… or /v1/sessions/{id}/….
func (s *Server) relayOf(w http.ResponseWriter, r *http.Request) *relayOwner {
	id := r.PathValue("id")
	var o *relayOwner
	s.mu.Lock()
	if strings.HasPrefix(r.URL.Path, "/v1/sessions/") {
		if se := s.sessions[id]; se != nil && se.relay != nil {
			o = &relayOwner{id: id, rl: se.relay, allowDial: se.Network == "client",
				done: func() bool { s.mu.Lock(); defer s.mu.Unlock(); return api.Terminal(se.State) },
				note: func(msg string) { appendNote(filepath.Join(se.dir, "stderr.log"), msg) }}
		}
	} else if j := s.jobs[id]; j != nil && j.relay != nil {
		o = &relayOwner{id: id, rl: j.relay, allowDial: j.Spec.Network == "client", allowCred: slices.Contains(j.Spec.Relay, "git"),
			done: func() bool { s.mu.Lock(); defer s.mu.Unlock(); return api.Terminal(j.State) },
			note: func(msg string) { s.note(j, msg) }}
	}
	s.mu.Unlock() // never write a response while holding the scheduler lock
	if o == nil {
		writeErr(w, 404, "no such job or session, or it has no relay")
	}
	return o
}

func appendNote(path, msg string) {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		return
	}
	fmt.Fprintf(f, "offload: %s\n", msg)
	f.Close()
}

// handleCallbacks long-polls for the next credential request (client side of the relay).
func (s *Server) handleCallbacks(w http.ResponseWriter, r *http.Request) {
	o := s.relayOf(w, r)
	if o == nil {
		return
	}
	rl := o.rl
	rl.mu.Lock()
	if rl.closed {
		rl.mu.Unlock()
		w.WriteHeader(http.StatusGone)
		return
	}
	rl.polling++
	rl.mu.Unlock()
	defer func() {
		rl.mu.Lock()
		rl.polling--
		rl.lastPoll = time.Now()
		rl.mu.Unlock()
	}()
	deadline := time.After(25 * time.Second)
	for {
		rl.mu.Lock()
		for len(rl.queue) > 0 {
			cb := rl.queue[0]
			rl.queue = rl.queue[1:]
			if time.Now().After(cb.expires) {
				continue // its asker gave up; a late answer would act on a stale request
			}
			rl.mu.Unlock()
			writeJSON(w, cb)
			return
		}
		rl.mu.Unlock()
		if o.done() {
			w.WriteHeader(http.StatusGone)
			return
		}
		select {
		case <-rl.notify:
		case <-rl.done:
			w.WriteHeader(http.StatusGone)
			return
		case <-deadline:
			w.WriteHeader(http.StatusNoContent)
			return
		case <-r.Context().Done():
			return
		case <-time.After(time.Second):
		}
	}
}

func (s *Server) handleCallbackReply(w http.ResponseWriter, r *http.Request) {
	o := s.relayOf(w, r)
	if o == nil {
		return
	}
	rl := o.rl
	b, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	rl.mu.Lock()
	cb := rl.pending[r.PathValue("cid")]
	delete(rl.pending, r.PathValue("cid")) // one answer per request
	rl.mu.Unlock()
	if cb == nil {
		writeErr(w, 404, "no such request")
		return
	}
	select {
	case cb.resp <- b:
	default:
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleCredential is called by `offload runner credential` (git's credential helper inside a
// job) over the local socket only.
func (s *Server) handleCredential(w http.ResponseWriter, r *http.Request) {
	if r.Context().Value(unixMarker{}) == nil {
		writeErr(w, 403, "local socket only")
		return
	}
	o := s.relayOf(w, r)
	if o == nil {
		return
	}
	if !o.allowCred {
		writeErr(w, 403, "this workload did not enable the git credential relay")
		return
	}
	rl := o.rl
	in, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 64<<10))
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	out, err := rl.ask("git-credential", in, time.Minute)
	if err != nil {
		o.note("git credentials: " + err.Error())
		log.Printf("%s: git credential: %v", o.id, err)
		w.WriteHeader(http.StatusNoContent)
		return
	}
	w.Write(out)
}

// CredentialHelper is `offload runner credential <op>`: git's credential helper inside a job.
// Only "get" is answered; store/erase are no-ops (nothing is kept on the runner).
func CredentialHelper(op string, in io.Reader, out io.Writer) int {
	if op != "get" {
		_, _ = io.Copy(io.Discard, in)
		return 0
	}
	sock, id := os.Getenv("OFFLOAD_RUNNER_SOCK"), os.Getenv("OFFLOAD_JOB")
	if sock == "" || id == "" {
		return 0
	}
	client := &http.Client{Timeout: 90 * time.Second, Transport: &http.Transport{
		Dial: func(_, _ string) (net.Conn, error) { return net.Dial("unix", sock) },
	}}
	resp, err := client.Post("http://local/v1/jobs/"+id+"/credential", "text/plain", in)
	if err != nil {
		fmt.Fprintf(os.Stderr, "offload credential: %v\n", err)
		return 0
	}
	defer resp.Body.Close()
	if resp.StatusCode == 200 {
		b, _ := io.ReadAll(resp.Body)
		// Only pass through credential fields; never let the answer set other helper options.
		for _, line := range strings.Split(string(b), "\n") {
			k, _, ok := strings.Cut(line, "=")
			switch {
			case !ok:
			case k == "username", k == "password", k == "password_expiry_utc", k == "oauth_refresh_token", k == "authtype", k == "credential":
				fmt.Fprintln(out, line)
			}
		}
	}
	return 0
}
