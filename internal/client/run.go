package client

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"os/signal"
	"path"
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
		return runInPlace(o.Argv, o.Env) // the command keeps normal SIGPIPE behavior
	}
	// A closed output (piped into `head`) must surface as a write error, which ends following
	// with the job's reference, instead of killing offload mid-write.
	signal.Ignore(syscall.SIGPIPE)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	defer stop()
	cfg, err := LoadConfig()
	if err != nil {
		return 1, err
	}
	for _, a := range o.Artifacts {
		if c := path.Clean(filepath.ToSlash(a)); c == "." || c == "/" || c == ".." || strings.HasPrefix(c, "../") || path.IsAbs(c) {
			return 1, fmt.Errorf("--get %s: name the outputs, relative to the working directory and inside it (for example --get dist)", a)
		}
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
		spec.GitTree, spec.IgnoreRules, spec.IgnoreGlobal = ws.GitTree, ws.IgnoreRules, ws.IgnoreGlobal
		spec.WriteBack = !o.NoWriteBack
		if !o.Host { // --host runs on the runner itself: no environment, even if the repo has one
			if spec.Environment, err = LoadEnvDef(ws.Root, o.Environment); err != nil {
				return 1, err
			}
		}
		if !o.NoGit {
			spec.Git = ws.Git
			if ws.Submods && ws.Git != nil {
				logf(o.Quiet, "note: submodules are synced as plain files; git commands inside them see the top-level repository")
			}
		}
		if d := time.Since(start); d > 2*time.Second {
			logf(o.Quiet, "scanned %d files in %s", len(ws.Manifest), d.Round(100*time.Millisecond))
		}
	}

	var snap map[string]fileSig
	if len(o.Artifacts) > 0 {
		snap = snapshotArtifacts(o.Artifacts)
	}
	for attempt := 0; ; attempt++ {
		spec.SubmitID = randomName() // one per submission (an OOM retry is a new job)
		// Every write-back job gets a baseline bound to its ID, so `offload attach` can still
		// apply its edits if this terminal goes away before it finishes.
		pendingBaseline := ""
		if ws != nil && spec.WriteBack {
			if pendingBaseline, err = saveBaseline(ws); err != nil {
				return 1, fmt.Errorf("can't save the write-back baseline (use --no-write-back to skip): %w", err)
			}
			defer os.Remove(pendingBaseline) // a no-op once renamed to the job's
		}
		h, j, err := submit(ctx, cfg, o, spec, ws, pendingBaseline)
		if err != nil {
			if ctx.Err() != nil {
				return 130, nil
			}
			return 1, err
		}
		ref := RefName(Discover(ctx, cfg), h) + ":" + j.ID
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
				_ = os.Remove(baselinePath(j.ID)) // that attempt's edits are not applied
				continue
			}
		}
		if err := finish(ctx, h, &j, ws, o, snap); err != nil {
			logf(false, "%v", err)
			if code == 0 {
				code = 1 // a build whose output didn't arrive must not look like a success
			}
		}
		return code, nil
	}
}

