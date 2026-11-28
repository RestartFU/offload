package runner

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"log/slog"
	"math"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/actions/scaleset"
	"github.com/actions/scaleset/listener"

	"github.com/RestartFU/offload/internal/api"
	"github.com/RestartFU/offload/internal/units"
)

// GitHub Actions: for each organization or repository it was added for, a runner keeps its own
// runner scale set on GitHub and tells GitHub how many jobs fit here right now. Scale sets of
// several runners share labels, and GitHub hands each job to one with room. Every job GitHub
// assigns gets a one-job (just-in-time) runner as an ordinary offload job.

//go:embed envs/github.Containerfile
var githubContainerfile string

func githubEnv() *api.EnvDef { return &api.EnvDef{Name: "github", Containerfile: githubContainerfile} }

const (
	githubDefaultMem = 8 << 30
	githubJITEnv     = "ACTIONS_RUNNER_INPUT_JITCONFIG"
	githubRunnerDir  = "/opt/actions-runner"
	githubTick       = 3 * time.Second
)

// githubAPI is the part of GitHub's scale set client the scaler uses (faked in tests).
type githubAPI interface {
	GenerateJitRunnerConfig(ctx context.Context, setting *scaleset.RunnerScaleSetJitRunnerSetting, scaleSetID int) (*scaleset.RunnerScaleSetJitRunnerConfig, error)
	RemoveRunner(ctx context.Context, runnerID int64) error
}

type githubPool struct {
	api.GitHubPool
	key   string
	dir   string
	token string

	gh       githubAPI
	cancel   context.CancelFunc
	done     chan struct{} // closed when its listener goroutine has returned
	slots    map[int]string
	failures int       // runner jobs in a row that ended without taking a GitHub job
	pauseTil time.Time // after repeated failures: accept no jobs until then
	envFail  string    // why the environment build failed, until envRetry
	envRetry time.Time
	listener *listener.Listener
}

// githubPoolRecord is what pool.json keeps; the token lives in its own 0600 file.
type githubPoolRecord struct {
	Owner      string        `json:"owner"`
	URL        string        `json:"url"`
	ScaleSet   string        `json:"scale_set"`
	ScaleSetID int           `json:"scale_set_id,omitempty"`
	Labels     []string      `json:"labels"`
	Max        int           `json:"max,omitempty"`
	Resources  api.Resources `json:"resources"`
	TokenFrom  string        `json:"token_from,omitempty"`
	CreatedAt  time.Time     `json:"created_at"`
	Runs       int           `json:"runs"`
	LastRun    *time.Time    `json:"last_run,omitempty"`
	Removing   bool          `json:"removing,omitempty"`
}

func (s *Server) githubDir() string { return filepath.Join(s.cfg.StateDir, "github") }

func githubScope(key string) string { return "github-" + key }

var scaleSetNameRE = regexp.MustCompile(`[^a-z0-9-]+`)

// githubScaleSetName is this runner's scale set name: unique per runner within an owner.
func githubScaleSetName(runner string) string {
	n := strings.Trim(scaleSetNameRE.ReplaceAllString(strings.ToLower(runner), "-"), "-")
	if n == "" {
		n = "runner"
	}
	return "offload-" + n
}

func (p *githubPool) record() githubPoolRecord {
	return githubPoolRecord{Owner: p.Owner, URL: p.URL, ScaleSet: p.ScaleSet, ScaleSetID: p.ScaleSetID, Labels: p.Labels,
		Max: p.Max, Resources: p.Resources, TokenFrom: p.TokenFrom, CreatedAt: p.CreatedAt, Runs: p.Runs, LastRun: p.LastRun,
		Removing: p.State == api.GitHubRemoving}
}

func (p *githubPool) save() error {
	err := writeFileJSON(filepath.Join(p.dir, "pool.json"), p.record())
	if err != nil {
		log.Printf("github %s: save: %v", p.Owner, err)
	}
	return err
}

func (p *githubPool) slotDir(n int) string { return filepath.Join(p.dir, "slots", strconv.Itoa(n)) }

