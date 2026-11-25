package client

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/RestartFU/offload/internal/api"
	"github.com/RestartFU/offload/internal/units"
)

// RunOptions is `offload run`.
type RunOptions struct {
	On          string
	Profile     string
	Resources   api.Resources
	Env         map[string]string
	Artifacts   []string
	NoSync      bool
	NoWriteBack bool
	NoGit       bool
	With        []string // extra credential sets
	Secrets     []string // local env vars to pass as secrets
	NoCreds     bool     // no relays, no auto secrets
	Environment string   // named environment (~/.config/offload/envs/NAME)
	Host        bool     // run on the runner host instead of a container
	Platform    string   // require e.g. linux/arm64
	Network     string   // "client": the job's connections go out from this machine
	Fresh       bool
	Ephemeral   bool
	Detach      bool
	Quiet       bool
	Argv        []string
}

func logf(quiet bool, format string, args ...any) {
	if !quiet {
		fmt.Fprintf(os.Stderr, "offload: "+format+"\n", args...)
	}
}

// inJob reports whether we're already running inside an offload job (a script that calls
// `offload run` was itself offloaded). Then the command simply runs here.
func inJob() bool { return os.Getenv("OFFLOAD_JOB") != "" }

func runInPlace(argv []string, env map[string]string) (int, error) {
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	cmd.Env = os.Environ()
	for k, v := range env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	signal.Ignore(os.Interrupt) // the child gets the terminal's signals itself
	err := cmd.Run()
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		if ws, ok := ee.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
			return 128 + int(ws.Signal()), nil
		}
		return ee.ExitCode(), nil
	}
	if err != nil {
		return 127, err
	}
	return 0, nil
}