// submit picks a runner, creates the job and uploads the changed source. A runner whose
// workspace slots are all busy is skipped for the next best one; if every runner is busy it waits.
func submit(ctx context.Context, cfg *Config, o RunOptions, spec api.JobSpec, ws *Workspace, pendingBaseline string) (*Host, api.Job, error) {
	var j api.Job
	// Rounds: try each eligible runner once (a busy one is skipped for the next best); when a
	// round has found them all busy, wait a moment and start a fresh round.
	busy := map[string]bool{}
	refused := map[string]bool{} // answered 403 (pairing reset there): not a candidate this run
	waited := false
	backoff := func(msg string) error {
		if !waited {
			logf(o.Quiet, "%s; waiting", msg)
			waited = true
		}
		busy = map[string]bool{}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(3 * time.Second):
			return nil
		}
	}
	for {
		hosts := Discover(ctx, cfg)
		var h *Host
		var err error
		if o.On != "" {
			h, err = FindHost(hosts, o.On)
		} else {
			var avail []*Host
			for _, x := range hosts {
				if !busy[x.URL] && !refused[x.URL] {
					avail = append(avail, x)
				}
			}
			h, err = PickFor(avail, spec, "job")
			if err != nil && len(busy) > 0 { // the runners left can't take it: wait for a busy one
				if berr := backoff("every runner that fits is busy"); berr != nil {
					return nil, j, berr
				}
				continue
			}
		}
		if err != nil {
			return nil, j, err
		}
		if err := checkProto(h); err != nil { // also for --on
			return nil, j, err
		}
		err = h.do(ctx, "POST", "/v1/jobs", spec, &j)
		for retry := 0; err != nil && retry < 5 && ctx.Err() == nil; retry++ {
			var ae *APIError
			if errors.As(err, &ae) {
				break // the runner answered
			}
			// The answer was lost, maybe after the runner created the job: ask again with the
			// same SubmitID, which returns that job rather than a second one.
			select {
			case <-ctx.Done():
			case <-time.After(2 * time.Second):
			}
			err = h.do(ctx, "POST", "/v1/jobs", spec, &j)
		}
		var ae *APIError
		if errors.As(err, &ae) && ae.Code == 403 && o.On == "" {
			logf(false, "%s: %s; using another runner", h.Name, ae.Msg)
			refused[h.URL] = true
			continue
		}
		if errors.As(err, &ae) && ae.Code == 409 {
			if o.On == "" {
				busy[h.URL] = true // next best this round
				continue
			}
			if berr := backoff(h.Name + ": " + ae.Msg); berr != nil {
				return nil, j, berr
			}
			continue
		}
		if err != nil {
			forgetHosts() // maybe stale; rediscover next time
			return nil, j, fmt.Errorf("%s: %w", h.Name, err)
		}
		rememberJob(j.ID, h)
		if !o.Detach {
			// Before the upload that makes the job runnable: a job may ask for credentials or
			// dial out the moment it starts, and the runner only waits briefly for a client.
			startCallbacks(ctx, h, j, cfg, o.Quiet)
		}
		if pendingBaseline != "" {
			// Bound to the job before the source upload that makes it runnable: whatever happens
			// to this connection afterwards, `offload attach` can still write its edits back.
			if err := os.Rename(pendingBaseline, baselinePath(j.ID)); err != nil {
				cancelJob(h, j.ID)
				return nil, j, fmt.Errorf("can't keep the job's write-back baseline (cancelled it): %w", err)
			}
		}
		if j.GitNeeded && ws != nil {
			if err := uploadGit(ctx, h, j, ws, o.Quiet); err != nil {
				if ctx.Err() != nil { // interrupted: give the reserved job back now
					cancelJob(h, j.ID)
					return nil, j, ctx.Err()
				}
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
	tags := map[string]string{}
	for k, v := range ws.Git.Tags {
		tags[k] = v
	}
	path, err := gitBundle(ctx, ws.Root, ws.Git.Head, j.GitHave, tags)
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
	if _, started := callbacksStarted.Load(id); err == nil && !started {
		cbctx, stopCallbacks := context.WithCancel(ctx)
		defer stopCallbacks()
		startCallbacks(cbctx, h, j, cfg, quiet)
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
	streamCtx, stopStreams := context.WithCancel(ctx)
	for i, s := range []struct {
		name string
		w    io.Writer
	}{{"stdout", os.Stdout}, {"stderr", os.Stderr}} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs[i] = streamLog(streamCtx, h, id, s.name, s.w, quiet && s.name == "stderr")
			if errs[i] != nil {
				stopStreams() // one stream's terminal error (not paired any more, gone, …) ends both
			}
		}()
	}
	wg.Wait()
	stopStreams()
	if ctx.Err() != nil {
		if cancelOnInterrupt {
			logf(quiet, "cancelling %s:%s", h.Name, id)
			cancelJob(h, id)
		}
		return 130, nil, nil
	}
	for _, err := range errs {
		if err != nil && !errors.Is(err, context.Canceled) { // the cause, not the sibling's cancel
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
			rw := &recordingWriter{w: w}
			n, cerr := io.Copy(rw, resp.Body)
			if rw.err != nil { // our own output failed (disk full, closed pipe): not a network matter
				resp.Body.Close()
				return fmt.Errorf("writing the job's %s: %w (it keeps running: offload attach %s)", stream, rw.err, id)
			}
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
		if errors.As(err, &ae) && ae.Code == 412 { // the runner was upgraded to another protocol
			return fmt.Errorf("%s: %s", h.Name, ae.Msg)
		}
		if errors.As(err, &ae) && ae.Code == 403 { // pairing reset: retrying can't help
			return fmt.Errorf("%s: %s; pair again (offload host pair %s --code CODE), then offload attach %s", h.Name, ae.Msg, h.Name, id)
		}
		if err == nil {
			var j api.Job
			if gerr := h.do(ctx, "GET", "/v1/jobs/"+id, nil, &j); gerr == nil && api.Terminal(j.State) {
				// The stream may have ended for a reconnect just before the job did: read
				// what was written in between, without following.
				return drainRest(ctx, h, func(off int64) string { return fmt.Sprintf("/v1/jobs/%s/logs?stream=%s&offset=%d", id, stream, off) }, offset, w)
			}
			time.Sleep(200 * time.Millisecond)
			continue
		}
		failures++
		if failures == 1 && stream == "stdout" {
			logf(quiet, "lost connection to %s (%v); reattaching", h.Name, err)
		}
		if failures > 360 { // ~30 minutes
			return fmt.Errorf("gave up reattaching to %s:%s; try: offload attach %s", h.Name, id, id)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(5 * time.Second):
		}
	}
}

// drainRest reads a finished stream from offset to its end, retrying a broken transfer from
// where it stopped, so the end of a job's output (or a session's last reply) is never dropped.
// Errors writing locally are returned.
func drainRest(ctx context.Context, h *Host, url func(int64) string, offset int64, w io.Writer) error {
	for attempt := 0; ; attempt++ {
		resp, err := h.request(ctx, "GET", url(offset), nil)
		if err == nil {
			rw := &recordingWriter{w: w}
			n, cerr := io.Copy(rw, resp.Body)
			resp.Body.Close()
			offset += n
			if rw.err != nil {
				return rw.err
			}
			if cerr == nil {
				return nil
			}
			err = cerr
		}
		var ae *APIError
		if errors.As(err, &ae) || ctx.Err() != nil || attempt >= 30 {
			return fmt.Errorf("reading the rest of the output: %w", err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
}

// callbacksStarted marks jobs whose credential/network callbacks submit already serves.
var callbacksStarted sync.Map

// startCallbacks answers a job's relay and dial requests from this machine until ctx ends.
func startCallbacks(ctx context.Context, h *Host, j api.Job, cfg *Config, quiet bool) {
	if (len(j.Spec.Relay) == 0 && j.Spec.Network != "client") || cfg == nil {
		return
	}
	allow := map[string]bool{"dial": j.Spec.Network == "client"}
	for _, r := range j.Spec.Relay {
		switch r {
		case "git":
			allow["git-credential"] = true
		case "ssh-agent":
			allow["ssh-agent"] = true
		}
	}
	callbacksStarted.Store(j.ID, true)
	go serveCallbacks(ctx, h, "/v1/jobs/"+j.ID, cfg, quiet, allow)
}

// collectError is the runner failing to collect a job's edits: final, attach can't fix it.
type collectError struct{ msg string }

func (e *collectError) Error() string { return "the runner couldn't collect the job's edits: " + e.msg }

// baseline is what a job was sent, kept until its write-back is applied (by `run` when it sees
// the job finish, or later by `offload attach`).
type baseline struct {
	Root     string          `json:"root"`
	Dir      string          `json:"dir"` // the job's working directory, relative to Root
	Manifest []api.FileEntry `json:"manifest"`
}

func baselinePath(id string) string { return filepath.Join(stateDir(), "writeback", id+".json") }

// lockPath is a lock file. Lock files live apart from anything cleaned up by age, so a lock
// held right now is never unlinked under its holder.
func lockPath(name string) string { return filepath.Join(stateDir(), "locks", name) }

func checkoutLock(root string) string {
	sum := sha256.Sum256([]byte(root))
	return lockPath("checkout-" + hex.EncodeToString(sum[:8]))
}

// saveBaseline writes the baseline to a pending file before the job is submitted (renamed to
// the job's once it has an ID), so a job never starts without one.
func saveBaseline(ws *Workspace) (string, error) {
	dir := filepath.Dir(baselinePath("x"))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	if old, err := os.ReadDir(dir); err == nil { // two months: longer than runners keep jobs
		for _, e := range old {
			if fi, err := e.Info(); err == nil && fi.Mode().IsRegular() && time.Since(fi.ModTime()) > 60*24*time.Hour {
				_ = os.Remove(filepath.Join(dir, e.Name()))
			}
		}
	}
	b, err := json.Marshal(baseline{Root: ws.Root, Dir: ws.Dir, Manifest: ws.Manifest})
	if err != nil {
		return "", err
	}
	f, err := os.CreateTemp(dir, ".pending-*")
	if err != nil {
		return "", err
	}
	_, err = f.Write(b)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(f.Name())
		return "", err
	}
	return f.Name(), nil
}

// consumeWriteBack applies a finished job's write-back from its baseline, once. With
// checkoutLocked the caller already holds the checkout's lock.
func consumeWriteBack(ctx context.Context, h *Host, j *api.Job, checkoutLocked, quiet bool, written map[string]fileSig) error {
	if !api.Terminal(j.State) {
		return nil
	}
	readBaseline := func() (*baseline, error) {
		b, err := os.ReadFile(baselinePath(j.ID))
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil // not started from this machine, or already applied
		}
		if err != nil {
			return nil, fmt.Errorf("reading the write-back baseline: %w", err)
		}
		var bl baseline
		if err := json.Unmarshal(b, &bl); err != nil {
			return nil, err
		}
		return &bl, nil
	}
	// Lock order everywhere: the checkout, then the job. Without the checkout lock yet, peek at
	// the baseline for its root, lock, and read it again under both locks.
	if !checkoutLocked {
		bl, err := readBaseline()
		if err != nil {
			return err
		}
		if bl == nil {
			if j.WriteBackError != "" {
				return &collectError{j.WriteBackError}
			}
			if _, err := os.Stat(baselinePath(j.ID) + ".done"); err == nil {
				return nil // applied already
			}
			if j.WriteBack != nil && len(j.WriteBack.Modified) > 0 {
				return fmt.Errorf("the job changed %d file(s), but this machine has no record of what it sent (started elsewhere, or already applied): not writing them back", len(j.WriteBack.Modified))
			}
			return nil
		}
		unlockCheckout, err := lockFile(checkoutLock(bl.Root))
		if err != nil {
			return err
		}
		defer unlockCheckout()
	}
	unlock, err := lockFile(lockPath("job-" + j.ID)) // two attaches of one job apply it once
	if err != nil {
		return err
	}
	defer unlock()
	bl, err := readBaseline()
	if err != nil {
		return err
	}
	if bl == nil { // applied (or forgotten) already; a collection failure is still a failure
		if j.WriteBackError != "" {
			return &collectError{j.WriteBackError}
		}
		return nil
	}
	if j.WriteBack == nil || j.State == api.StateCancelled { // nothing to write back, or cancelled: forget it
		if err := os.Remove(baselinePath(j.ID)); err != nil {
			return err
		}
		if j.WriteBackError != "" {
			return &collectError{j.WriteBackError}
		}
		return nil
	}
	if err := applyWriteBackLocked(ctx, h, j, bl.Root, bl.Manifest, quiet, written); err != nil {
		return err
	}
	// Remember it was applied, so attaching to this job again (to read its log) isn't taken for
	// edits that never arrived.
	_ = os.WriteFile(baselinePath(j.ID)+".done", nil, 0o600)
	return os.Remove(baselinePath(j.ID))
}

// finish applies write-back and artifacts and prints a one-line summary. Both run under the
// checkout's lock. It returns an error when requested artifacts didn't all arrive.
func finish(ctx context.Context, h *Host, j *api.Job, ws *Workspace, o RunOptions, snap map[string]fileSig) error {
	if ws != nil {
		unlock, err := lockFile(checkoutLock(ws.Root))
		if err != nil {
			return fmt.Errorf("write-back: %w", err)
		}
		defer unlock()
	}
	wrote := map[string]fileSig{}
	var wbErr error
	if err := consumeWriteBack(ctx, h, j, true, o.Quiet, wrote); err != nil {
		var ce *collectError
		if errors.As(err, &ce) { // nothing to retry: the edits never left the runner
			wbErr = fmt.Errorf("write-back: %w", err)
		} else {
			wbErr = fmt.Errorf("write-back: %w (run `offload attach %s` to retry)", err, j.ID)
		}
	}
	// What write-back installed is the job's own result, not a local change: artifacts may
	// replace exactly those files (identified as they were installed, before any later edit).
	if snap != nil && ws != nil {
		for w, sig := range wrote {
			rel := w
			if d := filepath.ToSlash(ws.Dir); d != "" && d != "." {
				var ok bool
				if rel, ok = strings.CutPrefix(w, d+"/"); !ok {
					continue
				}
			}
			snap[rel] = sig
		}
	}
	var artErr error
	if len(o.Artifacts) > 0 && j.State == api.StateExited {
		if !j.HasArtifact {
			artErr = fmt.Errorf("the job produced none of the requested artifacts (%s)", strings.Join(o.Artifacts, ", "))
		} else if kept, err := fetchArtifacts(ctx, h, j.ID, ".", snap); err != nil {
			artErr = fmt.Errorf("artifacts: %w", err)
		} else if kept > 0 {
			artErr = fmt.Errorf("%d artifact(s) not written over local changes (saved beside them)", kept)
		}
		if len(j.ArtifactsMissing) > 0 && artErr == nil {
			artErr = fmt.Errorf("the job didn't produce %s (the other artifacts were downloaded)", strings.Join(j.ArtifactsMissing, ", "))
		}
	}
	if j.StartedAt != nil && j.FinishedAt != nil && j.State == api.StateExited {
		peak := ""
		if j.PeakMem > 0 {
			peak = fmt.Sprintf(", peak %s of %s", units.FormatSize(j.PeakMem), units.FormatSize(j.Resources.Mem))
		}
		logf(o.Quiet, "%s:%s exit %d in %s%s", h.Name, j.ID, *j.ExitCode, j.FinishedAt.Sub(*j.StartedAt).Round(100*time.Millisecond), peak)
	}
	if wbErr != nil {
		return wbErr
	}
	return artErr
}

// checkTarGz reads a downloaded tar.gz to its end (a truncated one fails), then rewinds it.
func checkTarGz(f *os.File) error {
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return err
	}
	gz, err := gzip.NewReader(f)
	if err != nil {
		return err
	}
	tr := tar.NewReader(gz)
	for {
		if _, err := tr.Next(); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			return err
		}
		if _, err := io.Copy(io.Discard, tr); err != nil {
			return err
		}
	}
	if _, err := io.Copy(io.Discard, gz); err != nil { // and the gzip trailer (checksum)
		return err
	}
	_, err = f.Seek(0, io.SeekStart)
	return err
}

// linkIs reports whether rel is a symlink pointing at target.
func linkIs(tree *os.Root, rel, target string) bool {
	fi, err := tree.Lstat(rel)
	if err != nil || fi.Mode()&fs.ModeSymlink == 0 {
		return false
	}
	cur, err := tree.Readlink(rel)
	return err == nil && cur == target
}

func randomName() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// verifyArchive checks a downloaded write-back archive is complete and intact (every entry's
// hash) before anything from it is applied.
func verifyArchive(f *os.File, want map[string]api.ChangedFile) error {
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return err
	}
	defer f.Seek(0, io.SeekStart)
	gz, err := gzip.NewReader(f)
	if err != nil {
		return err
	}
	tr := tar.NewReader(gz)
	seen := 0
	for {
		hd, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
		c, ok := want[hd.Name]
		if !ok {
			return fmt.Errorf("unexpected entry %q", hd.Name)
		}
		h := sha256.New()
		if _, err := io.Copy(h, tr); err != nil {
			return err
		}
		if hex.EncodeToString(h.Sum(nil)) != c.Hash {
			return fmt.Errorf("%s: corrupted in transfer", hd.Name)
		}
		seen++
	}
	if seen != len(want) {
		return fmt.Errorf("incomplete: %d of %d files", seen, len(want))
	}
	return nil
}