func (s *Server) restoreGitHub() {
	files, _ := filepath.Glob(filepath.Join(s.githubDir(), "*", "pool.json"))
	for _, f := range files {
		var rec githubPoolRecord
		b, err := os.ReadFile(f)
		if err != nil || json.Unmarshal(b, &rec) != nil {
			continue
		}
		dir := filepath.Dir(f)
		key := filepath.Base(dir)
		if !validKey(key) || api.GitHubKey(rec.Owner) != key {
			continue
		}
		tok, _ := os.ReadFile(filepath.Join(dir, "token"))
		p := s.newPool(key, dir, rec, strings.TrimSpace(string(tok)))
		if rec.Removing {
			p.State = api.GitHubRemoving
		}
		s.github[key] = p
	}
	for _, j := range s.jobs {
		if p := s.github[j.Spec.GitHub]; p != nil && !api.Terminal(j.State) && j.GitHub != nil {
			if n, ok := githubSlotOf(j); ok {
				p.slots[n] = j.ID
			}
		}
	}
}

func (s *Server) newPool(key, dir string, rec githubPoolRecord, token string) *githubPool {
	return &githubPool{key: key, dir: dir, token: token, slots: map[int]string{}, GitHubPool: api.GitHubPool{
		Owner: rec.Owner, URL: rec.URL, ScaleSet: rec.ScaleSet, ScaleSetID: rec.ScaleSetID, Labels: rec.Labels, Max: rec.Max,
		Resources: rec.Resources, TokenFrom: rec.TokenFrom, CreatedAt: rec.CreatedAt, Runs: rec.Runs, LastRun: rec.LastRun,
		State: api.GitHubPreparing, Detail: "connecting to GitHub",
	}}
}

// startGitHub starts the listeners of restored pools (and finishes removals a restart interrupted).
func (s *Server) startGitHub(ctx context.Context) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, p := range s.github {
		if p.State == api.GitHubRemoving {
			go s.finishRemoval(p)
			continue
		}
		s.startPoolLocked(ctx, p)
	}
}

func (s *Server) startPoolLocked(ctx context.Context, p *githubPool) {
	pctx, cancel := context.WithCancel(ctx)
	p.cancel, p.done = cancel, make(chan struct{})
	go func() {
		defer close(p.done)
		s.runPool(pctx, p)
	}()
}

// runPool keeps a pool connected to GitHub: it reconnects after errors, with a growing delay.
func (s *Server) runPool(ctx context.Context, p *githubPool) {
	delay := 5 * time.Second
	for ctx.Err() == nil {
		started := time.Now()
		err := s.listenPool(ctx, p)
		if ctx.Err() != nil {
			return
		}
		if time.Since(started) > 10*time.Minute {
			delay = 5 * time.Second
		}
		s.mu.Lock()
		p.State, p.Detail, p.listener = api.GitHubFailed, githubProblem(err, p.Owner), nil
		s.mu.Unlock()
		log.Printf("github %s: %v (retrying in %s)", p.Owner, err, delay)
		select {
		case <-ctx.Done():
			return
		case <-time.After(delay):
		}
		delay = min(delay*2, 5*time.Minute)
	}
}

// githubProblem turns a GitHub client error into what to do about it.
func githubProblem(err error, owner string) string {
	msg := err.Error()
	switch {
	case errors.Is(err, scaleset.UnauthorizedError) || strings.Contains(msg, "401"):
		return "GitHub rejected the token: add the runner again with a fresh one (offload github add " + owner + ")"
	case errors.Is(err, scaleset.NotFoundError) || strings.Contains(msg, "403") || strings.Contains(msg, "404"):
		return "the token can't manage self-hosted runners of " + owner + ": a gh login needs admin:org (gh auth refresh -s admin:org); a fine-grained token needs Self-hosted runners: write"
	case errors.Is(err, scaleset.ConflictError):
		return "another listener holds this scale set (an older runner process, or a runner with the same name); retrying"
	}
	return msg
}

func (s *Server) githubClient(p *githubPool) (*scaleset.Client, error) {
	return scaleset.NewClientWithPersonalAccessToken(scaleset.NewClientWithPersonalAccessTokenConfig{
		GitHubConfigURL: p.URL, PersonalAccessToken: p.token,
		SystemInfo: scaleset.SystemInfo{System: "offload", Version: s.version},
	})
}

