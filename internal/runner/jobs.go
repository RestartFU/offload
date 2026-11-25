package runner

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	goruntime "runtime"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	"github.com/RestartFU/offload/internal/api"
	"github.com/RestartFU/offload/internal/units"
)

type job struct {
	api.Job
	dir     string
	slot    *slot
	cpus    []int
	cancel  bool
	syncing bool // an upload is being applied to the slot

	relay       *relay            // credential relay, when Spec.Relay is set
	secrets     map[string]string // in memory until the job starts, then only in tmpfs
	secretFiles map[string][]byte
	queuedAt    time.Time
	inactive    int         // consecutive checks the unit was gone without an exit record
	checked     time.Time   // last time systemd was asked whether the unit is active
	cancelled   atomic.Bool // set by DELETE; read by uploads without taking s.mu
	abortUpload func()      // interrupts the upload being read right now (set while syncing)
	mounts      []string    // runtime paths (sockets, secrets) this job's container may see

}

// exitRecord is written by the unit wrapper when the command ends.
type exitRecord struct {
	Code      int            `json:"code"`
	Signal    string         `json:"signal,omitempty"`
	OOM       bool           `json:"oom,omitempty"`
	Error     string         `json:"error,omitempty"`
	PeakMem   int64          `json:"peak_mem,omitempty"`
	Artifact  bool           `json:"artifacts,omitempty"`
	WriteBack *api.WriteBack `json:"write_back,omitempty"`
}

// contextRel normalizes an environment's build context directory ("" means the workspace root).
func contextRel(dir string) string {
	if dir == "" || dir == "." {
		return "."
	}
	return dir
}

// contextProblem explains why ctx can't be a build context (empty if it can): it must be a real
// directory in the synced tree (not a symlink, nor under one) with at least one synced file.
func contextProblem(m []api.FileEntry, ctx string) string {
	if ctx == "." {
		return ""
	}
	files := 0
	for _, e := range m {
		if e.Link != "" && (e.Path == ctx || strings.HasPrefix(ctx, e.Path+"/")) {
			return fmt.Sprintf("%q is a symlink in the repository; point context at the real directory", e.Path)
		}
		if strings.HasPrefix(e.Path, ctx+"/") {
			files++
		}
	}
	if files == 0 {
		return fmt.Sprintf("%q has no synced files", ctx)
	}
	return ""
}

// contextDigest identifies exactly the files a COPY/ADD build can see: those under ctx.
func contextDigest(m []api.FileEntry, ctx string) string {
	h := sha256.New()
	fmt.Fprintf(h, "%s\n", ctx)
	for _, e := range m {
		if ctx != "." && !strings.HasPrefix(e.Path, ctx+"/") {
			continue
		}
		fmt.Fprintf(h, "%s\x00%s\x00%o\x00%s\n", e.Path, e.Hash, e.Mode, e.Link)
	}
	return hex.EncodeToString(h.Sum(nil))[:16]
}

// view is the job as returned to clients: without the (possibly huge) source manifest.
func (j *job) view() api.Job {
	v := j.Job
	v.Spec.Manifest = nil
	return v
}

func (j *job) save() error {
	err := writeFileJSON(filepath.Join(j.dir, "job.json"), j.Job)
	if err != nil {
		log.Printf("job %s: save: %v", j.ID, err)
	}
	return err
}

func (s *Server) jobDir(id string) string { return filepath.Join(s.cfg.StateDir, "jobs", id) }

// restore reloads jobs, sessions and slots after a runner restart. Units keep running while the
// runner is down, so running jobs are simply watched again.
func (s *Server) restore() {
	slotFiles, _ := filepath.Glob(filepath.Join(s.cfg.StateDir, "work", "*", "*.json"))
	for _, f := range slotFiles {
		key := filepath.Base(filepath.Dir(f))
		var idx int
		if _, err := fmt.Sscanf(filepath.Base(f), "%d.json", &idx); err != nil || !validKey(key) {
			continue
		}
		sl := &slot{Key: key, Index: idx, Dir: strings.TrimSuffix(f, ".json")}
		sl.LastUsed = sl.load().LastUsed
		s.slots[key] = append(s.slots[key], sl)
	}
	dirs, _ := filepath.Glob(filepath.Join(s.cfg.StateDir, "jobs", "*", "job.json"))
	for _, f := range dirs {
		b, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		j := &job{dir: filepath.Dir(f)}
		if json.Unmarshal(b, &j.Job) != nil {
			continue
		}
		if j.Slot != "" {
			j.slot = s.findSlot(j.Slot)
		}
		switch j.State {
		case api.StateUploading:
			j.State, j.Reason = api.StateFailed, "runner restarted during upload"
			if j.StopRequested {
				j.State, j.Reason = api.StateCancelled, "cancelled"
			}
			now := time.Now()
			j.FinishedAt = &now
			j.save()
		case api.StateQueued:
			j.queuedAt = j.CreatedAt
			if j.StopRequested {
				j.State, j.Reason = api.StateCancelled, "cancelled"
				now := time.Now()
				j.FinishedAt = &now
				j.save()
				break
			}
			if len(j.Spec.Relay) > 0 || j.Spec.Network == "client" {
				j.relay = newRelay() // its sockets are created when it starts
			}
			if len(j.Spec.SecretNames) > 0 { // secrets were only in memory
				j.State, j.Reason = api.StateFailed, "runner restarted before the job started; its secrets were dropped"
				now := time.Now()
				j.FinishedAt = &now
				j.save()
			}
		case api.StateRunning:
			if j.StopRequested { // cancelled before the restart: keep stopping it
				j.cancel = true
				stopUnit(j.Unit)
			}
			if len(j.Spec.Relay) > 0 || j.Spec.Network == "client" {
				// Its relay sockets are (re)created in its workload dir, still mounted in its
				// container, by ensureRelays, which keeps retrying until they bind.
				j.relay = newRelay()
			}
			if j.AllowedCPUs != "" {
				j.cpus, _ = units.ParseCPUList(j.AllowedCPUs)
				for _, c := range j.cpus {
					s.cpuUse[c]++
				}
			}
		}
		if !api.Terminal(j.State) && j.slot != nil {
			j.slot.inUse = j.ID
		}
		s.jobs[j.ID] = j
	}
	s.restoreSessions()
}