// fileSig is what a path looked like (metadata only, cheap for large output trees).
type fileSig struct {
	Size, MTime, CTime int64
	Ino                uint64
	Mode               fs.FileMode
	Link               string
}

func sigOf(root *os.Root, rel string, fi fs.FileInfo) fileSig {
	sig := fileSig{Size: fi.Size(), MTime: fi.ModTime().UnixNano(), Ino: inode(fi), Mode: fi.Mode()}
	sig.CTime, _ = changeTime(fi)
	if fi.Mode()&fs.ModeSymlink != 0 {
		sig.Link, _ = root.Readlink(rel)
	}
	return sig
}

// snapshotArtifacts records what the --get destinations look like before the job runs, so the
// download never replaces anything that appeared or changed here meanwhile.
func snapshotArtifacts(paths []string) map[string]fileSig {
	snap := map[string]fileSig{}
	tree, err := os.OpenRoot(".")
	if err != nil {
		return snap
	}
	defer tree.Close()
	for _, p := range paths {
		p = path.Clean(filepath.ToSlash(p))
		if checkRel(p) != nil {
			continue
		}
		// Its existing parents too: one deleted during the job must stay deleted (deletedHere).
		for d := path.Dir(p); d != "." && d != "/"; d = path.Dir(d) {
			if fi, err := tree.Lstat(d); err == nil {
				snap[d] = sigOf(tree, d, fi)
			}
		}
		if fi, err := tree.Lstat(p); err == nil && fi.Mode()&fs.ModeSymlink != 0 {
			snap[p] = sigOf(tree, p, fi) // the link itself, never what it points at
			continue
		}
		_ = fs.WalkDir(tree.FS(), p, func(rel string, d fs.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			if fi, err := d.Info(); err == nil {
				snap[rel] = sigOf(tree, rel, fi)
			}
			return nil
		})
	}
	return snap
}

