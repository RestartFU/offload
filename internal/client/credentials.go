package client

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/RestartFU/offload/internal/api"
)

// CredentialsConfig is the [credentials] table of client.toml.
//
//	[credentials]
//	relay = ["ssh-agent", "git"]   # answered from this machine, only while a client is attached
//	git_hosts = ["github.com"]     # hosts git may get credentials for (empty = any)
//	git_config = ["url.*", "user.name", "user.email"]   # mirrored into jobs (default)
//
//	[credentials.sets.github]
//	auto = true                    # attach to every job; no flags needed
//	env = { GH_TOKEN = "cmd:gh auth token" }
//
//	[credentials.sets.npm]
//	files = { NPM_CONFIG_USERCONFIG = "~/.npmrc" }
//
// Values: "cmd:<shell command>" (its stdout), "env:<NAME>", "file:<path>", or a literal.
type CredentialsConfig struct {
	Relay     *[]string                `toml:"relay"`
	GitHosts  []string                 `toml:"git_hosts"`
	GitConfig *[]string                `toml:"git_config"` // client git config keys mirrored into jobs (globs)
	Sets      map[string]CredentialSet `toml:"sets"`
}

// CredentialSet is a named group of temporary secrets. Auto sets go with every job; others only
// with --with NAME.
type CredentialSet struct {
	Auto  bool              `toml:"auto"`
	Env   map[string]string `toml:"env"`
	Files map[string]string `toml:"files"`
}

func (c *Config) relays() []string {
	if c.Credentials.Relay != nil {
		return *c.Credentials.Relay
	}
	return []string{"ssh-agent", "git"}
}

// mirroredGitConfig reads the client's git config keys matching credentials.git_config, so that
// URL rewrites (e.g. https -> ssh) and identity behave on the runner as they do here.
func mirroredGitConfig(cfg *Config, dir string) [][2]string {
	pats := []string{"url.*", "user.name", "user.email"}
	if cfg.Credentials.GitConfig != nil {
		pats = *cfg.Credentials.GitConfig
	}
	if len(pats) == 0 {
		return nil
	}
	out, err := exec.Command("git", "-C", dir, "config", "--null", "--list").Output()
	if err != nil {
		return nil
	}
	var kv [][2]string
	for _, entry := range strings.Split(string(out), "\x00") {
		k, v, _ := strings.Cut(entry, "\n")
		if k == "" {
			continue
		}
		for _, p := range pats {
			if configKeyMatch(strings.ToLower(p), k) {
				kv = append(kv, [2]string{k, v})
				break
			}
		}
	}
	return kv
}

// configKeyMatch matches a git config key against "exact.key" or "prefix.*" (where * spans
// anything, including the dots and slashes inside URL subsections).
func configKeyMatch(pattern, key string) bool {
	if prefix, ok := strings.CutSuffix(pattern, "*"); ok {
		return strings.HasPrefix(key, prefix)
	}
	return pattern == key
}

func expand(p string) string {
	if strings.HasPrefix(p, "~/") {
		home, _ := os.UserHomeDir()
		return filepath.Join(home, p[2:])
	}
	return p
}

// resolveValue turns a credential source into its value, on this machine.
func resolveValue(src string) (string, error) {
	switch {
	case strings.HasPrefix(src, "cmd:"):
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		out, err := groupCommand(ctx, "sh", "-c", strings.TrimPrefix(src, "cmd:")).Output()
		if err != nil {
			return "", fmt.Errorf("%s: %w", strings.TrimPrefix(src, "cmd:"), err)
		}
		return strings.TrimRight(string(out), "\r\n"), nil
	case strings.HasPrefix(src, "env:"):
		v, ok := os.LookupEnv(strings.TrimPrefix(src, "env:"))
		if !ok {
			return "", fmt.Errorf("$%s is not set", strings.TrimPrefix(src, "env:"))
		}
		return v, nil
	case strings.HasPrefix(src, "file:"):
		b, err := os.ReadFile(expand(strings.TrimPrefix(src, "file:")))
		return string(b), err
	}
	return src, nil
}

// resolveSet resolves one credential set on this machine.
func resolveSet(name string, set CredentialSet, env map[string]string, files map[string][]byte) error {
	for k, src := range set.Env {
		v, err := resolveValue(src)
		if err != nil {
			return fmt.Errorf("credential set %s: %s: %w", name, k, err)
		}
		env[k] = v
	}
	for k, p := range set.Files {
		b, err := os.ReadFile(expand(p))
		if err != nil {
			return fmt.Errorf("credential set %s: %s: %w", name, k, err)
		}
		files[k] = b
	}
	return nil
}