// Run sends a command to the best runner and streams its output; the return value is the
// command's exit code. The caller never picks a runner, profile or memory size: the runner
// list is cached, a busy runner is skipped, the runner picks a profile from the command and a
// memory limit from earlier runs, and an out-of-memory kill is retried once with twice the memory.
// Ctrl-C (or SIGTERM) cancels the remote job.
func Run(o RunOptions) (int, error) {
	if inJob() && o.On == "" {
		return runInPlace(o.Argv, o.Env)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	defer stop()
	cfg, err := LoadConfig()
	if err != nil {
		return 1, err
	}
	spec := api.JobSpec{Argv: o.Argv, Env: o.Env, Profile: o.Profile, Resources: o.Resources,
		Artifacts: o.Artifacts, Fresh: o.Fresh, Ephemeral: o.Ephemeral, Client: clientLabel(), Detached: o.Detach}
	// --no-credentials drops the relays and auto sets; explicit --with/--secret still apply.
	if o.NoSync && o.Environment != "" {
		if spec.Environment, err = LoadEnvDef("", o.Environment); err != nil {
			return 1, err
		}
	}
	if o.Host {
		spec.Runtime, spec.Environment = "host", nil
	}
	spec.Platform = o.Platform
	spec.Network = o.Network
	if spec.Network == "client" {
		spec.DNSSearch = dnsSearch()
	}
	if !o.NoCreds {
		spec.Relay = cfg.relays()
		cwd, _ := os.Getwd()
		spec.GitConfig = mirroredGitConfig(cfg, cwd)
	}
	if spec.Secrets, spec.SecretFiles, err = resolveSecrets(cfg, o.With, o.Secrets, !o.NoCreds, o.Quiet); err != nil {
		return 1, err
	}
	var ws *Workspace
	if !o.NoSync {
		cwd, _ := os.Getwd()
		start := time.Now()
		if ws, err = LoadWorkspace(cwd); err != nil {
			return 1, fmt.Errorf("reading workspace: %w", err)
		}
		spec.WorkspaceKey, spec.Manifest, spec.Dir = ws.Key, ws.Manifest, ws.Dir
		spec.WriteBack = !o.NoWriteBack
		if spec.Environment, err = LoadEnvDef(ws.Root, o.Environment); err != nil {
			return 1, err
		}
		if !o.NoGit {
			spec.Git = ws.Git
		}
		if d := time.Since(start); d > 2*time.Second {
			logf(o.Quiet, "scanned %d files in %s", len(ws.Manifest), d.Round(100*time.Millisecond))
		}
	}

	for attempt := 0; ; attempt++ {
		h, j, err := submit(ctx, cfg, o, spec, ws)
		if err != nil {
			if ctx.Err() != nil {
				return 130, nil
			}
			return 1, err
		}
		ref := h.Name + ":" + j.ID
		if o.Detach {
			fmt.Println(ref)
			return 0, nil
		}
		code, done, err := follow(ctx, h, j.ID, o.Quiet, true, cfg)
		if err != nil || done == nil {
			return code, err
		}
		j = *done
		// The runner chose the memory limit and it was too small: retry once with double.
		if j.State == api.StateFailed && strings.HasPrefix(j.Reason, "out of memory") && j.AutoMem && attempt == 0 {
			next := j.Resources.Mem * 2
			if h.Status != nil && next > h.Status.Capacity.Mem {
				next = h.Status.Capacity.Mem
			}
			if next > j.Resources.Mem {
				logf(false, "%s ran out of memory at %s; retrying with %s", ref, units.FormatSize(j.Resources.Mem), units.FormatSize(next))
				spec.Resources.Mem = next
				spec.Fresh = false
				continue
			}
		}
		finish(ctx, h, &j, ws, o)
		return code, nil
	}
}

// submit picks a runner, creates the job and uploads the changed source. A runner whose
// workspace slots are all busy is skipped for the next best one; if every runner is busy it waits.
func submit(ctx context.Context, cfg *Config, o RunOptions, spec api.JobSpec, ws *Workspace) (*Host, api.Job, error) {
	var j api.Job
	busy := map[string]bool{}
	waited := false
	for {
		hosts := Discover(ctx, cfg)
		var h *Host
		var err error
		if o.On != "" {
			h, err = FindHost(hosts, o.On)
		} else {
			var avail []*Host
			for _, x := range hosts {
				if !busy[x.URL] {
					avail = append(avail, x)
				}
			}
			if len(avail) == 0 && len(hosts) > 0 {
				avail, busy = hosts, map[string]bool{} // all busy: wait on the best one
			}
			h, err = PickFor(avail, spec, "job")
		}
		if err != nil {
			return nil, j, err
		}
		err = h.do(ctx, "POST", "/v1/jobs", spec, &j)
		var ae *APIError
		if errors.As(err, &ae) && ae.Code == 409 {
			if o.On == "" && !busy[h.URL] && len(hosts) > 1 {
				busy[h.URL] = true
				continue
			}
			if !waited {
				logf(o.Quiet, "%s: %s; waiting", h.Name, ae.Msg)
				waited = true
			}
			select {
			case <-ctx.Done():
				return nil, j, ctx.Err()
			case <-time.After(3 * time.Second):
			}
			continue
		}
		if err != nil {
			forgetHosts() // maybe stale; rediscover next time
			return nil, j, fmt.Errorf("%s: %w", h.Name, err)
		}
		rememberJob(j.ID, h)
		if j.GitNeeded && ws != nil {
			if err := uploadGit(ctx, h, j, ws, o.Quiet); err != nil {
				logf(false, "git history not sent (%v); the job runs without .git", err)
			}
		}
		if ws != nil {
			up := time.Now()
			pr, pw := io.Pipe()
			go func() { pw.CloseWithError(writeTar(pw, ws.Root, ws.Manifest, j.Needed)) }()
			needed := len(j.Needed)
			err := h.do(ctx, "PUT", "/v1/jobs/"+j.ID+"/source", pr, &j)
			pr.Close()
			if err != nil {
				cancelJob(h, j.ID)
				return nil, j, fmt.Errorf("uploading source: %w", err)
			}
			if needed > 0 && time.Since(up) > 2*time.Second {
				logf(o.Quiet, "synced %d changed files in %s", needed, time.Since(up).Round(100*time.Millisecond))
			}
		}
		prof := ""
		if j.Spec.Profile != "" {
			prof = "profile " + j.Spec.Profile + ", "
		}
		where := "host"
		if j.Runtime == "container" {
			where = "container " + strings.TrimPrefix(j.Image, "localhost/")
		}
		logf(o.Quiet, "running on %s as %s:%s (%s, %s%g cpu, %s memory %s)", h.Name, h.Name, j.ID, where, prof,
			j.Resources.CPU, units.FormatSize(j.Resources.Mem), j.MemFrom)
		return h, j, nil
	}
}

// uploadGit sends the commits the runner is missing. The first job of a repository on a runner
// sends the whole history once; later ones send only new commits.
func uploadGit(ctx context.Context, h *Host, j api.Job, ws *Workspace, quiet bool) error {
	path, err := gitBundle(ws.Root, j.GitHave)
	if err != nil {
		return err
	}
	defer os.Remove(path)
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	if fi, err := f.Stat(); err == nil && fi.Size() > 8<<20 {
		logf(quiet, "sending %s of git history to %s (once per repository)", units.FormatSize(fi.Size()), h.Name)
	}
	return h.do(ctx, "PUT", "/v1/jobs/"+j.ID+"/git", f, nil)
}

func clientLabel() string {
	host, _ := os.Hostname()
	return host
}

func cancelJob(h *Host, id string) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = h.do(ctx, "DELETE", "/v1/jobs/"+id, nil, nil)
}