// applyWriteBack applies a finished job's edits to root. One write-back per checkout at a time:
// two jobs that both started from the same file must not both find it unchanged and the
// second silently replace the first one's result.
func applyWriteBack(ctx context.Context, h *Host, j *api.Job, root string, sent []api.FileEntry, quiet bool) error {
	unlock, err := lockFile(checkoutLock(root))
	if err != nil {
		return err
	}
	defer unlock()
	return applyWriteBackLocked(ctx, h, j, root, sent, quiet, nil)
}

// written, when set, receives the root-relative paths written back.
func applyWriteBackLocked(ctx context.Context, h *Host, j *api.Job, root string, sent []api.FileEntry, quiet bool, written map[string]fileSig) error {
	base := map[string]api.FileEntry{}
	for _, e := range sent {
		base[e.Path] = e
	}
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
	// Download it all first: a connection lost halfway must leave nothing half-applied.
	_ = os.MkdirAll(stateDir(), 0o700)
	archive, err := os.CreateTemp(stateDir(), ".writeback-*")
	if err != nil {
		return err
	}
	defer os.Remove(archive.Name())
	defer archive.Close()
	resp, err := h.request(ctx, "GET", "/v1/jobs/"+j.ID+"/writeback", nil)
	if err != nil {
		return err
	}
	_, err = io.Copy(archive, resp.Body)
	resp.Body.Close()
	if err != nil {
		return fmt.Errorf("downloading the job's edits: %w", err)
	}
	content := map[string]api.ChangedFile{} // symlinks carry no content
	for p, c := range byPath {
		if c.Link == "" {
			content[p] = c
		}
	}
	if err := verifyArchive(archive, content); err != nil {
		return fmt.Errorf("downloading the job's edits: %w", err)
	}
	gz, err := gzip.NewReader(archive)
	if err != nil {
		return err
	}
	tree, err := os.OpenRoot(root) // confined: symlinks in the tree can't redirect writes outside it
	if err != nil {
		return err
	}
	defer tree.Close()
	// A staging directory of this write-back's own (another job finishing in the same checkout
	// has its own), removed when done; .offload-incoming itself goes once it is empty.
	sfx := make([]byte, 4)
	_, _ = rand.Read(sfx)
	stage := path.Join(incomingDir, ".stage-"+j.ID+"-"+hex.EncodeToString(sfx))
	if err := mkdirReal(tree, stage); err != nil {
		return err
	}
	defer func() {
		_ = tree.RemoveAll(stage)
		_ = tree.Remove(incomingDir)
	}()
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
		if err := checkRel(c.Path); err != nil {
			return err
		}
		// Stage in a directory of our own first, so whatever happens to the destination's
		// directory meanwhile, the verified incoming copy is never lost.
		tmp, err := stageIncoming(tree, stage, tr, os.FileMode(c.Mode).Perm(), c.Hash)
		if err != nil {
			return fmt.Errorf("%s: %w", c.Path, err)
		}
		// Replace only if the local file is still exactly what this job was sent (content and
		// mode), re-checked right before the rename. Otherwise the job's version is kept next to
		// it as <file>.offload-incoming[.N], or under .offload-incoming/<job>/ when its directory
		// moved or isn't a real directory any more.
		orig, known := base[c.Path]
		if realParents(tree, c.Path) {
			cur, before, herr := hashFileIn(tree, c.Path)
			var unchanged bool
			if c.BaseLink != "" { // it was a symlink: still the one the job was sent?
				unchanged = known && orig.Link == c.BaseLink && linkIs(tree, c.Path, c.BaseLink)
			} else {
				unchanged = known && orig.Link == "" && orig.Hash == c.BaseHash && herr == nil && cur == orig.Hash &&
					before.Mode().Perm() == os.FileMode(orig.Mode).Perm()
				if unchanged {
					h, now, err := hashFileIn(tree, c.Path)
					unchanged = err == nil && os.SameFile(before, now) && now.Mode() == before.Mode() && h == orig.Hash
				}
			}
			if unchanged && tree.Rename(tmp, c.Path) == nil {
				wrote = append(wrote, c.Path)
				if fi, err := tree.Lstat(c.Path); err == nil && written != nil {
					written[c.Path] = sigOf(tree, c.Path, fi) // this exact file is the job's
				}
				continue
			}
			// Already there (an earlier attempt applied it before failing): nothing to do.
			if herr == nil && cur == c.Hash && before.Mode().Perm() == os.FileMode(c.Mode).Perm() {
				_ = tree.Remove(tmp)
				wrote = append(wrote, c.Path)
				continue
			}
			if saved, ok := linkBeside(tree, tmp, c.Path); ok {
				_ = tree.Remove(tmp)
				skipped = append(skipped, c.Path+" (job's version: "+saved+")")
				continue
			}
		}
		saved := path.Join(incomingDir, j.ID, c.Path)
		if err := mkdirReal(tree, path.Dir(saved)); err != nil {
			return fmt.Errorf("%s: saving the job's version: %w (it is in %s)", c.Path, err, tmp)
		}
		if err := tree.Rename(tmp, saved); err != nil {
			return fmt.Errorf("%s: saving the job's version: %w (it is in %s)", c.Path, err, tmp)
		}
		skipped = append(skipped, c.Path+" (job's version: "+saved+")")
	}
	for _, c := range wb.Modified { // symlinks the job retargeted (or files it made links)
		if c.Link == "" {
			continue
		}
		if err := checkRel(c.Path); err != nil {
			return err
		}
		orig, known := base[c.Path]
		if linkIs(tree, c.Path, c.Link) {
			wrote = append(wrote, c.Path) // already applied
			continue
		}
		var unchanged bool
		if realParents(tree, c.Path) {
			if c.BaseLink != "" {
				unchanged = known && orig.Link == c.BaseLink && linkIs(tree, c.Path, c.BaseLink)
			} else if cur, fi, err := hashFileIn(tree, c.Path); err == nil {
				unchanged = known && orig.Link == "" && orig.Hash == c.BaseHash && cur == orig.Hash && fi.Mode().Perm() == os.FileMode(orig.Mode).Perm()
			}
		}
		if unchanged {
			tmp := path.Join(path.Dir(c.Path), ".offload-"+randomName())
			if tree.Symlink(c.Link, tmp) == nil {
				if tree.Rename(tmp, c.Path) == nil {
					wrote = append(wrote, c.Path)
					if fi, err := tree.Lstat(c.Path); err == nil && written != nil {
						written[c.Path] = sigOf(tree, c.Path, fi)
					}
					continue
				}
				_ = tree.Remove(tmp)
			}
		}
		saved := ""
		for i := 0; i < 100 && saved == "" && realParents(tree, c.Path); i++ {
			cand := c.Path + ".offload-incoming"
			if i > 0 {
				cand = fmt.Sprintf("%s.%d", cand, i)
			}
			if tree.Symlink(c.Link, cand) == nil {
				saved = cand
			}
		}
		if saved == "" { // its directory moved: keep it with this job's other kept versions
			cand := path.Join(incomingDir, j.ID, c.Path)
			if err := mkdirReal(tree, path.Dir(cand)); err != nil {
				return fmt.Errorf("%s: saving the job's link (to %s): %w", c.Path, c.Link, err)
			}
			if err := tree.Symlink(c.Link, cand); err != nil {
				return fmt.Errorf("%s: saving the job's link (to %s): %w", c.Path, c.Link, err)
			}
			saved = cand
		}
		skipped = append(skipped, c.Path+" (job's version: "+saved+")")
	}
	if len(wrote) > 0 {
		logf(quiet, "wrote back %d file(s) the job changed: %s", len(wrote), strings.Join(firstN(wrote, 8), ", "))

	}
	if len(skipped) > 0 {
		logf(false, "not written back (changed locally during the job): %s", strings.Join(firstN(skipped, 8), ", "))
	}
	return nil
}