func (s *Server) findSlot(id string) *slot {
	key, idx, _ := strings.Cut(id, "/")
	for _, sl := range s.slots[key] {
		if fmt.Sprint(sl.Index) == idx {
			return sl
		}
	}
	return nil
}

// acquireSlotLocked hands out a free slot for key, creating one if allowed.
func (s *Server) acquireSlotLocked(key, owner string) (*slot, error) {
	var free []*slot
	for _, sl := range s.slots[key] {
		if sl.inUse == "" {
			free = append(free, sl)
		}
	}
	if len(free) > 0 {
		sort.Slice(free, func(i, j int) bool { return free[i].LastUsed.After(free[j].LastUsed) })
		free[0].inUse = owner
		return free[0], nil
	}
	if len(s.slots[key]) >= s.cfg.Storage.MaxSlotsPerKey {
		return nil, fmt.Errorf("all %d workspace slots for %s are busy", len(s.slots[key]), key)
	}
	used := map[int]bool{}
	for _, sl := range s.slots[key] {
		used[sl.Index] = true
	}
	idx := 0
	for used[idx] {
		idx++
	}
	sl := &slot{Key: key, Index: idx, Dir: filepath.Join(s.cfg.StateDir, "work", key, fmt.Sprint(idx)), inUse: owner, LastUsed: time.Now()}
	if err := os.MkdirAll(sl.Dir, 0o755); err != nil {
		return nil, err
	}
	s.slots[key] = append(s.slots[key], sl)
	return sl, nil
}

