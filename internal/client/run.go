package client

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/RestartFU/offload/internal/api"
	"github.com/RestartFU/offload/internal/units"
)

// RunOptions is `offload run`.
type RunOptions struct {
	On        string
	Profile   string
	Resources api.Resources
	Env       map[string]string
	Artifacts []string
	NoSync    bool
	Fresh     bool
	Ephemeral bool
	Detach    bool
	Quiet     bool
	Argv      []string
}

func logf(quiet bool, format string, args ...any) {
	if !quiet {
		fmt.Fprintf(os.Stderr, "offload: "+format+"\n", args...)
	}
}

// Run sends a command to a runner and streams its output; the return value is the command's
// exit code. Ctrl-C (or SIGTERM) cancels the remote job.
func Run(o RunOptions) (int, error) {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	defer stop()
	cfg, err := LoadConfig()
	if err != nil {
		return 1, err
	}
	spec := api.JobSpec{Argv: o.Argv, Env: o.Env, Profile: o.Profile, Resources: o.Resources,
		Artifacts: o.Artifacts, Fresh: o.Fresh, Ephemeral: o.Ephemeral, Client: clientLabel()}
	var ws *Workspace
	if !o.NoSync {
		cwd, _ := os.Getwd()
		start := time.Now()
		if ws, err = LoadWorkspace(cwd); err != nil {
			return 1, fmt.Errorf("reading workspace: %w", err)
		}
		spec.WorkspaceKey, spec.Manifest, spec.Dir = ws.Key, ws.Manifest, ws.Dir
		if d := time.Since(start); d > 2*time.Second {
			logf(o.Quiet, "scanned %d files in %s", len(ws.Manifest), d.Round(100*time.Millisecond))
		}
	}

	var h *Host
	var j api.Job
	for attempt := 0; ; attempt++ {
		hosts := Discover(ctx, cfg)
		if o.On != "" {
			h, err = FindHost(hosts, o.On)
		} else {
			h, err = Pick(hosts, o.Resources, o.Profile, spec.WorkspaceKey, "job")
		}
		if err != nil {
			return 1, err
		}
		err = h.do(ctx, "POST", "/v1/jobs", spec, &j)
		var ae *APIError
		if errors.As(err, &ae) && ae.Code == 409 && attempt < 60 {
			if attempt == 0 {
				logf(o.Quiet, "%s: %s; waiting", h.Name, ae.Msg)
			}
			select {
			case <-ctx.Done():
				return 130, nil
			case <-time.After(5 * time.Second):
			}
			continue
		}
		if err != nil {
			return 1, fmt.Errorf("%s: %w", h.Name, err)
		}
		break
	}
	rememberJob(j.ID, h)
	ref := h.Name + ":" + j.ID

	if ws != nil {
		up := time.Now()
		pr, pw := io.Pipe()
		go func() { pw.CloseWithError(writeTar(pw, ws.Root, ws.Manifest, j.Needed)) }()
		err := h.do(ctx, "PUT", "/v1/jobs/"+j.ID+"/source", pr, &j)
		pr.Close()
		if err != nil {
			cancelJob(h, j.ID)
			if ctx.Err() != nil {
				return 130, nil
			}
			return 1, fmt.Errorf("uploading source: %w", err)
		}
		if len(j.Needed) > 0 || time.Since(up) > time.Second {
			logf(o.Quiet, "synced %d/%d files to %s in %s", len(j.Needed), len(ws.Manifest), h.Name, time.Since(up).Round(10*time.Millisecond))
		}
	}
	logf(o.Quiet, "job %s (%.0f cpu, burst %.0f, %s)", ref, j.Resources.CPU, j.Resources.CPUMax, units.FormatSize(j.Resources.Mem))
	if o.Detach {
		fmt.Println(ref)
		return 0, nil
	}
	return follow(ctx, h, j.ID, o.Quiet, true, o.Artifacts != nil)
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

// follow streams a job's output from the start, reconnecting from the last byte after network
// trouble or laptop sleep. With cancelOnInterrupt, Ctrl-C cancels the job; otherwise it detaches.
func follow(ctx context.Context, h *Host, id string, quiet, cancelOnInterrupt, artifacts bool) (int, error) {
	var offset int64
	var j api.Job
	failures := 0
	queuedNoted := false
	for {
		if !queuedNoted {
			if err := h.do(ctx, "GET", "/v1/jobs/"+id, nil, &j); err == nil && j.State == api.StateQueued {
				why := strings.TrimPrefix(j.Reason, "waiting: ")
				if why == "" {
					why = "waiting for capacity"
				}
				logf(quiet, "queued: %s", why)
				queuedNoted = true
			} else if err == nil {
				queuedNoted = true
			}
		}
		resp, err := h.request(ctx, "GET", fmt.Sprintf("/v1/jobs/%s/logs?follow=1&offset=%d", id, offset), nil)
		if err == nil {
			n, cerr := io.Copy(os.Stdout, resp.Body)
			resp.Body.Close()
			offset += n
			if n > 0 {
				failures = 0
			}
			err = cerr
		}
		if ctx.Err() != nil {
			if cancelOnInterrupt {
				logf(quiet, "cancelling %s:%s", h.Name, id)
				cancelJob(h, id)
				return 130, nil
			}
			return 130, nil
		}
		var ae *APIError
		if errors.As(err, &ae) && ae.Code == 404 {
			return 1, fmt.Errorf("job %s is gone from %s", id, h.Name)
		}
		if err == nil {
			// The stream ended cleanly, which happens once the job is finished.
			if gerr := h.do(ctx, "GET", "/v1/jobs/"+id, nil, &j); gerr == nil && api.Terminal(j.State) {
				break
			}
			time.Sleep(200 * time.Millisecond)
		} else {
			failures++
			if failures == 1 {
				logf(quiet, "lost connection to %s (%v); reattaching", h.Name, err)
			}
			if failures > 360 { // ~30 minutes
				return 1, fmt.Errorf("gave up reattaching to %s:%s; try: offload attach %s:%s", h.Name, id, h.Name, id)
			}
			select {
			case <-ctx.Done():
				continue
			case <-time.After(5 * time.Second):
			}
		}
	}
	if j.HasArtifact && artifacts {
		if err := fetchArtifacts(ctx, h, id, "."); err != nil {
			logf(false, "artifacts: %v", err)
		}
	}
	switch {
	case j.State == api.StateCancelled:
		logf(quiet, "job was cancelled")
		return 130, nil
	case j.State == api.StateFailed:
		logf(false, "job failed: %s", j.Reason)
		if j.ExitCode != nil && *j.ExitCode != 0 {
			return *j.ExitCode, nil
		}
		return 137, nil
	case j.ExitCode != nil:
		if j.Reason != "" && *j.ExitCode != 0 {
			logf(quiet, "%s", j.Reason)
		}
		return *j.ExitCode, nil
	}
	return 1, nil
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