// checkRel accepts only clean, relative, slash-separated paths that stay inside their tree.
func checkRel(p string) error {
	if p == "" || p == "." || path.IsAbs(p) || strings.HasPrefix(p, "../") || p == ".." || path.Clean(p) != p || strings.Contains(p, "\x00") {
		return fmt.Errorf("unsafe path %q", p)
	}
	return nil
}

// hashIn returns the sha256 of a regular file inside root (never following a final symlink).
func hashIn(root *os.Root, rel string) (string, error) {
	h, _, err := hashFileIn(root, rel)
	return h, err
}

// hashFileIn hashes a regular file through one descriptor and returns that descriptor's identity.
// The path must name that same file both before and after the read, so a swap mid-check fails.
func hashFileIn(root *os.Root, rel string) (string, os.FileInfo, error) {
	f, err := root.OpenFile(rel, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return "", nil, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return "", nil, err
	}
	if !fi.Mode().IsRegular() {
		return "", nil, fmt.Errorf("%s is not a regular file", rel)
	}
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", nil, err
	}
	if now, err := root.Lstat(rel); err != nil || !os.SameFile(fi, now) {
		return "", nil, fmt.Errorf("%s changed while being read", rel)
	}
	return hex.EncodeToString(h.Sum(nil)), fi, nil
}