// follow streams a job's stdout and stderr from the start, reconnecting from the last byte after
// network trouble or laptop sleep, and returns the exit code with the final job record. With
// cancelOnInterrupt, Ctrl-C cancels the job; otherwise it only detaches.
func follow(ctx context.Context, h *Host, id string, quiet, cancelOnInterrupt bool, cfg *Config) (int, *api.Job, error) {
	var j api.Job
	err := h.do(ctx, "GET", "/v1/jobs/"+id, nil, &j)
	if err == nil && (len(j.Spec.Relay) > 0 || j.Spec.Network == "client") && cfg != nil {
		cbctx, stopCallbacks := context.WithCancel(ctx)
		defer stopCallbacks()
		go serveCallbacks(cbctx, h, "/v1/jobs/"+id, cfg, quiet)
	}
	if err == nil && j.State == api.StateQueued {
		why := strings.TrimPrefix(j.Reason, "waiting: ")
		if why == "" {
			why = "waiting for capacity"
		}
		logf(quiet, "queued: %s", why)
	}
	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i, s := range []struct {
		name string
		w    io.Writer
	}{{"stdout", os.Stdout}, {"stderr", os.Stderr}} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs[i] = streamLog(ctx, h, id, s.name, s.w, quiet && s.name == "stderr")
		}()
	}
	wg.Wait()
	if ctx.Err() != nil {
		if cancelOnInterrupt {
			logf(quiet, "cancelling %s:%s", h.Name, id)
			cancelJob(h, id)
		}
		return 130, nil, nil
	}
	for _, err := range errs {
		if err != nil {
			return 1, nil, err
		}
	}
	if err := h.do(ctx, "GET", "/v1/jobs/"+id, nil, &j); err != nil {
		return 1, nil, err
	}
	switch {
	case j.State == api.StateCancelled:
		logf(quiet, "job was cancelled")
		return 130, &j, nil
	case j.State == api.StateFailed:
		logf(false, "%s:%s failed: %s", h.Name, id, j.Reason)
		if j.ExitCode != nil && *j.ExitCode != 0 {
			return *j.ExitCode, &j, nil
		}
		if strings.Contains(j.Reason, "out of memory") || strings.HasPrefix(j.Reason, "killed") {
			return 137, &j, nil
		}
		return 1, &j, nil
	case j.ExitCode != nil:
		return *j.ExitCode, &j, nil
	}
	return 1, &j, nil
}

// streamLog copies one log stream until the job is over, reattaching at the last byte.
func streamLog(ctx context.Context, h *Host, id, stream string, w io.Writer, quiet bool) error {
	var offset int64
	failures := 0
	for {
		resp, err := h.request(ctx, "GET", fmt.Sprintf("/v1/jobs/%s/logs?follow=1&stream=%s&offset=%d", id, stream, offset), nil)
		if err == nil {
			n, cerr := io.Copy(w, resp.Body)
			resp.Body.Close()
			offset += n
			if n > 0 {
				failures = 0
			}
			err = cerr
		}
		if ctx.Err() != nil {
			return nil
		}
		var ae *APIError
		if errors.As(err, &ae) && ae.Code == 404 {
			return fmt.Errorf("job %s is gone from %s", id, h.Name)
		}
		if err == nil {
			var j api.Job
			if gerr := h.do(ctx, "GET", "/v1/jobs/"+id, nil, &j); gerr == nil && api.Terminal(j.State) {
				return nil
			}
			time.Sleep(200 * time.Millisecond)
			continue
		}
		failures++
		if failures == 1 && stream == "stdout" {
			logf(quiet, "lost connection to %s (%v); reattaching", h.Name, err)
		}
		if failures > 360 { // ~30 minutes
			return fmt.Errorf("gave up reattaching to %s:%s; try: offload attach %s:%s", h.Name, id, h.Name, id)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(5 * time.Second):
		}
	}
}

