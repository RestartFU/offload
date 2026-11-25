package runner

import (
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
	"strings"
	"sync"
	"time"

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
	ID   string `json:"id"`
	Kind string `json:"kind"` // "ssh-agent" or "git-credential"
	Data []byte `json:"data"`
	resp chan []byte
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
}

func newRelay() *relay {
	return &relay{pending: map[string]*callback{}, notify: make(chan struct{}, 1)}
}

func (r *relay) attached() bool {
	return r.polling > 0 || time.Since(r.lastPoll) < 10*time.Second
}

// ask sends a request to the client and waits for its answer.
func (r *relay) ask(kind string, data []byte, timeout time.Duration) ([]byte, error) {
	r.mu.Lock()
	if !r.attached() {
		r.mu.Unlock()
		return nil, errNoClient
	}
	cb := &callback{ID: newID("c-"), Kind: kind, Data: data, resp: make(chan []byte, 1)}
	r.queue = append(r.queue, cb)
	r.pending[cb.ID] = cb
	r.mu.Unlock()
	select {
	case r.notify <- struct{}{}:
	default:
	}
	defer func() { r.mu.Lock(); delete(r.pending, cb.ID); r.mu.Unlock() }()
	select {
	case b := <-cb.resp:
		return b, nil
	case <-time.After(timeout):
		return nil, fmt.Errorf("client did not answer %s request in %s", kind, timeout)
	}
}

func (r *relay) close() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.agent != nil {
		r.agent.Close()
		_ = os.Remove(r.agent.Addr().String())
		r.agent = nil
	}
}

func runtimeDir() string { return filepath.Dir(unixSocketPath()) }

func secretsDir(id string) string { return filepath.Join(runtimeDir(), "secrets", id) }

// startAgent serves an SSH agent socket for the job that relays every message to the client.
// The agent protocol is strict request/response with 4-byte length framing.
func (r *relay) startAgent(id string) (string, error) {
	path := filepath.Join(runtimeDir(), id+".agent")
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
func (s *Server) setupCredentials(j *job, env map[string]string) string {
	if j.relay != nil {
		for _, kind := range j.Spec.Relay {
			switch kind {
			case "ssh-agent":
				path, err := j.relay.startAgent(j.ID)
				if err != nil {
					s.note(j, "ssh-agent relay unavailable: "+err.Error())
					continue
				}
				env["SSH_AUTH_SOCK"] = path
				if _, ok := env["GIT_SSH_COMMAND"]; !ok {
					env["GIT_SSH_COMMAND"] = "ssh -o StrictHostKeyChecking=accept-new"
				}
			case "git":
				if !s.listensUnix() {
					s.note(j, "git credential relay needs the runner's unix socket (listen = [..., \"unix\"])")
					continue
				}
				addGitConfig(env, "credential.helper", "") // drop the runner's own helpers
				addGitConfig(env, "credential.helper", "!'"+s.self+"' runner credential")
				env["OFFLOAD_RUNNER_SOCK"] = unixSocketPath()
			}
		}
	}
	for _, kv := range j.Spec.GitConfig {
		addGitConfig(env, kv[0], kv[1])
	}
	dir, err := writeSecrets(j.ID, j.secrets, j.secretFiles)
	if err != nil {
		s.note(j, "secrets unavailable: "+err.Error())
	}
	j.secrets, j.secretFiles = nil, nil // from here on they only exist in the tmpfs directory
	return dir
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
	return dir, nil
}

// addGitConfig appends one GIT_CONFIG_KEY_n/VALUE_n pair (git's env-based config).
func addGitConfig(env map[string]string, key, value string) {
	n := 0
	fmt.Sscan(env["GIT_CONFIG_COUNT"], &n)
	env[fmt.Sprintf("GIT_CONFIG_KEY_%d", n)] = key
	env[fmt.Sprintf("GIT_CONFIG_VALUE_%d", n)] = value
	env["GIT_CONFIG_COUNT"] = fmt.Sprint(n + 1)
}

// cleanupCredentials closes relays and deletes the job's secrets.
func (s *Server) cleanupCredentials(j *job) {
	if j.relay != nil {
		j.relay.close()
	}
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
	id   string
	rl   *relay
	done func() bool
	note func(string)
}

// relayOf finds the relay of /v1/jobs/{id}/… or /v1/sessions/{id}/….
func (s *Server) relayOf(w http.ResponseWriter, r *http.Request) *relayOwner {
	id := r.PathValue("id")
	s.mu.Lock()
	defer s.mu.Unlock()
	if strings.HasPrefix(r.URL.Path, "/v1/sessions/") {
		if se := s.sessions[id]; se != nil && se.relay != nil {
			return &relayOwner{id: id, rl: se.relay,
				done: func() bool { s.mu.Lock(); defer s.mu.Unlock(); return api.Terminal(se.State) },
				note: func(msg string) { appendNote(filepath.Join(se.dir, "stderr.log"), msg) }}
		}
	} else if j := s.jobs[id]; j != nil && j.relay != nil {
		return &relayOwner{id: id, rl: j.relay,
			done: func() bool { s.mu.Lock(); defer s.mu.Unlock(); return api.Terminal(j.State) },
			note: func(msg string) { s.note(j, msg) }}
	}
	writeErr(w, 404, "no such job or session, or it has no relay")
	return nil
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
		if len(rl.queue) > 0 {
			cb := rl.queue[0]
			rl.queue = rl.queue[1:]
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
	rl.mu.Unlock()
	if cb == nil {
		writeErr(w, 404, "no such request")
		return
	}
	cb.resp <- b
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