func (s *Server) handleCreateJob(w http.ResponseWriter, r *http.Request) {
	var out reply // written after s.mu is released (deferred first, so it runs last)
	defer out.send(w)
	var spec api.JobSpec
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 256<<20)).Decode(&spec); err != nil {
		out.err(400, "bad job spec: "+err.Error())
		return
	}
	if len(spec.Argv) == 0 {
		out.err(400, "argv is empty")
		return
	}
	if spec.Dir != "" {
		if _, err := cleanRel(spec.Dir); err != nil && spec.Dir != "." {
			out.err(400, err.Error())
			return
		}
	}
	profiles := s.cfg.loadProfiles()
	var prof *Profile
	if spec.Profile != "" {
		if prof = profiles[spec.Profile]; prof == nil {
			out.err(400, "unknown profile "+spec.Profile)
			return
		}
	} else if prof = matchProfile(profiles, spec.Argv); prof != nil {
		spec.Profile = prof.Name
	}
	if spec.Client == "" {
		spec.Client = r.Header.Get("X-Offload-Caller")
	}
	if err := validSecrets(spec.Secrets, spec.SecretFiles); err != nil {
		out.err(400, err.Error())
		return
	}
	if spec.Platform != "" && spec.Platform != goruntime.GOOS+"/"+goruntime.GOARCH {
		out.err(400, "this runner is "+goruntime.GOOS+"/"+goruntime.GOARCH+", not "+spec.Platform)
		return
	}
	rt := "host"
	switch {
	case spec.Runtime == "container" && !s.containersAvailable():
		out.err(400, "containers are not available on this runner (install podman)")
		return
	case spec.Runtime == "container", spec.Runtime == "" && s.containersAvailable():
		rt = "container"
	case spec.Runtime != "" && spec.Runtime != "host":
		out.err(400, "runtime must be host or container")
		return
	}
	if e := spec.Environment; e != nil && contextRel(e.ContextDir) != "." {
		if _, err := cleanRel(e.ContextDir); err != nil {
			writeErr(w, 400, "environment context: "+err.Error())
			return
		}
	}
	if rt == "host" && spec.Environment != nil {
		out.err(400, "this job has a container environment but runs on the host; drop --host or install podman")
		return
	}
	j := &job{Job: api.Job{
		ID: newID("j-"), Runner: s.cfg.Name, State: api.StateUploading, Spec: spec,
		Resources: s.cfg.effective(spec.Resources, prof), CreatedAt: time.Now(), Runtime: rt,
	}}
	if rt == "container" {
		j.Image, _, _ = envPlan(spec.Environment)
	}
	j.dir = s.jobDir(j.ID)
	// Secrets stay in memory; the record on disk only names them.
	j.secrets, j.secretFiles = spec.Secrets, spec.SecretFiles
	j.Spec.Secrets, j.Spec.SecretFiles = nil, nil
	for k := range j.secrets {
		j.Spec.SecretNames = append(j.Spec.SecretNames, k)
	}
	for k := range j.secretFiles {
		j.Spec.SecretNames = append(j.Spec.SecretNames, k)
	}
	sort.Strings(j.Spec.SecretNames)
	if spec.Network == "client" && rt != "container" {
		out.err(400, "--network client needs the container runtime on the runner")
		return
	}
	if len(spec.Relay) > 0 || spec.Network == "client" {
		j.relay = newRelay()
		if !spec.Detached {
			j.relay.lastPoll = time.Now() // the client starts polling right after creating the job
		}
	}
	j.MemFrom = "requested"
	if spec.Resources.Mem == 0 {
		j.AutoMem, j.MemFrom = true, "default"
		if prof != nil && prof.Resources.Mem != "" {
			j.MemFrom = "profile " + prof.Name
		}
		s.mu.Lock()
		if m := s.suggestMemLocked(spec); m > 0 {
			j.Resources.Mem, j.MemFrom = m, "learned"
		}
		s.mu.Unlock()
	}
	if j.Resources.Mem > s.cfg.memCap {
		out.err(400, fmt.Sprintf("job asks for %s memory; this runner hands out at most %s", units.FormatSize(j.Resources.Mem), units.FormatSize(s.cfg.memCap)))
		return
	}
	if err := os.MkdirAll(filepath.Join(j.dir, "tmp"), 0o700); err != nil {
		out.err(500, err.Error())
		return
	}
	for _, f := range []string{"output.log", "stderr.log"} {
		if err := os.WriteFile(filepath.Join(j.dir, f), nil, 0o600); err != nil {
			out.err(500, err.Error())
			return
		}
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if spec.WorkspaceKey == "" {
		if err := os.MkdirAll(filepath.Join(j.dir, "work"), 0o755); err != nil {
			out.err(500, err.Error())
			return
		}
		j.State, j.queuedAt = api.StateQueued, time.Now()
	} else {
		if !validKey(spec.WorkspaceKey) {
			out.err(400, "invalid workspace key")
			return
		}
		slotKey := spec.WorkspaceKey
		if rt == "container" {
			_, _, variant := envPlan(spec.Environment)
			slotKey += "--" + variant // build output is kept apart per environment base
		}
		sl, err := s.acquireSlotLocked(slotKey, j.ID)
		if err != nil {
			_ = os.RemoveAll(j.dir)
			out.err(http.StatusConflict, err.Error())
			return
		}
		if spec.Fresh {
			if err := sl.wipe(); err != nil {
				sl.inUse = ""
				out.err(500, err.Error())
				return
			}
		}
		needed, err := sl.plan(spec.Manifest)
		if err != nil {
			sl.inUse = ""
			_ = os.RemoveAll(j.dir)
			out.err(400, err.Error())
			return
		}
		j.slot, j.Slot = sl, sl.id()
		j.Needed, j.SyncFiles = needed, len(spec.Manifest)
		if spec.Git != nil {
			if !validGitRef(spec.Git) {
				sl.inUse = ""
				_ = os.RemoveAll(j.dir)
				out.err(400, "invalid git ref")
				return
			}
			var err error
			if j.GitNeeded, j.GitHave, err = s.gitPlan(spec.WorkspaceKey, spec.Git.Head); err != nil {
				j.Spec.Git = nil // git unavailable here; run without it
				s.note(j, "git metadata unavailable on this runner: "+err.Error())
			}
		}
	}
	j.Proto = api.Proto
	if err := j.save(); err != nil {
		if j.slot != nil {
			s.releaseSlotLocked(j.slot, false)
		}
		_ = os.RemoveAll(j.dir)
		out.err(500, "can't record the job: "+err.Error())
		return
	}
	s.jobs[j.ID] = j
	out.json(j.view())
	j.Needed, j.GitHave = nil, nil
	s.kick()
}

// note appends an offload message to the job's stderr, so the caller sees it in order.
func (s *Server) note(j *job, msg string) {
	f, err := os.OpenFile(filepath.Join(j.dir, "stderr.log"), os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		return
	}
	fmt.Fprintf(f, "offload: %s\n", msg)
	f.Close()
}

