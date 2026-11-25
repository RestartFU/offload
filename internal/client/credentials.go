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
	"time"
)

// CredentialsConfig is the [credentials] table of client.toml.
//
//	[credentials]
//	relay = ["ssh-agent", "git"]   # answered from this machine, only while a client is attached
//	git_hosts = ["github.com"]     # hosts git may get credentials for (empty = any)
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
	Relay    *[]string                `toml:"relay"`
	GitHosts []string                 `toml:"git_hosts"`
	Sets     map[string]CredentialSet `toml:"sets"`
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
		out, err := exec.CommandContext(ctx, "sh", "-c", strings.TrimPrefix(src, "cmd:")).Output()
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

// resolveSecrets builds a job's secrets from the auto sets, --with sets and --secret names.
// An auto set that can't be resolved (say, gh isn't logged in) is skipped with a warning; an
// explicitly requested one is an error.
func resolveSecrets(cfg *Config, sets, names []string, quiet bool) (map[string]string, map[string][]byte, error) {
	env := map[string]string{}
	files := map[string][]byte{}
	var auto []string
	for name, set := range cfg.Credentials.Sets {
		if set.Auto {
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
	for _, n := range names {
		v, ok := os.LookupEnv(n)
		if !ok {
			return nil, nil, fmt.Errorf("--secret %s: not set in this environment", n)
		}
		env[n] = v
	}
	return env, files, nil
}

// serveCallbacks answers the job's credential requests from this machine until ctx ends or the
// job finishes. Each kind of use is announced once on stderr, so the caller knows it happened.
func serveCallbacks(ctx context.Context, h *Host, id string, cfg *Config, quiet bool) {
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
		resp, err := h.request(ctx, "GET", "/v1/jobs/"+id+"/callbacks", nil)
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
			switch cb.Kind {
			case "ssh-agent":
				answer = agentRequest(cb.Data)
				announce("ssh", "the job is using this machine's ssh-agent")
			case "git-credential":
				host := credentialField(cb.Data, "host")
				if allowedHost(cfg.Credentials.GitHosts, host) {
					answer = gitCredentialFill(cb.Data)
					if len(answer) > 0 {
						announce("git:"+host, "lent git credentials for "+host+" to the job")
					}
				} else {
					announce("git-deny:"+host, "refused git credentials for "+host+" (not in credentials.git_hosts)")
				}
			}
			_ = h.do(ctx, "POST", "/v1/jobs/"+id+"/callbacks/"+cb.ID, bytes.NewReader(answer), nil)
		}()
	}
}

// agentRequest forwards one framed SSH agent message to the local agent.
func agentRequest(msg []byte) []byte {
	sock := os.Getenv("SSH_AUTH_SOCK")
	if sock == "" {
		return nil
	}
	c, err := net.DialTimeout("unix", sock, 5*time.Second)
	if err != nil {
		return nil
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(2 * time.Minute)) // hardware keys may need a touch
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

// gitCredentialFill asks this machine's git for a credential, never prompting.
func gitCredentialFill(req []byte) []byte {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", "credential", "fill")
	cmd.Stdin = bytes.NewReader(req)
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "GCM_INTERACTIVE=never", "GIT_ASKPASS=true", "SSH_ASKPASS=true")
	out, err := cmd.Output()
	if err != nil {
		return nil
	}
	return out
}