// ensureScaleSet finds or creates this runner's scale set for the pool, with the pool's labels.
func ensureScaleSet(ctx context.Context, c *scaleset.Client, name string, labels []string) (*scaleset.RunnerScaleSet, error) {
	want := make([]scaleset.Label, len(labels))
	for i, l := range labels {
		want[i] = scaleset.Label{Name: l}
	}
	ss, err := c.GetRunnerScaleSet(ctx, 1, name)
	if err != nil {
		return nil, err
	}
	if ss == nil {
		return c.CreateRunnerScaleSet(ctx, &scaleset.RunnerScaleSet{Name: name, RunnerGroupID: 1, Labels: want,
			RunnerSetting: scaleset.RunnerSetting{DisableUpdate: true}})
	}
	have := map[string]bool{}
	for _, l := range ss.Labels {
		have[l.Name] = true
	}
	same := len(ss.Labels) == len(want)
	for _, l := range want {
		same = same && have[l.Name]
	}
	if same {
		return ss, nil
	}
	ss.Labels = want
	return c.UpdateRunnerScaleSet(ctx, ss.ID, ss)
}

func (s *Server) listenPool(ctx context.Context, p *githubPool) error {
	s.mu.Lock()
	p.State, p.Detail = api.GitHubPreparing, "connecting to GitHub"
	s.mu.Unlock()
	c, err := s.githubClient(p)
	if err != nil {
		return err
	}
	ss, err := ensureScaleSet(ctx, c, p.ScaleSet, p.Labels)
	if err != nil {
		return err
	}
	s.mu.Lock()
	p.ScaleSetID, p.gh = ss.ID, c
	p.save()
	p.Detail = "installing the GitHub Actions runner"
	s.mu.Unlock()
	if _, err := s.actionsRunner(ctx, p.token); err != nil {
		return fmt.Errorf("installing the GitHub Actions runner: %w", err)
	}
	session, err := c.MessageSessionClient(ctx, ss.ID, s.cfg.Name)
	if err != nil {
		return err
	}
	defer session.Close(context.WithoutCancel(ctx))
	s.mu.Lock()
	capNow := p.Capacity // tickGitHub's: 0 until the environment is built
	s.mu.Unlock()
	l, err := listener.New(session, listener.Config{ScaleSetID: ss.ID, MaxRunners: capNow,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		return err
	}
	s.mu.Lock()
	p.listener = l
	s.mu.Unlock()
	err = l.Run(ctx, &githubScaler{s: s, p: p})
	s.mu.Lock()
	if p.listener == l {
		p.listener = nil
	}
	s.mu.Unlock()
	return err
}

// tickGitHub refreshes each pool's capacity (what its listener offers GitHub on the next poll)
// and keeps its environment image built.
func (s *Server) tickGitHub() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, p := range s.github {
		if p.State == api.GitHubRemoving {
			continue
		}
		p.Running, p.Starting = 0, 0
		for _, j := range s.githubJobsLocked(p) {
			if j.GitHub != nil && j.GitHub.Job != "" {
				p.Running++
			} else {
				p.Starting++
			}
		}
		ready := false
		if time.Now().After(p.envRetry) {
			var err error
			if _, ready, err = s.sessionImageLocked(githubEnv()); err != nil {
				p.envFail, p.envRetry = err.Error(), time.Now().Add(5*time.Minute)
			} else {
				p.envFail = ""
			}
		}
		capNow := s.githubCapacityLocked(p)
		switch {
		case p.envFail != "":
			p.State, p.Detail, capNow = api.GitHubFailed, "building the GitHub runner environment failed (retrying in a few minutes): "+p.envFail, p.Running+p.Starting
		case !ready:
			if p.listener != nil {
				p.State, p.Detail = api.GitHubPreparing, "building the GitHub runner environment (first time on this runner, a few minutes)"
			}
			capNow = p.Running + p.Starting
		case p.listener != nil && time.Now().Before(p.pauseTil):
			p.State, p.Detail = api.GitHubFailed, fmt.Sprintf("runners keep failing to start; paused until %s", p.pauseTil.Format(time.TimeOnly))
			capNow = p.Running + p.Starting
		case p.listener != nil && s.paused:
			p.State, p.Detail = api.GitHubPreparing, "this runner is paused: it takes no new GitHub jobs until it is resumed"
		case p.listener != nil:
			p.State, p.Detail = api.GitHubListening, ""
		}
		p.Capacity = capNow
		if p.listener != nil {
			p.listener.SetMaxRunners(capNow)
		}
	}
}