// handleGitBundle receives the commits a job's workspace is missing.
func (s *Server) handleGitBundle(w http.ResponseWriter, r *http.Request) {
	gitCtx, stopGit := context.WithCancel(r.Context())
	defer stopGit()
	s.mu.Lock()
	j := s.jobs[r.PathValue("id")]
	ok := j != nil && j.State == api.StateUploading && j.Spec.Git != nil && !j.syncing
	if ok {
		j.syncing = true
		rc := http.NewResponseController(w)
		j.abortUpload = func() { _ = rc.SetReadDeadline(time.Now()); stopGit() } // DELETE interrupts the read, or git
	}
	s.mu.Unlock()
	if !ok {
		writeErr(w, 409, "job is not waiting for git history")
		return
	}
	defer func() {
		s.mu.Lock()
		j.syncing, j.abortUpload = false, nil
		if j.cancel && j.State == api.StateUploading { // cancelled while the bundle was arriving
			s.finishLocked(j, api.StateCancelled, nil, "cancelled")
		}
		s.mu.Unlock()
	}()
	tmp, err := os.CreateTemp(j.dir, "bundle-*")
	if err == nil {
		_, err = tmp.ReadFrom(uploadBody(w, r, 16<<30, j.cancelled.Load))
		if cerr := tmp.Close(); err == nil {
			err = cerr
		}
		defer os.Remove(tmp.Name())
	}
	if err == nil {
		err = s.gitReceive(gitCtx, j.Spec.WorkspaceKey, j.Spec.Git.Head, tmp.Name())
	}
	if err != nil {
		s.note(j, "git history upload failed; running without .git: "+err.Error())
		s.mu.Lock()
		j.Spec.Git = nil
		s.mu.Unlock()
		writeErr(w, 400, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleUpload(w http.ResponseWriter, r *http.Request) {
	var out reply // written after s.mu is released (deferred first, so it runs last)
	defer out.send(w)
	s.mu.Lock()
	j := s.jobs[r.PathValue("id")]
	if j == nil {
		s.mu.Unlock()
		out.err(404, "no such job")
		return
	}
	if j.State != api.StateUploading || j.slot == nil || j.syncing {
		s.mu.Unlock()
		out.err(409, "job is not waiting for source")
		return
	}
	j.syncing = true
	rc := http.NewResponseController(w)
	gitCtx, stopGit := context.WithCancel(r.Context())
	defer stopGit()
	j.abortUpload = func() { _ = rc.SetReadDeadline(time.Now()); stopGit() } // DELETE interrupts the read, or git
	sl, manifest := j.slot, j.Spec.Manifest
	s.mu.Unlock()

	needed, err := sl.plan(manifest)
	if err == nil {
		err = sl.apply(manifest, needed, uploadBody(w, r, 8<<30, j.cancelled.Load))
	}
	s.mu.Lock()
	g := j.Spec.Git
	s.mu.Unlock()
	if err == nil && g != nil {
		if gerr := s.gitCheckout(gitCtx, sl, j.Spec.WorkspaceKey, g); gerr != nil {
			s.note(j, "running without .git: "+gerr.Error())
			// Never a previous job's .git describing another commit.
			if root, err := os.OpenRoot(sl.Dir); err == nil {
				_ = root.RemoveAll(".git")
				root.Close()
			}
		}
	} else if err == nil {
		// No git for this job: don't leave a stale .git describing some other commit.
		if root, err := os.OpenRoot(sl.Dir); err == nil {
			_ = root.RemoveAll(".git")
			root.Close()
		}
	}

	if err == nil && j.Spec.GitTree {
		s.pruneUntracked(gitCtx, sl, manifest)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	j.syncing, j.abortUpload = false, nil
	if j.cancel {
		s.finishLocked(j, api.StateCancelled, nil, "cancelled")
		out.err(409, "job was cancelled during upload")
		return
	}
	if err != nil {
		s.finishLocked(j, api.StateFailed, nil, "source sync: "+err.Error())
		out.err(400, "source sync: "+err.Error())
		return
	}
	j.State, j.queuedAt = api.StateQueued, time.Now()
	if e := j.Spec.Environment; e != nil {
		ctx := contextRel(e.ContextDir)
		if problem := contextProblem(manifest, ctx); problem != "" && usesContext(e.Containerfile) {
			s.finishLocked(j, api.StateFailed, nil, "environment context: "+problem)
			out.err(400, "environment context: "+problem)
			return
		}
		j.ContextDigest = contextDigest(manifest, ctx)
	}
	j.Spec.Manifest = nil // synced; no need to keep it on disk
	if err := j.save(); err != nil {
		s.finishLocked(j, api.StateFailed, nil, "can't record the synced job: "+err.Error())
		out.err(500, "can't record the synced job: "+err.Error())
		return
	}
	out.json(j.view())
	s.kick()
}

// uploadTimeout bounds how long a created job may wait for its source (or git history) upload.
const uploadTimeout = 10 * time.Minute

// reapUploads fails jobs whose client created them but never finished uploading, so their
// workspace slot doesn't stay reserved forever.
func (s *Server) reapUploads() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, j := range s.jobs {
		if j.State == api.StateUploading && !j.syncing && time.Since(j.CreatedAt) > uploadTimeout {
			s.finishLocked(j, api.StateFailed, nil, "source upload abandoned")
		}
	}
}

// retireOtherVersions stops, before the runner serves anything, every workload started by a
// runner of another protocol version (their mounts and sockets differ and may expose more than
// current ones), and removes offload containers that no current workload owns. Nothing from an
// older version ever runs next to new work.
func (s *Server) retireOtherVersions() error {
	type victim struct {
		unit   string
		finish func()
	}
	var victims []victim
	live, known := map[string]bool{}, map[string]bool{}
	s.mu.Lock()
	for id := range s.jobs {
		known["offload-"+id] = true
	}
	for id := range s.sessions {
		known["offload-"+id] = true
	}
	for _, j := range s.jobs {
		if api.Terminal(j.State) {
			continue
		}
		if j.State == api.StateRunning && j.Proto != api.Proto {
			j := j
			log.Printf("job %s: started by protocol %d, stopping it (runner speaks %d)", j.ID, j.Proto, api.Proto)
			victims = append(victims, victim{j.Unit, func() {
				if !api.Terminal(j.State) {
					s.finishLocked(j, api.StateFailed, nil, "stopped: the runner was upgraded while it ran")
				}
			}})
			continue
		}
		live["offload-"+j.ID] = true
	}
	for _, se := range s.sessions {
		if api.Terminal(se.State) {
			continue
		}
		if se.Proto != api.Proto {
			se := se
			log.Printf("session %s: started by protocol %d, stopping it", se.ID, se.Proto)
			victims = append(victims, victim{se.Unit, func() {
				if !api.Terminal(se.State) {
					s.finishSessionLocked(se, nil, "runner upgraded; start the session again")
				}
			}})
			continue
		}
		live["offload-"+se.ID] = true
	}
	s.mu.Unlock()
	var problems []string
	for _, v := range victims {
		if err := stopUnitConfirmed(v.unit); err != nil {
			problems = append(problems, err.Error())
			continue
		}
		s.mu.Lock()
		v.finish()
		s.mu.Unlock()
	}
	if s.containersAvailable() {
		owner := strings.TrimPrefix(s.ownerLabel(), "dev.offload.runner=")
		out, err := podman(context.Background(), "ps", "-a", "--filter", "name=^offload-[js]-",
			"--format", `{{.Names}} {{index .Labels "dev.offload.runner"}}`)
		if err != nil {
			problems = append(problems, "listing containers: "+err.Error())
		} else {
			for _, line := range strings.Split(out, "\n") {
				f := strings.Fields(line)
				if len(f) == 0 || live[f[0]] {
					continue
				}
				name, label := f[0], ""
				if len(f) > 1 {
					label = f[1]
				}
				// Ours by label, or (made before labels existed) by our own records. Anything
				// else belongs to another runner of this user.
				if label != owner && !(label == "" && known[name]) {
					continue
				}
				log.Printf("removing container %s: no current workload owns it", name)
				if _, err := podman(context.Background(), "rm", "-f", name); err != nil {
					problems = append(problems, "removing container "+name+": "+err.Error())
				}
			}
		}
	}
	if len(problems) > 0 {
		return errors.New(strings.Join(problems, "; "))
	}
	return nil
}

// stopUnitConfirmed stops a unit, waiting, and confirms it is gone.
func stopUnitConfirmed(unit string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	err := exec.CommandContext(ctx, "systemctl", "--user", "stop", unit).Run() // waits for it
	if active, known := unitActive(unit); active || !known {
		return fmt.Errorf("stopping %s: still running or unknown (%v)", unit, err)
	}
	return nil
}

// admit starts queued jobs that fit, oldest first. Small jobs may pass a big one, unless the
// big one has waited over two minutes (then nothing passes it, so it can't starve).
func (s *Server) admit() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.retireErr != "" || s.upgrading {
		return // workloads of an older version may still run: start nothing until they're gone
	}
	var queued []*job
	for _, j := range s.jobs {
		if j.State == api.StateQueued && !j.cancel {
			queued = append(queued, j)
		}
	}
	sort.Slice(queued, func(a, b int) bool { return queued[a].queuedAt.Before(queued[b].queuedAt) })
	for _, j := range queued {
		if j.Runtime == "container" && !s.ensureImageLocked(j) {
			if j.State == api.StateQueued {
				j.Reason = "waiting: building environment"
			}
			continue // a build never blocks other jobs
		}
		ok, why := s.fitsLocked(j.Resources)
		if !ok {
			if j.Reason != "waiting: "+why {
				j.Reason = "waiting: " + why
			}
			if time.Since(j.queuedAt) > 2*time.Minute {
				return
			}
			continue
		}
		j.Reason = ""
		if err := s.startJobLocked(j); err != nil {
			log.Printf("job %s: start: %v", j.ID, err)
			s.finishLocked(j, api.StateFailed, nil, "start: "+err.Error())
		}
	}
}

// makeWorkDir creates the job's working directory through real directories only. A previous
// job may have left a symlink where a directory of the path should be (one with no synced files,
// so sync never replaced it); the runner's wrapper must never start outside the workspace.
func (s *Server) makeWorkDir(j *job) error {
	base := filepath.Join(j.dir, "work")
	if j.slot != nil {
		base = j.slot.Dir
	}
	if err := os.MkdirAll(base, 0o755); err != nil {
		return err
	}
	if j.Spec.Dir == "" || j.Spec.Dir == "." {
		return nil
	}
	root, err := os.OpenRoot(base)
	if err != nil {
		return err
	}
	defer root.Close()
	parts := strings.Split(j.Spec.Dir, "/")
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
			return fmt.Errorf("working directory %q: %q is not a directory on the runner (a previous job changed it); run with --fresh", j.Spec.Dir, p)
		}
	}
	return nil
}