// finish applies write-back and artifacts and prints a one-line summary.
func finish(ctx context.Context, h *Host, j *api.Job, ws *Workspace, o RunOptions) {
	if j.WriteBack != nil && ws != nil {
		if err := applyWriteBack(ctx, h, j, ws.Root, o.Quiet); err != nil {
			logf(false, "write-back: %v", err)
		}
	}
	if j.HasArtifact && len(o.Artifacts) > 0 {
		if err := fetchArtifacts(ctx, h, j.ID, "."); err != nil {
			logf(false, "artifacts: %v", err)
		}
	}
	if j.StartedAt != nil && j.FinishedAt != nil && j.State == api.StateExited {
		peak := ""
		if j.PeakMem > 0 {
			peak = fmt.Sprintf(", peak %s of %s", units.FormatSize(j.PeakMem), units.FormatSize(j.Resources.Mem))
		}
		logf(o.Quiet, "%s:%s exit %d in %s%s", h.Name, j.ID, *j.ExitCode, j.FinishedAt.Sub(*j.StartedAt).Round(100*time.Millisecond), peak)
	}
}

// applyWriteBack writes files the job edited into the local tree, unless the local copy also
// changed since upload (then it's left alone and reported).
func applyWriteBack(ctx context.Context, h *Host, j *api.Job, root string, quiet bool) error {
	wb := j.WriteBack
	if len(wb.Deleted) > 0 {
		logf(quiet, "job deleted %d synced file(s) on the runner (not deleted locally): %s", len(wb.Deleted), strings.Join(firstN(wb.Deleted, 5), ", "))
	}
	if len(wb.Modified) == 0 {
		return nil
	}
	byPath := map[string]api.ChangedFile{}
	for _, c := range wb.Modified {
		byPath[c.Path] = c
	}
	resp, err := h.request(ctx, "GET", "/v1/jobs/"+j.ID+"/writeback", nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	gz, err := gzip.NewReader(resp.Body)
	if err != nil {
		return err
	}
	tr := tar.NewReader(gz)
	var wrote, skipped []string
	for {
		hd, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
		c, ok := byPath[hd.Name]
		if !ok || hd.Typeflag != tar.TypeReg {
			return fmt.Errorf("unexpected write-back entry %q", hd.Name)
		}
		local := filepath.Join(root, filepath.FromSlash(c.Path))
		if !strings.HasPrefix(local, filepath.Clean(root)+string(filepath.Separator)) {
			return fmt.Errorf("write-back path escapes the tree: %q", c.Path)
		}
		if cur, err := hashLocal(local); err != nil || cur != c.BaseHash {
			skipped = append(skipped, c.Path)
			continue
		}
		tmp := local + ".offload-tmp"
		f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, os.FileMode(c.Mode).Perm())
		if err != nil {
			return err
		}
		hash := sha256.New()
		_, err = io.Copy(io.MultiWriter(f, hash), tr)
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		if err == nil && hex.EncodeToString(hash.Sum(nil)) != c.Hash {
			err = fmt.Errorf("%s: corrupted in transfer", c.Path)
		}
		if err == nil {
			err = os.Rename(tmp, local)
		}
		if err != nil {
			os.Remove(tmp)
			return err
		}
		wrote = append(wrote, c.Path)
	}
	if len(wrote) > 0 {
		logf(quiet, "wrote back %d file(s) the job changed: %s", len(wrote), strings.Join(firstN(wrote, 8), ", "))
	}
	if len(skipped) > 0 {
		logf(false, "not written back (changed locally during the job): %s", strings.Join(firstN(skipped, 8), ", "))
	}
	return nil
}

func hashLocal(p string) (string, error) {
	f, err := os.Open(p)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func firstN(s []string, n int) []string {
	if len(s) <= n {
		return s
	}
	return append(append([]string(nil), s[:n]...), fmt.Sprintf("… %d more", len(s)-n))
}

// fetchArtifacts downloads and unpacks a job's artifacts into dest.
func fetchArtifacts(ctx context.Context, h *Host, id, dest string) error {
	resp, err := h.request(ctx, "GET", "/v1/jobs/"+id+"/artifacts", nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	gz, err := gzip.NewReader(resp.Body)
	if err != nil {
		return err
	}
	tr := tar.NewReader(gz)
	n := 0
	for {
		hd, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
		name := filepath.Clean(filepath.FromSlash(hd.Name))
		if filepath.IsAbs(name) || name == ".." || strings.HasPrefix(name, ".."+string(filepath.Separator)) {
			return fmt.Errorf("refusing artifact path %q", hd.Name)
		}
		p := filepath.Join(dest, name)
		switch hd.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(p, 0o755); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
				return err
			}
			f, err := os.OpenFile(p, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, os.FileMode(hd.Mode).Perm())
			if err != nil {
				return err
			}
			_, err = io.Copy(f, tr)
			f.Close()
			if err != nil {
				return err
			}
			n++
		case tar.TypeSymlink:
			_ = os.Remove(p)
			if err := os.Symlink(hd.Linkname, p); err != nil {
				return err
			}
		}
	}
	logf(false, "downloaded %d artifact file(s) into %s", n, dest)
	return nil
}