func (s *Server) githubJobsLocked(p *githubPool) []*job {
	var out []*job
	for _, j := range s.jobs {
		if j.Spec.GitHub == p.key && !api.Terminal(j.State) {
			out = append(out, j)
		}
	}
	return out
}

// githubSpec is the job spec a pool's runners get (also what memory history is keyed on).
func githubSpec(p *githubPool) api.JobSpec {
	return api.JobSpec{Argv: []string{githubRunnerDir + "/run.sh"}, Resources: p.Resources, Runtime: "container",
		Environment: githubEnv(), Detached: true, Client: "github " + p.Owner, GitHub: p.key}
}

// githubCapacityLocked is how many GitHub jobs the pool can have at once right now: its runners,
// plus as many more of its size as would be admitted now.
func (s *Server) githubCapacityLocked(p *githubPool) int {
	active := len(s.githubJobsLocked(p))
	if s.retireErr != "" || s.upgrading || s.paused {
		return active
	}
	a := s.allocatedLocked()
	var pend api.Resources
	for _, j := range s.jobs {
		if (j.State == api.StateQueued || j.State == api.StateUploading) && !j.StopRequested {
			pend.CPU += j.Resources.CPU
			pend.Mem += j.booked()
		}
	}
	cpu := s.cfg.Capacity.CPU - a.CPU - pend.CPU
	if idle, ok := s.cpuIdleLocked(); ok && s.cfg.Policy.CPUOvercommit > 1 {
		cpu = max(cpu, min(s.cfg.Capacity.CPU*s.cfg.Policy.CPUOvercommit-a.CPU-pend.CPU, idle-pend.CPU))
	}
	mem := s.memRoomLocked() - pend.Mem
	book := s.bookMemLocked(githubSpec(p), p.Resources.Mem)
	more := 0
	if p.Resources.CPU > 0 && book > 0 && cpu > 0 && mem > 0 {
		more = int(math.Floor(min(cpu/p.Resources.CPU, float64(mem)/float64(book))))
	}
	n := active + max(more, 0)
	if p.Max > 0 {
		n = min(n, max(p.Max, active))
	}
	return n
}

type githubScaler struct {
	s *Server
	p *githubPool
}

// Scale handles one message from GitHub: it records which runner took which job, and starts or
// drops runners so there is one for every job GitHub assigned to this scale set.
func (sc *githubScaler) Scale(ctx context.Context, m *scaleset.RunnerScaleSetMessage) error {
	if m == nil {
		return nil
	}
	s, p := sc.s, sc.p
	s.mu.Lock()
	for _, js := range m.JobStartedMessages {
		if j := s.githubJobByRunnerLocked(p, js.RunnerName); j != nil {
			g := *j.GitHub
			g.Repo, g.Job, g.RunID, g.Event = js.OwnerName+"/"+js.RepositoryName, js.JobDisplayName, js.WorkflowRunID, js.EventName
			g.Workflow = githubWorkflowFile(js.JobWorkflowRef)
			if js.WorkflowRunID != 0 {
				g.URL = fmt.Sprintf("https://github.com/%s/%s/actions/runs/%d", js.OwnerName, js.RepositoryName, js.WorkflowRunID)
			}
			j.GitHub = &g
			p.failures = 0
			j.save()
		}
	}
	for _, jc := range m.JobCompletedMessages {
		if jc.RunnerName == "" {
			continue // assigned, then cancelled before a runner took it
		}
		if j := s.githubJobByRunnerLocked(p, jc.RunnerName); j != nil {
			g := *j.GitHub
			g.Result = jc.Result
			j.GitHub = &g
			j.save()
		}
		now := time.Now()
		p.Runs, p.LastRun = p.Runs+1, &now
		p.save()
	}
	need, drop := 0, []*job(nil)
	if st := m.Statistics; st != nil {
		p.Assigned = st.TotalAssignedJobs
		live := s.githubJobsLocked(p)
		need = st.TotalAssignedJobs - len(live)
		if need < 0 {
			for _, j := range live {
				if len(drop) < -need && (j.GitHub == nil || j.GitHub.Job == "") {
					drop = append(drop, j)
				}
			}
		}
		if time.Now().Before(p.pauseTil) {
			need = 0
		}
	}
	for _, j := range drop {
		s.cancelJobLocked(j, "GitHub no longer has a job for this runner")
	}
	s.mu.Unlock()
	for _, j := range drop {
		if j.GitHub != nil && j.GitHub.RunnerID != 0 {
			_ = p.gh.RemoveRunner(ctx, int64(j.GitHub.RunnerID))
		}
	}
	for range max(need, 0) {
		if err := s.startGitHubRunner(ctx, p); err != nil {
			return err
		}
	}
	return nil
}