func (s *Server) workDir(j *job) string {
	root := filepath.Join(j.dir, "work")
	if j.slot != nil {
		root = j.slot.Dir
	}
	if j.Spec.Dir != "" && j.Spec.Dir != "." {
		return filepath.Join(root, filepath.FromSlash(j.Spec.Dir))
	}
	return root
}

func (s *Server) baseEnv(prof *Profile, cpus int) map[string]string {
	env := map[string]string{}
	home, _ := os.UserHomeDir()
	env["HOME"] = home
	env["PATH"] = "/usr/local/bin:/usr/bin:/bin"
	for k, v := range s.cfg.Env {
		env[k] = v
	}
	for k, v := range parallelEnv(cpus) {
		env[k] = v
	}
	if prof != nil {
		for k, v := range prof.Env {
			env[k] = v
		}
	}
	return env
}

func (s *Server) startJobLocked(j *job) error {
	var prof *Profile
	if j.Spec.Profile != "" {
		prof = s.cfg.loadProfiles()[j.Spec.Profile]
	}
	ncpu := int(math.Ceil(j.Resources.CPUMax))
	j.cpus = s.pickCPUsLocked(ncpu)
	container := j.Runtime == "container"
	env := s.baseEnv(prof, ncpu)
	if container {
		env = s.containerEnv(prof, ncpu, j)
	}
	env["TMPDIR"] = filepath.Join(j.dir, "tmp")
	env["OFFLOAD_JOB"] = j.ID
	for k, v := range j.Spec.Env {
		env[k] = v
	}
	dir := s.workDir(j)
	if err := s.makeWorkDir(j); err != nil {
		s.releaseCPUsLocked(j.cpus)
		j.cpus = nil // released exactly once: finishLocked must not release them again
		return err
	}
	j.Unit = "offload-" + j.ID + ".service"
	if len(j.cpus) > 0 {
		j.AllowedCPUs = units.FormatCPUList(j.cpus)
	}
	argv := append([]string{s.self, "runner", "exec", "--job-dir", j.dir}, artifactArgs(j.Spec.Artifacts)...)
	secrets, err := s.setupCredentials(j, env)
	if err != nil {
		s.releaseCPUsLocked(j.cpus)
		s.cleanupCredentials(j)
		j.cpus, j.AllowedCPUs, j.Unit = nil, "", ""
		return err
	}
	if secrets != "" && !container {
		argv = append(argv, "--secrets", secrets)
	}
	if j.slot != nil && j.Spec.WriteBack {
		argv = append(argv, "--write-back", j.slot.Dir, "--record", j.slot.recordPath())
	}
	argv = append(argv, "--")
	unitEnv := env
	if container {
		// The unit runs podman with the user manager's environment; the job's variables go to
		// the container, and its secrets are loaded inside from the tmpfs mount.
		argv = append(argv, s.containerArgv(j, env, dir, secrets)...)
		unitEnv = map[string]string{}
		s.touchImage(j.Image)
	} else {
		argv = append(argv, j.Spec.Argv...)
	}
	u := unitSpec{
		Name: j.Unit, Slice: s.cfg.Slice, Dir: dir, Env: unitEnv, Argv: argv, Delegate: container,
		CPUWeight: cpuWeight(j.Resources.CPU), CPUQuotaPct: int(math.Ceil(j.Resources.CPUMax * 100)),
		AllowedCPUs: j.AllowedCPUs, MemoryMax: j.Resources.Mem, OOMScoreAdj: s.cfg.Policy.OOMScoreAdj,
		Stdout: filepath.Join(j.dir, "output.log"),
		Stderr: filepath.Join(j.dir, "stderr.log"),
	}
	if s.features["io"] {
		u.IOWeight = s.cfg.Policy.IOWeight
	}
	// Record "running" with the unit name before systemd starts it: if the runner dies right after
	// the start, restore sees a running job and watches its unit instead of starting it again.
	now := time.Now()
	j.State, j.StartedAt = api.StateRunning, &now
	j.Proto = api.Proto // the version that launched it (a queued job may predate an upgrade)
	if err := j.save(); err != nil {
		s.releaseCPUsLocked(j.cpus)
		s.cleanupCredentials(j)
		j.cpus, j.AllowedCPUs, j.Unit = nil, "", ""
		j.State, j.StartedAt = api.StateQueued, nil
		return fmt.Errorf("can't record the job before starting it: %w", err)
	}
	if err := startUnit(contextBG(), u); err != nil {
		if active, known := unitActive(u.Name); !known || active {
			// The unit may be running (systemd-run timed out after submitting it): keep the
			// job "running" with its resources; checkRunning settles it once systemd answers.
			log.Printf("job %s: systemd-run reported %v; unit state: active=%v known=%v, keeping the job", j.ID, err, active, known)
			return nil
		}
		s.releaseCPUsLocked(j.cpus)
		s.cleanupCredentials(j)
		j.cpus, j.AllowedCPUs, j.Unit = nil, "", ""
		j.State, j.StartedAt = api.StateQueued, nil
		return err
	}
	return nil
}