// resolveSecrets collects a job's or session's secrets. Every secret is NAME = source, where a
// source is "cmd:<shell>", "env:<VAR>", "file:<path>" or a literal; file secrets arrive on the
// runner as a file whose path is put in NAME. They come from, in order:
//
//   - credential sets marked auto = true in client.toml (skipped with a warning if they fail);
//   - sets named by --with or OFFLOAD_WITH=a,b;
//   - OFFLOAD_SECRET_<NAME>=<source> and OFFLOAD_SECRET_FILE_<NAME>=<path> in the environment,
//     so an MCP server entry's "env" block (or any wrapper) can configure them;
//   - --secret NAME (copies $NAME) or --secret NAME=<source>.
func resolveSecrets(cfg *Config, sets, args []string, withAuto, quiet bool) (map[string]string, map[string][]byte, error) {
	env := map[string]string{}
	files := map[string][]byte{}
	var auto []string
	for name, set := range cfg.Credentials.Sets {
		if set.Auto && withAuto {
			auto = append(auto, name)
		}
	}
	sort.Strings(auto)
	for _, name := range auto {
		e, f := map[string]string{}, map[string][]byte{}
		if err := resolveSet(name, cfg.Credentials.Sets[name], e, f); err != nil {
			logf(quiet, "skipping %v", err)
			continue
		}
		for k, v := range e {
			env[k] = v
		}
		for k, v := range f {
			files[k] = v
		}
	}
	if w := os.Getenv("OFFLOAD_WITH"); w != "" {
		for _, n := range strings.Split(w, ",") {
			if n = strings.TrimSpace(n); n != "" {
				sets = append(sets, n)
			}
		}
	}
	for _, name := range sets {
		set, ok := cfg.Credentials.Sets[name]
		if !ok {
			var known []string
			for k := range cfg.Credentials.Sets {
				known = append(known, k)
			}
			sort.Strings(known)
			return nil, nil, fmt.Errorf("no credential set %q in client.toml (have: %s)", name, strings.Join(known, ", "))
		}
		if err := resolveSet(name, set, env, files); err != nil {
			return nil, nil, err
		}
	}
	for _, kv := range os.Environ() {
		k, v, _ := strings.Cut(kv, "=")
		switch {
		case strings.HasPrefix(k, "OFFLOAD_SECRET_FILE_"):
			name := strings.TrimPrefix(k, "OFFLOAD_SECRET_FILE_")
			b, err := os.ReadFile(expand(strings.TrimPrefix(v, "file:")))
			if err != nil {
				return nil, nil, fmt.Errorf("%s: %w", k, err)
			}
			files[name] = b
		case strings.HasPrefix(k, "OFFLOAD_SECRET_"):
			name := strings.TrimPrefix(k, "OFFLOAD_SECRET_")
			val, err := resolveValue(v)
			if err != nil {
				return nil, nil, fmt.Errorf("%s: %w", k, err)
			}
			env[name] = val
		}
	}
	for _, a := range args {
		name, src, hasSrc := strings.Cut(a, "=")
		if !hasSrc {
			src = "env:" + name
		}
		val, err := resolveValue(src)
		if err != nil {
			return nil, nil, fmt.Errorf("--secret %s: %w", name, err)
		}
		env[name] = val
	}
	return env, files, nil
}

// serveCallbacks answers the job's credential requests from this machine until ctx ends or the
// job finishes. Each kind of use is announced once on stderr, so the caller knows it happened.
// base is "/v1/jobs/<id>" or "/v1/sessions/<id>".
// allow lists the callback kinds this workload enabled ("dial", "ssh-agent", "git-credential");
// anything else is refused here too, whatever the runner forwards.
func serveCallbacks(ctx context.Context, h *Host, base string, cfg *Config, quiet bool, allow map[string]bool) {
	// Everything dispatched below (key prompts, credential helpers, dials) is cancelled when the
	// workload ends (the poll returns) or the caller stops serving it.
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var once sync.Map
	announce := func(key, msg string) {
		if _, loaded := once.LoadOrStore(key, true); !loaded {
			logf(quiet, "%s", msg)
		}
	}
	for ctx.Err() == nil {
		var cb struct {
			ID   string `json:"id"`
			Kind string `json:"kind"`
			Data []byte `json:"data"`
		}
		resp, err := h.request(ctx, "GET", base+"/callbacks", nil)
		if err != nil {
			var ae *APIError
			if errors.As(err, &ae) && (ae.Code == 404 || ae.Code == 410) {
				return
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(2 * time.Second):
			}
			continue
		}
		switch resp.StatusCode {
		case http.StatusGone:
			resp.Body.Close()
			return
		case http.StatusNoContent:
			resp.Body.Close()
			continue
		}
		err = decodeJSON(resp.Body, &cb)
		resp.Body.Close()
		if err != nil {
			continue
		}
		go func() {
			var answer []byte
			if !allow[cb.Kind] {
				announce("deny:"+cb.Kind, "refused a "+cb.Kind+" request the job did not enable")
				_ = h.do(ctx, "POST", base+"/callbacks/"+cb.ID, bytes.NewReader(nil), nil)
				return
			}
			switch cb.Kind {
			case "dial":
				answer = []byte(serveDial(ctx, h, cb.Data, cfg))
				announce("net", "the job's network goes through this machine")
			case "ssh-agent":
				answer = agentRequest(ctx, cb.Data)
				announce("ssh", "the job is using this machine's ssh-agent")
			case "git-credential":
				req, err := parseCredentialRequest(cb.Data)
				host := req.Host
				if err != nil {
					announce("git-bad", "refused a malformed git credential request: "+err.Error())
				} else if allowedHost(cfg.Credentials.GitHosts, host) {
					answer = gitCredentialFill(ctx, req.canonical())
					if len(answer) > 0 {
						announce("git:"+host, "lent git credentials for "+host+" to the job")
					}
				} else {
					announce("git-deny:"+host, "refused git credentials for "+host+" (not in credentials.git_hosts)")
				}
			}
			_ = h.do(ctx, "POST", base+"/callbacks/"+cb.ID, bytes.NewReader(answer), nil)
		}()
	}
}