func githubWorkflowFile(ref string) string {
	ref, _, _ = strings.Cut(ref, "@")
	return filepath.Base(ref)
}

func (s *Server) githubJobByRunnerLocked(p *githubPool, runner string) *job {
	for _, j := range s.jobs {
		if j.Spec.GitHub == p.key && j.GitHub != nil && j.GitHub.Runner == runner {
			return j
		}
	}
	return nil
}

// startGitHubRunner queues a one-job runner. Its config is a credential: it lives only in the
// job's secrets (memory, then tmpfs), never in the job record.
func (s *Server) startGitHubRunner(ctx context.Context, p *githubPool) error {
	s.mu.Lock()
	slot := 0
	for p.slots[slot] != "" {
		slot++
	}
	work := filepath.Join(p.slotDir(slot), "work")
	p.slots[slot] = "pending"
	s.mu.Unlock()
	release := func() {
		s.mu.Lock()
		if p.slots[slot] == "pending" {
			delete(p.slots, slot)
		}
		s.mu.Unlock()
	}
	for _, d := range []string{work, filepath.Join(p.slotDir(slot), "containers"), filepath.Join(p.slotDir(slot), "rustup")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			release()
			return err
		}
	}
	name := fmt.Sprintf("%s-%s", p.ScaleSet, strings.TrimPrefix(newID(""), "-"))
	jit, err := p.gh.GenerateJitRunnerConfig(ctx, &scaleset.RunnerScaleSetJitRunnerSetting{Name: name, WorkFolder: work}, p.ScaleSetID)
	if err != nil {
		release()
		return fmt.Errorf("creating a runner on GitHub: %w", err)
	}
	install, err := s.actionsRunner(ctx, p.token)
	if err != nil {
		release()
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	spec := githubSpec(p)
	now := time.Now()
	j := &job{Job: api.Job{
		ID: newID("j-"), Runner: s.cfg.Name, State: api.StateQueued, Spec: spec, Caller: "github",
		Resources: p.Resources, CreatedAt: now, Runtime: "container", Proto: api.Proto, MemFrom: "requested",
		GitHub: &api.GitHubJob{Owner: p.Owner, Runner: name},
	}, queuedAt: now}
	if jit.Runner != nil {
		j.GitHub.RunnerID = jit.Runner.ID
	}
	j.Image, _, _ = envPlan(spec.Environment)
	j.dir = s.jobDir(j.ID)
	j.secrets = map[string]string{githubJITEnv: jit.EncodedJITConfig}
	j.Spec.SecretNames = []string{githubJITEnv}
	j.Spec.Env = map[string]string{"OFFLOAD_GITHUB_SLOT": strconv.Itoa(slot), "OFFLOAD_GITHUB_INSTALL": install}
	j.MemBooked = s.bookMemLocked(j.Spec, j.Resources.Mem)
	fail := func(err error) error {
		_ = os.RemoveAll(j.dir)
		delete(p.slots, slot)
		return err
	}
	if err := os.MkdirAll(filepath.Join(j.dir, "tmp"), 0o700); err != nil {
		return fail(err)
	}
	for _, f := range []string{"output.log", "stderr.log"} {
		if err := os.WriteFile(filepath.Join(j.dir, f), nil, 0o600); err != nil {
			return fail(err)
		}
	}
	if err := j.save(); err != nil {
		return fail(err)
	}
	s.jobs[j.ID] = j
	p.slots[slot] = j.ID
	marker := s.cacheDir("scoped", githubScope(p.key), ".used")
	_ = os.MkdirAll(filepath.Dir(marker), 0o755)
	_ = os.WriteFile(marker, nil, 0o644)
	s.kick()
	return nil
}