func artifactArgs(paths []string) []string {
	var out []string
	for _, p := range paths {
		out = append(out, "--artifact", p)
	}
	return out
}

// checkRunning finishes jobs whose wrapper wrote an exit record or whose unit disappeared.
func (s *Server) checkRunning() {
	s.mu.Lock()
	var running []*job
	for _, j := range s.jobs {
		if j.State == api.StateRunning {
			running = append(running, j)
		}
	}
	s.mu.Unlock()
	for _, j := range running {
		rec, err := readExit(filepath.Join(j.dir, "exit.json"))
		if err != nil && time.Since(j.checked) < time.Second {
			continue // asking systemd is slower; once a second is plenty for crashed units
		}
		j.checked = time.Now()
		// An exit record only counts once systemd confirms the unit is gone too.
		active, known := unitActive(j.Unit)
		s.mu.Lock()
		switch {
		case !known || (err == nil && active):
			// systemd didn't answer, or the wrapper is still exiting: no conclusion yet.
		case err == nil:
			state, reason := api.StateExited, rec.Error
			if rec.OOM {
				state, reason = api.StateFailed, fmt.Sprintf("out of memory (limit %s)", units.FormatSize(j.Resources.Mem))
			}
			if j.cancel {
				state, reason = api.StateCancelled, "cancelled"
			}
			code := rec.Code
			j.Signal, j.HasArtifact, j.WriteBack, j.PeakMem = rec.Signal, rec.Artifact, rec.WriteBack, rec.PeakMem
			s.learn(j, rec)
			s.finishLocked(j, state, &code, reason)
		case active:
			j.inactive = 0
		default:
			// The unit is gone without a record: give the wrapper a moment, then call it killed.
			if j.inactive++; j.inactive >= 3 {
				state, reason := api.StateFailed, "killed before it could report an exit status"
				if j.cancel {
					state, reason = api.StateCancelled, "cancelled"
				}
				s.finishLocked(j, state, nil, reason)
			}
		}
		s.mu.Unlock()
	}
}

