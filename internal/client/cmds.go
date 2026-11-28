package client

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/RestartFU/offload/internal/api"
	"github.com/RestartFU/offload/internal/units"
)

func discover(ctx context.Context) ([]*Host, error) {
	cfg, err := LoadConfig()
	if err != nil {
		return nil, err
	}
	return Discover(ctx, cfg), nil
}

// Hosts prints every reachable runner and its load.
func Hosts() error {
	forgetHosts() // always rescan: this is how new runners show up
	hosts, err := discover(context.Background())
	if err != nil {
		return err
	}
	if len(hosts) == 0 {
		fmt.Println("no runners found")
		return nil
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "RUNNER\tURL\tCPU (alloc/cap)\tMEM (alloc/cap)\tHOST AVAIL\tLOAD\tJOBS\tSESSIONS\tPROFILES\tSTATE")
	for _, h := range hosts {
		st := h.Status
		var profs []string
		for name := range st.Profiles {
			profs = append(profs, name)
		}
		state := "ok"
		if h.derived && h.Token == "" {
			state = "not paired"
		}
		if !st.Accepting {
			state = st.Reason
		}
		fmt.Fprintf(tw, "%s\t%s\t%.1f/%.0f\t%s/%s\t%s\t%.1f\t%d run, %d queued\t%d\t%s\t%s\n",
			h.Name, h.URL, st.Allocated.CPU, st.Capacity.CPU,
			units.FormatSize(st.Allocated.Mem), units.FormatSize(st.Capacity.Mem),
			units.FormatSize(st.MemAvailable), st.Load1, st.Running, st.Queued, st.Sessions,
			strings.Join(sortedStrings(profs), ","), state)
	}
	return tw.Flush()
}

func sortedStrings(s []string) []string {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
	return s
}