// agentRequest forwards one framed SSH agent message to the local agent.
func agentRequest(ctx context.Context, msg []byte) []byte {
	sock := os.Getenv("SSH_AUTH_SOCK")
	if sock == "" || !api.AgentRequestAllowed(msg) {
		return nil // a job may list keys and sign, never add, remove, lock or extend
	}
	d := net.Dialer{Timeout: 5 * time.Second}
	c, err := d.DialContext(ctx, "unix", sock)
	if err != nil {
		return nil
	}
	defer c.Close()
	defer context.AfterFunc(ctx, func() { c.Close() })() // the workload ended: abandon the request
	_ = c.SetDeadline(time.Now().Add(2 * time.Minute))   // hardware keys may need a touch
	if _, err := c.Write(msg); err != nil {
		return nil
	}
	var hdr [4]byte
	if _, err := io.ReadFull(c, hdr[:]); err != nil {
		return nil
	}
	n := binary.BigEndian.Uint32(hdr[:])
	if n > 256<<10 {
		return nil
	}
	out := make([]byte, 4+n)
	copy(out, hdr[:])
	if _, err := io.ReadFull(c, out[4:]); err != nil {
		return nil
	}
	return out
}

// credentialRequest is a git credential request reduced to the fields we authorize on. Git lets a
// later "host=" (or a "url=") override an earlier one, so the job's raw text is never passed on:
// git gets a request rebuilt from exactly the fields that were checked.
type credentialRequest struct {
	Protocol, Host, Path, Username string
}

func parseCredentialRequest(data []byte) (credentialRequest, error) {
	var r credentialRequest
	seen := map[string]bool{}
	for _, line := range strings.Split(strings.TrimRight(string(data), "\n"), "\n") {
		if line == "" {
			break // end of request
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok || strings.ContainsAny(v, "\x00\r") {
			return r, fmt.Errorf("bad line")
		}
		switch k {
		case "protocol", "host", "path", "username":
			if seen[k] {
				return r, fmt.Errorf("duplicate %s", k)
			}
			seen[k] = true
		case "url":
			return r, fmt.Errorf("url= requests are not relayed")
		default:
			continue // capability[], wwwauth[] and the like are dropped
		}
		switch k {
		case "protocol":
			r.Protocol = v
		case "host":
			r.Host = v
		case "path":
			r.Path = v
		case "username":
			r.Username = v
		}
	}
	if r.Host == "" || r.Protocol == "" {
		return r, fmt.Errorf("missing protocol or host")
	}
	return r, nil
}

func (r credentialRequest) canonical() []byte {
	var b strings.Builder
	fmt.Fprintf(&b, "protocol=%s\nhost=%s\n", r.Protocol, r.Host)
	if r.Path != "" {
		fmt.Fprintf(&b, "path=%s\n", r.Path)
	}
	if r.Username != "" {
		fmt.Fprintf(&b, "username=%s\n", r.Username)
	}
	b.WriteString("\n")
	return []byte(b.String())
}

func credentialField(data []byte, key string) string {
	for _, line := range strings.Split(string(data), "\n") {
		if k, v, ok := strings.Cut(line, "="); ok && k == key {
			return v
		}
	}
	return ""
}

func allowedHost(allow []string, host string) bool {
	if host == "" {
		return false
	}
	if len(allow) == 0 {
		return true
	}
	for _, a := range allow {
		if ok, _ := path.Match(a, host); ok {
			return true
		}
	}
	return false
}

// groupCommand runs a helper in its own process group: on timeout or cancel the whole group is
// killed (helpers spawn helpers that would otherwise hold the output pipe open), and Wait gives
// up shortly after.
func groupCommand(ctx context.Context, name string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = 2 * time.Second
	return cmd
}

// gitCredentialFill asks this machine's git for a credential, never prompting.
func gitCredentialFill(ctx context.Context, req []byte) []byte {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	cmd := groupCommand(ctx, "git", "credential", "fill")
	cmd.Stdin = bytes.NewReader(req)
	var env []string
	for _, e := range os.Environ() {
		if !strings.HasPrefix(e, "GIT_ASKPASS=") && !strings.HasPrefix(e, "SSH_ASKPASS=") {
			env = append(env, e)
		}
	}
	cmd.Env = append(env, "GIT_TERMINAL_PROMPT=0", "GCM_INTERACTIVE=never")
	out, err := cmd.Output()
	if err != nil || credentialField(out, "password") == "" {
		return nil // nothing usable; never lend an empty login
	}
	return out
}