func githubSlotOf(j *job) (int, bool) {
	n, err := strconv.Atoi(j.Spec.Env["OFFLOAD_GITHUB_SLOT"])
	return n, err == nil && n >= 0
}

// githubFinishedLocked frees a finished runner job's slot. A runner that never took a GitHub job
// counts as a failure; several in a row pause the pool so a broken setup doesn't spin.
func (s *Server) githubFinishedLocked(j *job) {
	p := s.github[j.Spec.GitHub]
	if p == nil {
		return
	}
	if n, ok := githubSlotOf(j); ok && p.slots[n] == j.ID {
		delete(p.slots, n)
	}
	if j.GitHub != nil && j.GitHub.Job == "" && j.State != api.StateCancelled {
		if p.gh != nil && j.GitHub.RunnerID != 0 {
			go func(gh githubAPI, id int64) { _ = gh.RemoveRunner(context.Background(), id) }(p.gh, int64(j.GitHub.RunnerID))
		}
		p.failures++
		if p.failures >= 3 {
			p.pauseTil = time.Now().Add(min(time.Minute<<min(p.failures-3, 4), 15*time.Minute))
			log.Printf("github %s: %d runners in a row ended without a job (last: %s, %s); pausing", p.Owner, p.failures, j.ID, j.Reason)
		}
	}
}

func (s *Server) cancelJobLocked(j *job, reason string) {
	j.cancelled.Store(true)
	j.StopRequested = true
	_ = j.save()
	switch j.State {
	case api.StateQueued, api.StateUploading:
		s.finishLocked(j, api.StateCancelled, nil, reason)
	case api.StateRunning:
		j.cancel = true
		stopUnit(j.Unit)
	}
}

var actionsRunnerMu sync.Mutex

// actionsRunner installs the latest GitHub Actions runner into the runner's cache (once a day at
// most it checks for a newer one) and returns its directory. Jobs see it read-only, as an overlay.
func (s *Server) actionsRunner(ctx context.Context, token string) (string, error) {
	actionsRunnerMu.Lock()
	defer actionsRunnerMu.Unlock()
	base := s.cacheDir("actions-runner")
	stamp := filepath.Join(base, "checked")
	current := ""
	if b, err := os.ReadFile(filepath.Join(base, "current")); err == nil {
		current = strings.TrimSpace(string(b))
	}
	if fi, err := os.Stat(stamp); current != "" && err == nil && time.Since(fi.ModTime()) < 24*time.Hour {
		return filepath.Join(base, current), nil
	}
	ver, err := latestActionsRunner(ctx, token)
	if err != nil {
		if current != "" {
			return filepath.Join(base, current), nil
		}
		return "", err
	}
	dir := filepath.Join(base, ver)
	if _, err := os.Stat(filepath.Join(dir, "run.sh")); err != nil {
		if err := downloadActionsRunner(ctx, ver, dir); err != nil {
			if current != "" {
				return filepath.Join(base, current), nil
			}
			return "", err
		}
		log.Printf("github: installed GitHub Actions runner %s", ver)
	}
	_ = os.WriteFile(filepath.Join(base, "current"), []byte(ver), 0o644)
	_ = os.WriteFile(stamp, nil, 0o644)
	return dir, nil
}