// Ps lists recent jobs and live sessions on every runner.
func Ps(all bool) error {
	ctx := context.Background()
	hosts, err := discover(ctx)
	if err != nil {
		return err
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tSTATE\tAGE\tRES\tCOMMAND / PROFILE\tNOTE")
	q := ""
	if all {
		q = "?all=1"
	}
	for _, h := range hosts {
		var jobs []api.Job
		if err := h.do(ctx, "GET", "/v1/jobs"+q, nil, &jobs); err == nil {
			for _, j := range jobs {
				note := j.Reason
				if j.ExitCode != nil {
					note = strings.TrimSpace(fmt.Sprintf("exit %d %s", *j.ExitCode, note))
				}
				fmt.Fprintf(tw, "%s:%s\t%s\t%s\t%.0f/%.0f cpu %s\t%s\t%s\n", h.Name, j.ID, j.State,
					time.Since(j.CreatedAt).Round(time.Second), j.Resources.CPU, j.Resources.CPUMax,
					units.FormatSize(j.Resources.Mem), truncate(strings.Join(j.Spec.Argv, " "), 60), note)
			}
		}
		var sessions []api.Session
		if err := h.do(ctx, "GET", "/v1/sessions", nil, &sessions); err == nil {
			for _, s := range sessions {
				if api.Terminal(s.State) && !all {
					continue
				}
				fmt.Fprintf(tw, "%s:%s\t%s\t%s\t%.0f/%.0f cpu %s\tsession %s\t%d attached %s\n", h.Name, s.ID, s.State,
					time.Since(s.CreatedAt).Round(time.Second), s.Resources.CPU, s.Resources.CPUMax,
					units.FormatSize(s.Resources.Mem), s.Profile, s.Attached, s.Reason)
			}
		}
	}
	return tw.Flush()
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}

// Attach follows a job's output to the end and exits with its code. Ctrl-C detaches only.
func Attach(ref string) (int, error) {
	signal.Ignore(syscall.SIGPIPE) // a closed output ends following with a message, as in run
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	hosts, err := discover(ctx)
	if err != nil {
		return 1, err
	}
	h, id, err := locateJob(ctx, hosts, ref)
	if err == nil {
		err = checkProto(h)
	}
	if err != nil {
		return 1, err
	}
	cfg, _ := LoadConfig()
	code, done, err := follow(ctx, h, id, false, false, cfg, nil)
	if err == nil && done != nil {
		if werr := consumeWriteBack(ctx, h, done, false, false, nil); werr != nil {
			logf(false, "write-back: %v", werr)
			if code == 0 {
				code = 1 // the job's edits didn't all arrive
			}
		}
	}
	return code, err
}

// Cancel stops a job.
func Cancel(ref string) error {
	ctx := context.Background()
	hosts, err := discover(ctx)
	if err != nil {
		return err
	}
	h, id, err := locateJob(ctx, hosts, ref)
	if err != nil {
		return err
	}
	if strings.HasPrefix(id, "s-") {
		return h.do(ctx, "DELETE", "/v1/sessions/"+id, nil, nil)
	}
	return h.do(ctx, "DELETE", "/v1/jobs/"+id, nil, nil)
}

// Get downloads a finished job's artifacts.
func Get(ref, dest string) error {
	ctx := context.Background()
	hosts, err := discover(ctx)
	if err != nil {
		return err
	}
	h, id, err := locateJob(ctx, hosts, ref)
	if err != nil {
		return err
	}
	var j api.Job
	if err := h.do(ctx, "GET", "/v1/jobs/"+id, nil, &j); err != nil {
		return err
	}
	if !j.HasArtifact {
		if len(j.ArtifactsMissing) > 0 {
			return fmt.Errorf("the job produced none of its artifacts (%s)", strings.Join(j.ArtifactsMissing, ", "))
		}
		return fmt.Errorf("the job has no artifacts (did it run with --get, and has it finished?)")
	}
	if _, err := fetchArtifacts(ctx, h, id, dest, "", nil); err != nil {
		return err
	}
	if len(j.ArtifactsMissing) > 0 {
		return fmt.Errorf("the job didn't produce %s (the other artifacts were downloaded)", strings.Join(j.ArtifactsMissing, ", "))
	}
	return nil
}

// Upgrade pushes a binary (this one by default) to runners, which swap it in and restart; running
// jobs continue. A runner of another OS/arch needs a matching build passed with --binary.
func Upgrade(names []string, all bool, binary string, force bool) error {
	ctx := context.Background()
	hosts, err := discover(ctx)
	if err != nil {
		return err
	}
	self := binary
	if self == "" {
		if self, err = os.Executable(); err != nil {
			return err
		}
	}
	var targets []*Host
	if all {
		targets = hosts
	}
	for _, n := range names {
		h, err := FindHost(hosts, n)
		if err != nil {
			return err
		}
		targets = append(targets, h)
	}
	if len(targets) == 0 {
		return fmt.Errorf("name a runner or pass --all")
	}
	for _, h := range targets {
		if binary == "" && h.Status.OS != "" && (h.Status.OS != goruntime.GOOS || h.Status.Arch != goruntime.GOARCH) {
			return fmt.Errorf("%s is %s/%s; build a matching binary (CGO_ENABLED=0 GOOS=%s GOARCH=%s go build) and pass --binary",
				h.Name, h.Status.OS, h.Status.Arch, h.Status.OS, h.Status.Arch)
		}
		// A runner of another protocol version stops workloads started by its old version when it
		// restarts: say so before doing it.
		// For this binary we know the protocol; a --binary candidate is checked by the runner.
		if busy := h.Status.Running + h.Status.Sessions; binary == "" && busy > 0 && h.Status.Proto != api.Proto && !force {
			return fmt.Errorf("%s has %d running job(s)/session(s) that the upgrade would stop (protocol %d -> %d); wait, or pass --force", h.Name, busy, h.Status.Proto, api.Proto)
		}
		f, err := os.Open(self)
		if err != nil {
			return err
		}
		q := ""
		if force {
			q = "?force=1"
		}
		err = h.do(ctx, "PUT", "/v1/admin/binary"+q, f, nil)
		f.Close()
		if err != nil {
			return fmt.Errorf("%s: %w", h.Name, err)
		}
		fmt.Printf("%s: upgraded (was %s)\n", h.Name, h.Status.Version)
	}
	return nil
}

// Pair registers this client's token for each runner. A runner that hasn't paired this client
// yet wants its one-time code (`offload runner pair-code` on it); re-pairing a paired runner
// needs none.
func Pair(names []string, all bool, code string) error {
	ctx := context.Background()
	hosts, err := discover(ctx)
	if err != nil {
		return err
	}
	var targets []*Host
	if all {
		targets = hosts
	}
	for _, n := range names {
		h, err := FindHost(hosts, n)
		if err != nil {
			return err
		}
		targets = append(targets, h)
	}
	if len(targets) == 0 {
		return fmt.Errorf("name a runner or pass --all")
	}
	if code != "" && len(targets) > 1 {
		return fmt.Errorf("a pairing code belongs to one runner: name it")
	}
	for _, h := range targets {
		if h.sock != "" {
			fmt.Printf("%s: local, nothing to pair\n", h.Name)
			continue
		}
		if !h.derived {
			fmt.Printf("%s: its token is set in client.toml, nothing to pair\n", h.Name)
			continue
		}
		unlock, err := lockFile(runnerTokenPath(h.URL) + ".lock") // one pairing per runner at a time
		if err != nil {
			return err
		}
		err = pairOne(ctx, h, code)
		unlock()
		if err != nil {
			return fmt.Errorf("%s: %w", h.Name, err)
		}
	}
	return nil
}

// pairOne pairs with one runner; the caller holds its lock.
func pairOne(ctx context.Context, h *Host, code string) error {
	pending := runnerTokenPath(h.URL) + ".pending"
	h.Token = storedToken(h.URL) // re-read under the lock: a concurrent pair may have rotated it
	// A pending token is left by an attempt whose answer never arrived: the runner may have
	// switched to it. Ask with it, through the pair endpoint (it works across protocol versions
	// and is a no-op for a token already paired).
	if b, err := os.ReadFile(pending); err == nil {
		probe := *h
		probe.Token = strings.TrimSpace(string(b))
		sum := sha256.Sum256([]byte(probe.Token))
		var ae *APIError
		var out map[string]string
		switch err := probe.do(ctx, "POST", "/v1/admin/pair", map[string]any{"hash": hex.EncodeToString(sum[:])}, &out); {
		case err == nil:
			if err := os.Rename(pending, runnerTokenPath(h.URL)); err != nil {
				return err
			}
			h.Token = probe.Token
		case errors.As(err, &ae) && ae.Code == 403:
			_ = os.Remove(pending) // never accepted
		default:
			return fmt.Errorf("can't tell whether the last pairing took effect (%v); try again", err)
		}
	}
	{
		// Every pairing mints a fresh token, so re-pairing after a reset never revives an old one.
		// The request authenticates with what we have (the stored token, or an older client's
		// derived one); the runner then retires that.
		fresh, err := randomHex(32)
		if err != nil {
			return err
		}
		auth := *h
		if auth.Token == "" {
			auth.Token = legacyToken(h.URL)
		}
		sum := sha256.Sum256([]byte(fresh))
		body := map[string]any{"hash": hex.EncodeToString(sum[:]), "code": code, "rotate": true}
		// Saved before asking, so a runner that accepted it (and retired the old one) never
		// leaves us without a usable token.
		if err := os.MkdirAll(filepath.Dir(pending), 0o700); err != nil {
			return err
		}
		if err := os.WriteFile(pending, []byte(fresh+"\n"), 0o600); err != nil {
			return err
		}
		var out map[string]string
		if err := auth.do(ctx, "POST", "/v1/admin/pair", body, &out); err != nil {
			var ae *APIError
			if errors.As(err, &ae) { // the runner answered: it did not take the new token
				os.Remove(pending)
			} // otherwise keep it: the next `host pair` checks whether it was taken
			return err
		}
		if err := os.Rename(pending, runnerTokenPath(h.URL)); err != nil {
			return fmt.Errorf("paired; the new token is in %s (rename failed: %v)", pending, err)
		}
		fmt.Printf("%s: %s\n", h.Name, out["status"])
	}
	return nil
}

// lockFileCtx is lockFile, giving up when ctx ends (Ctrl-C while another process holds it).
func lockFileCtx(ctx context.Context, path string) (func(), error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	for {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return func() { _ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN); f.Close() }, nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) {
			f.Close()
			return nil, err
		}
		select {
		case <-ctx.Done():
			f.Close()
			return nil, ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
	}
}

// lockFile takes an exclusive lock on path (created if needed) and returns its release.
func lockFile(path string) (func(), error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		f.Close()
		return nil, err
	}
	return func() { _ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN); f.Close() }, nil
}