func readExit(path string) (*exitRecord, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var rec exitRecord
	if err := json.Unmarshal(b, &rec); err != nil {
		return nil, err
	}
	return &rec, nil
}

// finishLocked moves a job to a terminal state and releases its CPUs and slot.
func (s *Server) finishLocked(j *job, state string, code *int, reason string) {
	now := time.Now()
	j.State, j.ExitCode, j.Reason, j.FinishedAt = state, code, reason, &now
	s.cleanupCredentials(j)
	s.releaseCPUsLocked(j.cpus)
	j.cpus = nil
	if j.slot != nil {
		if j.slot.leases > 0 {
			// A build is still snapshotting it; the build releases it afterwards.
			j.slot.releaseAfterBuild = true
			j.slot.dropAfterBuild = !s.cfg.persistent || j.Spec.Ephemeral
		} else {
			s.releaseSlotLocked(j.slot, !s.cfg.persistent || j.Spec.Ephemeral)
		}
		j.slot = nil
	}
	j.save()
	s.kick()
}

func (s *Server) releaseSlotLocked(sl *slot, drop bool) {
	sl.inUse, sl.LastUsed, sl.releaseAfterBuild = "", time.Now(), false
	if drop {
		s.dropSlotLocked(sl)
	}
}

func (s *Server) dropSlotLocked(sl *slot) {
	_ = removeAll(sl.Dir)
	_ = os.Remove(sl.recordPath())
	list := s.slots[sl.Key]
	for i, x := range list {
		if x == sl {
			s.slots[sl.Key] = append(list[:i], list[i+1:]...)
			break
		}
	}
	if len(s.slots[sl.Key]) == 0 {
		delete(s.slots, sl.Key)
		_ = os.Remove(filepath.Dir(sl.Dir))
	}
}

func (s *Server) handleCancelJob(w http.ResponseWriter, r *http.Request) {
	var out reply // written after s.mu is released (deferred first, so it runs last)
	defer out.send(w)
	s.mu.Lock()
	defer s.mu.Unlock()
	j := s.jobs[r.PathValue("id")]
	if j == nil {
		out.err(404, "no such job")
		return
	}
	j.cancelled.Store(true)
	if !api.Terminal(j.State) && !j.StopRequested {
		// Recorded before it is acknowledged: a restart must never bring a cancelled job back.
		j.StopRequested = true
		if err := j.save(); err != nil {
			j.StopRequested = false
			out.err(500, "can't record the cancel: "+err.Error())
			return
		}
	}
	switch j.State {
	case api.StateUploading, api.StateQueued:
		if j.syncing {
			j.cancel = true // the upload handler finishes it once the slot is consistent
			if j.abortUpload != nil {
				j.abortUpload()
			}
		} else {
			s.finishLocked(j, api.StateCancelled, nil, "cancelled")
		}
	case api.StateRunning:
		j.cancel = true
		stopUnit(j.Unit)
	}
	out.json(j.view())
}