var runnerVersionRE = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+$`)

func latestActionsRunner(ctx context.Context, token string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", "https://api.github.com/repos/actions/runner/releases/latest", nil)
	req.Header.Set("Accept", "application/vnd.github+json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	var rel struct {
		Tag string `json:"tag_name"`
	}
	if resp.StatusCode != 200 || json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&rel) != nil {
		return "", fmt.Errorf("GitHub answered %s for the latest actions runner release", resp.Status)
	}
	v := strings.TrimPrefix(rel.Tag, "v")
	if !runnerVersionRE.MatchString(v) {
		return "", fmt.Errorf("unexpected actions runner version %q", rel.Tag)
	}
	return v, nil
}

func downloadActionsRunner(ctx context.Context, ver, dir string) error {
	arch := map[string]string{"amd64": "x64", "arm64": "arm64", "arm": "arm"}[runtime.GOARCH]
	if arch == "" {
		return fmt.Errorf("GitHub publishes no actions runner for %s", runtime.GOARCH)
	}
	tmp, err := os.MkdirTemp(filepath.Dir(dir), ".download-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	u := fmt.Sprintf("https://github.com/actions/runner/releases/download/v%s/actions-runner-linux-%s-%s.tar.gz", ver, arch, ver)
	req, _ := http.NewRequestWithContext(ctx, "GET", u, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("downloading %s: %s", u, resp.Status)
	}
	tree := filepath.Join(tmp, "tree")
	if err := os.Mkdir(tree, 0o755); err != nil {
		return err
	}
	tar := exec.CommandContext(ctx, "tar", "-xz", "--no-same-owner", "-C", tree)
	tar.Stdin = resp.Body
	if out, err := tar.CombinedOutput(); err != nil {
		return fmt.Errorf("unpacking the actions runner: %v %s", err, strings.TrimSpace(string(out)))
	}
	return os.Rename(tree, dir)
}

func (s *Server) githubViewLocked(p *githubPool) api.GitHubPool {
	v := p.GitHubPool
	v.Labels = append([]string(nil), p.Labels...)
	return v
}

func (s *Server) handleGitHubList(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	out := []api.GitHubPool{}
	for _, p := range s.github {
		out = append(out, s.githubViewLocked(p))
	}
	s.mu.Unlock()
	sort.Slice(out, func(a, b int) bool { return strings.ToLower(out[a].Owner) < strings.ToLower(out[b].Owner) })
	writeJSON(w, out)
}

// handleGitHubAdd starts (or updates) taking an owner's GitHub jobs. It checks the token and
// creates the scale set on GitHub before answering, so a wrong token or scope fails right away.
func (s *Server) handleGitHubAdd(w http.ResponseWriter, r *http.Request) {
	var req api.GitHubAdd
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&req); err != nil {
		writeErr(w, 400, "bad request: "+err.Error())
		return
	}
	owner, canonical, err := api.GitHubOwner(req.URL)
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	if req.Token == "" || strings.ContainsAny(req.Token, " \t\r\n") {
		writeErr(w, 400, "a GitHub token is required (offload github add uses your gh login by default)")
		return
	}
	if len(req.Labels) == 0 {
		req.Labels = []string{"offload"}
	}
	for _, l := range req.Labels {
		if !validKey(l) {
			writeErr(w, 400, "invalid label "+strconv.Quote(l)+": use letters, digits, '.', '_' and '-'")
			return
		}
	}
	if req.Max < 0 {
		writeErr(w, 400, "--max can't be negative")
		return
	}
	if !s.containersAvailable() {
		writeErr(w, 400, "GitHub jobs need containers, and this runner has no podman")
		return
	}
	res := req.Resources
	if res.CPUMax > 0 && res.CPU > res.CPUMax {
		writeErr(w, 400, fmt.Sprintf("--cpu %.1f is more than --cpu-max %.1f", res.CPU, res.CPUMax))
		return
	}
	if res.CPU > s.cfg.Capacity.CPU+1e-9 {
		writeErr(w, 400, fmt.Sprintf("asks for %.1f cpu per job; this runner guarantees at most %.1f", res.CPU, s.cfg.Capacity.CPU))
		return
	}
	if res.Mem > s.cfg.memCap {
		writeErr(w, 400, fmt.Sprintf("asks for %s per job; this runner hands out at most %s", units.FormatSize(res.Mem), units.FormatSize(s.cfg.memCap)))
		return
	}
	if res.Mem == 0 {
		res.Mem = min(githubDefaultMem, s.cfg.memCap)
	}
	res = s.cfg.effective(res, nil)
	key := api.GitHubKey(owner)
	if !validKey(key) {
		writeErr(w, 400, "can't use "+owner)
		return
	}
	name := githubScaleSetName(s.cfg.Name)
	probe := &githubPool{GitHubPool: api.GitHubPool{URL: canonical, Owner: owner}, token: req.Token}
	c, err := s.githubClient(probe)
	if err == nil {
		ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
		_, err = ensureScaleSet(ctx, c, name, req.Labels)
		cancel()
	}
	if err != nil {
		writeErr(w, http.StatusBadGateway, githubProblem(err, owner))
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.serveCtx == nil {
		writeErr(w, 503, "runner is starting; try again in a few seconds")
		return
	}
	p := s.github[key]
	if p != nil && p.State == api.GitHubRemoving {
		writeErr(w, http.StatusConflict, owner+" is still being removed here; try again in a minute")
		return
	}
	dir := filepath.Join(s.githubDir(), key)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if err := writeSecretFile(filepath.Join(dir, "token"), req.Token); err != nil {
		writeErr(w, 500, "storing the token: "+err.Error())
		return
	}
	restart := p != nil
	if p == nil {
		p = s.newPool(key, dir, githubPoolRecord{Owner: owner, URL: canonical, ScaleSet: name, CreatedAt: time.Now()}, req.Token)
		s.github[key] = p
	}
	p.token, p.Labels, p.Max, p.Resources, p.TokenFrom = req.Token, req.Labels, req.Max, res, req.TokenFrom
	p.State, p.Detail = api.GitHubPreparing, "connecting to GitHub"
	if err := p.save(); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if restart && p.cancel != nil {
		// Reconnect with the new settings once the old listener has let go of its session.
		cancel, done := p.cancel, p.done
		p.cancel = nil
		go func() {
			cancel()
			<-done
			s.mu.Lock()
			defer s.mu.Unlock()
			if p.State != api.GitHubRemoving && p.cancel == nil {
				s.startPoolLocked(s.serveCtx, p)
			}
		}()
	} else {
		s.startPoolLocked(s.serveCtx, p)
	}
	log.Printf("github %s: taking jobs labeled %s as scale set %s", owner, strings.Join(p.Labels, ","), name)
	writeJSON(w, s.githubViewLocked(p))
}

func writeSecretFile(path, value string) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(value+"\n"), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// handleGitHubRemove stops taking an owner's jobs here: it stops the runners, deletes this
// runner's scale set on GitHub, then the pool's files.
func (s *Server) handleGitHubRemove(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p := s.github[r.PathValue("key")]
	if p == nil {
		writeErr(w, 404, "this runner doesn't take GitHub jobs for that owner")
		return
	}
	if p.State != api.GitHubRemoving {
		p.State, p.Detail = api.GitHubRemoving, "stopping its runners"
		if err := p.save(); err != nil {
			writeErr(w, 500, "can't record the removal: "+err.Error())
			return
		}
		for _, j := range s.githubJobsLocked(p) {
			s.cancelJobLocked(j, "GitHub runners removed for "+p.Owner)
		}
		go s.finishRemoval(p)
	}
	writeJSON(w, s.githubViewLocked(p))
}

func (s *Server) finishRemoval(p *githubPool) {
	s.mu.Lock()
	cancel, done := p.cancel, p.done
	s.mu.Unlock()
	if cancel != nil {
		cancel()
		<-done
	}
	for {
		s.mu.Lock()
		left := len(s.githubJobsLocked(p))
		s.mu.Unlock()
		if left == 0 {
			break
		}
		time.Sleep(time.Second)
	}
	problem := ""
	if c, err := s.githubClient(p); err == nil && p.ScaleSetID != 0 {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		if err := c.DeleteRunnerScaleSet(ctx, p.ScaleSetID); err != nil && !errors.Is(err, scaleset.NotFoundError) {
			problem = err.Error()
		}
		cancel()
	}
	if problem != "" {
		log.Printf("github %s: deleting scale set %s on GitHub: %s (delete it under Settings → Actions → Runners)", p.Owner, p.ScaleSet, problem)
	}
	err := removeAll(p.dir)
	if err == nil {
		err = removeAll(s.cacheDir("scoped", githubScope(p.key)))
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err != nil {
		p.Detail = "couldn't delete its files: " + err.Error()
		log.Printf("github %s: %s", p.Owner, p.Detail)
		return
	}
	delete(s.github, p.key)
	log.Printf("github %s: removed", p.Owner)
}