// realParents reports whether every parent directory of rel inside root is a real directory.
func realParents(root *os.Root, rel string) bool {
	parts := strings.Split(rel, "/")
	for i := 1; i < len(parts); i++ {
		fi, err := root.Lstat(strings.Join(parts[:i], "/"))
		if err != nil || !fi.IsDir() {
			return false
		}
	}
	return true
}

var errChangedLocally = errors.New("changed locally")

// conflictError means the local file changed during write-back; the job's version is at saved.
type conflictError struct{ saved string }

func (e *conflictError) Error() string { return "changed locally; job's version saved as " + e.saved }
func (e *conflictError) Unwrap() error { return errChangedLocally }

// mkdirReal creates dir (and its parents) inside root, refusing any existing component that is
// not a real directory (a symlink there would alias another path).
func mkdirReal(root *os.Root, dir string) error {
	if dir == "." || dir == "" {
		return nil
	}
	parts := strings.Split(dir, "/")
	for i := range parts {
		p := strings.Join(parts[:i+1], "/")
		fi, err := root.Lstat(p)
		switch {
		case errors.Is(err, os.ErrNotExist):
			if err := root.Mkdir(p, 0o755); err != nil {
				return err
			}
		case err != nil:
			return err
		case !fi.IsDir():
			return fmt.Errorf("%s is not a real directory", p)
		}
	}
	return nil
}