func (s *Server) handleGetJob(w http.ResponseWriter, r *http.Request) {
	var out reply // written after s.mu is released (deferred first, so it runs last)
	defer out.send(w)
	s.mu.Lock()
	defer s.mu.Unlock()
	j := s.jobs[r.PathValue("id")]
	if j == nil {
		out.err(404, "no such job")
		return
	}
	out.json(j.view())
}

func (s *Server) handleListJobs(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	out := []api.Job{}
	all := r.URL.Query().Get("all") != ""
	for _, j := range s.jobs {
		if all || !api.Terminal(j.State) || (j.FinishedAt != nil && time.Since(*j.FinishedAt) < time.Hour) {
			c := j.Job
			c.Spec.Manifest = nil
			out = append(out, c)
		}
	}
	s.mu.Unlock()
	sort.Slice(out, func(a, b int) bool { return out[a].CreatedAt.Before(out[b].CreatedAt) })
	writeJSON(w, out)
}

// handleJobLogs streams output.log from ?offset=, following until the job ends when ?follow=1.
func (s *Server) handleJobLogs(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	s.mu.Lock()
	j := s.jobs[id]
	s.mu.Unlock()
	if j == nil {
		writeErr(w, 404, "no such job")
		return
	}
	var offset int64
	fmt.Sscan(r.URL.Query().Get("offset"), &offset)
	follow := r.URL.Query().Get("follow") != ""
	done := func() bool {
		s.mu.Lock()
		defer s.mu.Unlock()
		return api.Terminal(j.State)
	}
	file := "output.log"
	if r.URL.Query().Get("stream") == "stderr" {
		file = "stderr.log"
	}
	streamFile(w, r, filepath.Join(j.dir, file), offset, follow, done)
}

func (s *Server) handleWriteBack(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	j := s.jobs[r.PathValue("id")]
	s.mu.Unlock()
	if j == nil {
		writeErr(w, 404, "no such job")
		return
	}
	p := filepath.Join(j.dir, "writeback.tar.gz")
	if _, err := os.Stat(p); err != nil {
		writeErr(w, 404, "no write-back")
		return
	}
	w.Header().Set("Content-Type", "application/gzip")
	http.ServeFile(w, r, p)
}

// streamFile copies path from offset to the response, polling for growth while follow is set
// and done() is false. It always drains the file after done() turns true.
func streamFile(w http.ResponseWriter, r *http.Request, path string, offset int64, follow bool, done func() bool) {
	f, err := os.Open(path)
	if err != nil {
		writeErr(w, 404, "no output yet")
		return
	}
	defer f.Close()
	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("X-Accel-Buffering", "no")
	flusher, _ := w.(http.Flusher)
	buf := make([]byte, 64<<10)
	for {
		finished := done()
		for {
			n, err := f.Read(buf)
			if n > 0 {
				if _, werr := w.Write(buf[:n]); werr != nil {
					return
				}
			}
			if err != nil {
				if !errors.Is(err, io.EOF) {
					return
				}
				break
			}
		}
		if flusher != nil {
			flusher.Flush()
		}
		if !follow || finished {
			return
		}
		select {
		case <-r.Context().Done():
			return
		case <-time.After(200 * time.Millisecond):
		}
	}
}

func (s *Server) handleArtifacts(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	j := s.jobs[r.PathValue("id")]
	s.mu.Unlock()
	if j == nil {
		writeErr(w, 404, "no such job")
		return
	}
	p := filepath.Join(j.dir, "artifacts.tar.gz")
	if _, err := os.Stat(p); err != nil {
		writeErr(w, 404, "no artifacts")
		return
	}
	w.Header().Set("Content-Type", "application/gzip")
	http.ServeFile(w, r, p)
}

// gc drops finished jobs past retention and workspace slots unused for too long.
func (s *Server) gc() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.gcContainersLocked()
	for id, j := range s.jobs {
		if api.Terminal(j.State) && j.FinishedAt != nil && time.Since(*j.FinishedAt) > s.cfg.jobsRetain {
			_ = os.RemoveAll(j.dir)
			delete(s.jobs, id)
		}
	}
	for id, se := range s.sessions {
		if api.Terminal(se.State) && se.FinishedAt != nil && time.Since(*se.FinishedAt) > s.cfg.jobsRetain {
			_ = os.RemoveAll(se.dir)
			delete(s.sessions, id)
		}
	}
	for _, list := range s.slots {
		for _, sl := range append([]*slot(nil), list...) {
			if sl.inUse == "" && time.Since(sl.LastUsed) > s.cfg.cacheRetain {
				log.Printf("gc: dropping workspace slot %s (unused since %s)", sl.id(), sl.LastUsed.Format(time.DateTime))
				s.dropSlotLocked(sl)
			}
		}
	}
}
