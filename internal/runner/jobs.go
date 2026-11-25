package runner

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
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
	inactive    int // consecutive checks the unit was gone without an exit record
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

func (j *job) save() {
	if err := writeFileJSON(filepath.Join(j.dir, "job.json"), j.Job); err != nil {
		log.Printf("job %s: save: %v", j.ID, err)
	}
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
			now := time.Now()
			j.FinishedAt = &now
			j.save()
		case api.StateQueued:
			j.queuedAt = j.CreatedAt
			if len(j.Spec.SecretNames) > 0 { // secrets were only in memory
				j.State, j.Reason = api.StateFailed, "runner restarted before the job started; its secrets were dropped"
				now := time.Now()
				j.FinishedAt = &now
				j.save()
			}
		case api.StateRunning:
			if len(j.Spec.Relay) > 0 {
				j.relay = newRelay() // the SSH agent socket is gone, but git relaying resumes
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
	var spec api.JobSpec
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 256<<20)).Decode(&spec); err != nil {
		writeErr(w, 400, "bad job spec: "+err.Error())
		return
	}
	if len(spec.Argv) == 0 {
		writeErr(w, 400, "argv is empty")
		return
	}
	if spec.Dir != "" {
		if _, err := cleanRel(spec.Dir); err != nil && spec.Dir != "." {
			writeErr(w, 400, err.Error())
			return
		}
	}
	profiles := s.cfg.loadProfiles()
	var prof *Profile
	if spec.Profile != "" {
		if prof = profiles[spec.Profile]; prof == nil {
			writeErr(w, 400, "unknown profile "+spec.Profile)
			return
		}
	} else if prof = matchProfile(profiles, spec.Argv); prof != nil {
		spec.Profile = prof.Name
	}
	if spec.Client == "" {
		spec.Client = r.Header.Get("X-Offload-Caller")
	}
	j := &job{Job: api.Job{
		ID: newID("j-"), Runner: s.cfg.Name, State: api.StateUploading, Spec: spec,
		Resources: s.cfg.effective(spec.Resources, prof), CreatedAt: time.Now(),
	}}
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
	if len(spec.Relay) > 0 {
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
		writeErr(w, 400, fmt.Sprintf("job asks for %s memory; this runner hands out at most %s", units.FormatSize(j.Resources.Mem), units.FormatSize(s.cfg.memCap)))
		return
	}
	if err := os.MkdirAll(filepath.Join(j.dir, "tmp"), 0o700); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	for _, f := range []string{"output.log", "stderr.log"} {
		if err := os.WriteFile(filepath.Join(j.dir, f), nil, 0o600); err != nil {
			writeErr(w, 500, err.Error())
			return
		}
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if spec.WorkspaceKey == "" {
		if err := os.MkdirAll(filepath.Join(j.dir, "work"), 0o755); err != nil {
			writeErr(w, 500, err.Error())
			return
		}
		j.State, j.queuedAt = api.StateQueued, time.Now()
	} else {
		if !validKey(spec.WorkspaceKey) {
			writeErr(w, 400, "invalid workspace key")
			return
		}
		sl, err := s.acquireSlotLocked(spec.WorkspaceKey, j.ID)
		if err != nil {
			_ = os.RemoveAll(j.dir)
			writeErr(w, http.StatusConflict, err.Error())
			return
		}
		if spec.Fresh {
			if err := sl.wipe(); err != nil {
				sl.inUse = ""
				writeErr(w, 500, err.Error())
				return
			}
		}
		needed, err := sl.plan(spec.Manifest)
		if err != nil {
			sl.inUse = ""
			_ = os.RemoveAll(j.dir)
			writeErr(w, 400, err.Error())
			return
		}
		j.slot, j.Slot = sl, sl.id()
		j.Needed, j.SyncFiles = needed, len(spec.Manifest)
		if spec.Git != nil {
			if !validGitRef(spec.Git) {
				sl.inUse = ""
				_ = os.RemoveAll(j.dir)
				writeErr(w, 400, "invalid git ref")
				return
			}
			var err error
			if j.GitNeeded, j.GitHave, err = s.gitPlan(spec.WorkspaceKey, spec.Git.Head); err != nil {
				j.Spec.Git = nil // git unavailable here; run without it
				s.note(j, "git metadata unavailable on this runner: "+err.Error())
			}
		}
	}
	s.jobs[j.ID] = j
	j.save()
	writeJSON(w, j.Job)
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
	s.mu.Lock()
	j := s.jobs[r.PathValue("id")]
	ok := j != nil && j.State == api.StateUploading && j.Spec.Git != nil && !j.syncing
	if ok {
		j.syncing = true
	}
	s.mu.Unlock()
	if !ok {
		writeErr(w, 409, "job is not waiting for git history")
		return
	}
	defer func() { s.mu.Lock(); j.syncing = false; s.mu.Unlock() }()
	tmp, err := os.CreateTemp(j.dir, "bundle-*")
	if err == nil {
		_, err = tmp.ReadFrom(http.MaxBytesReader(w, r.Body, 16<<30))
		if cerr := tmp.Close(); err == nil {
			err = cerr
		}
		defer os.Remove(tmp.Name())
	}
	if err == nil {
		err = s.gitReceive(j.Spec.WorkspaceKey, j.Spec.Git.Head, tmp.Name())
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
	s.mu.Lock()
	j := s.jobs[r.PathValue("id")]
	if j == nil {
		s.mu.Unlock()
		writeErr(w, 404, "no such job")
		return
	}
	if j.State != api.StateUploading || j.slot == nil || j.syncing {
		s.mu.Unlock()
		writeErr(w, 409, "job is not waiting for source")
		return
	}
	j.syncing = true
	sl, manifest := j.slot, j.Spec.Manifest
	s.mu.Unlock()

	needed, err := sl.plan(manifest)
	if err == nil {
		err = sl.apply(manifest, needed, http.MaxBytesReader(w, r.Body, 8<<30))
	}
	s.mu.Lock()
	g := j.Spec.Git
	s.mu.Unlock()
	if err == nil && g != nil {
		if gerr := s.gitCheckout(sl, g); gerr != nil {
			s.note(j, "running without .git: "+gerr.Error())
		}
	} else if err == nil {
		// No git for this job: don't leave a stale .git describing some other commit.
		_ = os.RemoveAll(filepath.Join(sl.Dir, ".git"))
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	j.syncing = false
	if j.cancel {
		s.finishLocked(j, api.StateCancelled, nil, "cancelled")
		writeErr(w, 409, "job was cancelled during upload")
		return
	}
	if err != nil {
		s.finishLocked(j, api.StateFailed, nil, "source sync: "+err.Error())
		writeErr(w, 400, "source sync: "+err.Error())
		return
	}
	j.State, j.queuedAt = api.StateQueued, time.Now()
	j.Spec.Manifest = nil // synced; no need to keep it on disk
	j.save()
	writeJSON(w, j.Job)
	s.kick()
}

// admit starts queued jobs that fit, oldest first. Small jobs may pass a big one, unless the
// big one has waited over two minutes (then nothing passes it, so it can't starve).
func (s *Server) admit() {
	s.mu.Lock()
	defer s.mu.Unlock()
	var queued []*job
	for _, j := range s.jobs {
		if j.State == api.StateQueued && !j.cancel {
			queued = append(queued, j)
		}
	}
	sort.Slice(queued, func(a, b int) bool { return queued[a].queuedAt.Before(queued[b].queuedAt) })
	for _, j := range queued {
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
	env := s.baseEnv(prof, ncpu)
	env["TMPDIR"] = filepath.Join(j.dir, "tmp")
	env["OFFLOAD_JOB"] = j.ID
	for k, v := range j.Spec.Env {
		env[k] = v
	}
	dir := s.workDir(j)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		s.releaseCPUsLocked(j.cpus)
		return err
	}
	j.Unit = "offload-" + j.ID + ".service"
	if len(j.cpus) > 0 {
		j.AllowedCPUs = units.FormatCPUList(j.cpus)
	}
	argv := append([]string{s.self, "runner", "exec", "--job-dir", j.dir}, artifactArgs(j.Spec.Artifacts)...)
	if dir := s.setupCredentials(j, env); dir != "" {
		argv = append(argv, "--secrets", dir)
	}
	if j.slot != nil && j.Spec.WriteBack {
		argv = append(argv, "--write-back", j.slot.Dir, "--record", j.slot.recordPath())
	}
	argv = append(argv, "--")
	argv = append(argv, j.Spec.Argv...)
	u := unitSpec{
		Name: j.Unit, Slice: s.cfg.Slice, Dir: dir, Env: env, Argv: argv,
		CPUWeight: cpuWeight(j.Resources.CPU), CPUQuotaPct: int(math.Ceil(j.Resources.CPUMax * 100)),
		AllowedCPUs: j.AllowedCPUs, MemoryMax: j.Resources.Mem, OOMScoreAdj: s.cfg.Policy.OOMScoreAdj,
		Stdout: filepath.Join(j.dir, "output.log"),
		Stderr: filepath.Join(j.dir, "stderr.log"),
	}
	if s.features["io"] {
		u.IOWeight = s.cfg.Policy.IOWeight
	}
	if err := startUnit(contextBG(), u); err != nil {
		s.releaseCPUsLocked(j.cpus)
		s.cleanupCredentials(j)
		j.cpus, j.AllowedCPUs, j.Unit = nil, "", ""
		return err
	}
	now := time.Now()
	j.State, j.StartedAt = api.StateRunning, &now
	j.save()
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
		active := err != nil && unitActive(j.Unit)
		s.mu.Lock()
		switch {
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
		j.slot.inUse = ""
		j.slot.LastUsed = now
		if !s.cfg.persistent || j.Spec.Ephemeral {
			s.dropSlotLocked(j.slot)
		}
		j.slot = nil
	}
	j.save()
	s.kick()
}

func (s *Server) dropSlotLocked(sl *slot) {
	_ = os.RemoveAll(sl.Dir)
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
	s.mu.Lock()
	defer s.mu.Unlock()
	j := s.jobs[r.PathValue("id")]
	if j == nil {
		writeErr(w, 404, "no such job")
		return
	}
	switch j.State {
	case api.StateUploading, api.StateQueued:
		if j.syncing {
			j.cancel = true // the upload handler finishes it once the slot is consistent
		} else {
			s.finishLocked(j, api.StateCancelled, nil, "cancelled")
		}
	case api.StateRunning:
		j.cancel = true
		stopUnit(j.Unit)
	}
	writeJSON(w, j.Job)
}

func (s *Server) handleGetJob(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	j := s.jobs[r.PathValue("id")]
	if j == nil {
		writeErr(w, 404, "no such job")
		return
	}
	writeJSON(w, j.Job)
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