// writeIn replaces rel inside root with r's content via an exclusively created temporary file
// and an atomic rename; wantHash, when set, must match.
func writeIn(root *os.Root, rel string, r io.Reader, mode os.FileMode, wantHash string) error {
	return writeInIf(root, rel, r, mode, wantHash, nil)
}

const incomingDir = ".offload-incoming"

// stageIncoming writes r into a new file in the stage directory, verifying its hash and setting
// its mode, and returns its path.
func stageIncoming(root *os.Root, stage string, r io.Reader, mode os.FileMode, wantHash string) (string, error) {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	tmp := path.Join(stage, hex.EncodeToString(b))
	f, err := root.OpenFile(tmp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return "", err
	}
	h := sha256.New()
	_, err = io.Copy(io.MultiWriter(f, h), r)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil && wantHash != "" && hex.EncodeToString(h.Sum(nil)) != wantHash {
		err = errors.New("corrupted in transfer")
	}
	if err == nil {
		err = root.Chmod(tmp, mode)
	}
	if err != nil {
		_ = root.Remove(tmp)
		return "", err
	}
	return tmp, nil
}

// linkBeside hard-links tmp next to rel as rel.offload-incoming[.N], under a name no existing
// file has (links never replace anything).
func linkBeside(root *os.Root, tmp, rel string) (string, bool) {
	for i := 0; i < 100; i++ {
		name := rel + ".offload-incoming"
		if i > 0 {
			name = fmt.Sprintf("%s.%d", name, i)
		}
		if root.Link(tmp, name) == nil {
			return name, true
		}
	}
	return "", false
}

// writeInIf is writeIn, but replaces rel only if still() reports true just before the rename.
func writeInIf(root *os.Root, rel string, r io.Reader, mode os.FileMode, wantHash string, still func() bool) error {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	tmp := path.Join(path.Dir(rel), ".offload-"+hex.EncodeToString(b))
	f, err := root.OpenFile(tmp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	h := sha256.New()
	_, err = io.Copy(io.MultiWriter(f, h), r)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil && wantHash != "" && hex.EncodeToString(h.Sum(nil)) != wantHash {
		err = fmt.Errorf("%s: corrupted in transfer", rel)
	}
	if err == nil {
		err = root.Chmod(tmp, mode)
	}
	if err == nil && still != nil && !still() {
		// Keep the incoming content beside the local file instead of discarding it, under a name
		// no existing file has (hard links never replace anything).
		err = fmt.Errorf("%w; could not save the job's version beside it", errChangedLocally)
		for i := 0; i < 100; i++ {
			name := rel + ".offload-incoming"
			if i > 0 {
				name = fmt.Sprintf("%s.%d", name, i)
			}
			if root.Link(tmp, name) == nil {
				err = &conflictError{saved: name}
				break
			}
		}
	}
	if err == nil {
		err = root.Rename(tmp, rel)
	}
	if err != nil {
		_ = root.Remove(tmp)
	}
	return err
}

func firstN(s []string, n int) []string {
	if len(s) <= n {
		return s
	}
	return append(append([]string(nil), s[:n]...), fmt.Sprintf("… %d more", len(s)-n))
}

// fetchArtifacts downloads and unpacks a job's artifacts into dest.
// deletedHere reports whether a directory above rel (or rel itself, for a directory entry) was
// there when the snapshot was taken and is gone now.
func deletedHere(tree *os.Root, rel string, isDir bool, snap map[string]fileSig) bool {
	d := path.Dir(rel)
	if isDir {
		d = rel
	}
	for ; d != "." && d != "/" && d != ""; d = path.Dir(d) {
		if _, had := snap[d]; had {
			if _, err := tree.Lstat(d); errors.Is(err, os.ErrNotExist) {
				return true
			}
		}
	}
	return false
}

// unchangedSinceSnap reports whether rel is as the snapshot saw it (or absent both times, or
// deleted here since: nothing to lose).
func unchangedSinceSnap(tree *os.Root, rel string, snap map[string]fileSig) bool {
	before, had := snap[rel]
	fi, err := tree.Lstat(rel)
	if errors.Is(err, os.ErrNotExist) {
		return !had // absent then and now; deleted here meanwhile is a local change to keep
	}
	if err != nil {
		return false
	}
	return had && sigOf(tree, rel, fi) == before
}

// fetchArtifacts extracts a job's artifacts into dest. With a snapshot (see snapshotArtifacts),
// a path that appeared or changed here since it was taken is kept, and the artifact is saved
// beside it as <file>.offload-incoming[.N]; kept counts those.
func fetchArtifacts(ctx context.Context, h *Host, id, dest string, snap map[string]fileSig) (kept int, err error) {
	// The whole archive first, checked complete: a connection lost halfway must not leave new
	// and old outputs mixed.
	_ = os.MkdirAll(stateDir(), 0o700)
	archive, err := os.CreateTemp(stateDir(), ".artifacts-*")
	if err != nil {
		return kept, err
	}
	defer os.Remove(archive.Name())
	defer archive.Close()
	resp, err := h.request(ctx, "GET", "/v1/jobs/"+id+"/artifacts", nil)
	if err != nil {
		return kept, err
	}
	_, err = io.Copy(archive, resp.Body)
	resp.Body.Close()
	if err == nil {
		err = checkTarGz(archive)
	}
	if err != nil {
		return kept, fmt.Errorf("downloading artifacts: %w", err)
	}
	gz, err := gzip.NewReader(archive)
	if err != nil {
		return kept, err
	}
	if err := os.MkdirAll(dest, 0o755); err != nil {
		return kept, err
	}
	tree, err := os.OpenRoot(dest) // confined: entries (and symlinks they create) can't escape dest
	if err != nil {
		return kept, err
	}
	defer tree.Close()
	tr := tar.NewReader(gz)
	n := 0
	for {
		hd, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return kept, err
		}
		name := path.Clean(hd.Name)
		if err := checkRel(name); err != nil {
			return kept, fmt.Errorf("refusing artifact path %q", hd.Name)
		}
		// A directory deleted here while the job ran stays deleted: what the job put under it
		// goes to .offload-incoming/<job>/ instead of recreating it.
		relocated := false
		if snap != nil && deletedHere(tree, name, hd.Typeflag == tar.TypeDir, snap) {
			name, relocated = path.Join(incomingDir, id, name), true
		}
		// Every entry type goes through real directories only: an existing symlink in the
		// destination (even one pointing inside it) must not alias another path.
		if err := mkdirReal(tree, path.Dir(name)); err != nil {
			return kept, fmt.Errorf("refusing artifact %q: %w", hd.Name, err)
		}
		if relocated && hd.Typeflag != tar.TypeDir {
			if kept == 0 {
				logf(false, "artifacts under a directory deleted here were saved in %s", path.Join(incomingDir, id))
			}
			kept++
		}
		// Nothing that appeared or changed here since the snapshot is replaced, whatever kind
		// of entry would replace it.
		still := func() bool { return true }
		if snap != nil {
			still = func() bool { return unchangedSinceSnap(tree, name, snap) }
		}
		switch hd.Typeflag {
		case tar.TypeDir:
			if err := mkdirReal(tree, name); err != nil {
				return kept, fmt.Errorf("refusing artifact %q: %w", hd.Name, err)
			}
		case tar.TypeReg:
			if err := writeInIf(tree, name, tr, os.FileMode(hd.Mode).Perm(), "", still); err != nil {
				var ce *conflictError
				if errors.As(err, &ce) {
					logf(false, "artifact %s not written over local changes (saved as %s)", name, ce.saved)
					kept++
					continue
				}
				return kept, err
			}
			n++
		case tar.TypeSymlink:
			if fi, err := tree.Lstat(name); err == nil && fi.IsDir() {
				return kept, fmt.Errorf("refusing artifact %q: would replace a directory", hd.Name)
			}
			if !still() {
				saved := ""
				for i := 0; i < 100 && saved == ""; i++ {
					cand := name + ".offload-incoming"
					if i > 0 {
						cand = fmt.Sprintf("%s.%d", cand, i)
					}
					if tree.Symlink(hd.Linkname, cand) == nil {
						saved = cand
					}
				}
				logf(false, "artifact %s not written over local changes (saved as %s)", name, saved)
				kept++
				continue
			}
			_ = tree.Remove(name)
			if err := tree.Symlink(hd.Linkname, name); err != nil {
				return kept, err
			}
		}
	}
	logf(false, "downloaded %d artifact file(s) into %s", n, dest)
	return kept, nil
}
