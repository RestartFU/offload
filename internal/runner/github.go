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

	gh         githubAPI
	cancel     context.CancelFunc
	done       chan struct{} // closed when its listener goroutine has returned
	restarting bool          // a restart waits for the old listener to let go of its session
	forget     bool          // remove without deleting the scale set on GitHub (rm --forget)
	slots      map[int]string
	failures   int       // runner jobs in a row that ended without taking a GitHub job
	pauseTil   time.Time // after repeated failures: accept no jobs until then
	envFail    string    // why the environment build failed, until envRetry
	envRetry   time.Time
	listener   *listener.Listener
}

// githubPoolRecord is what pool.json keeps (0600, in a 0700 directory), the token included: one
// atomic write keeps settings and token together.
type githubPoolRecord struct {
	Owner      string        `json:"owner"`
	URL        string        `json:"url"`
	ScaleSet   string        `json:"scale_set"`
	ScaleSetID int           `json:"scale_set_id,omitempty"`
	Labels     []string      `json:"labels"`
	Max        int           `json:"max,omitempty"`
	Resources  api.Resources `json:"resources"`
	TokenFrom  string        `json:"token_from,omitempty"`
	Shared     bool          `json:"shared,omitempty"`
	CreatedAt  time.Time     `json:"created_at"`
	Runs       int           `json:"runs"`
	LastRun    *time.Time    `json:"last_run,omitempty"`
	Removing   bool          `json:"removing,omitempty"`
	Forget     bool          `json:"forget,omitempty"`
	Token      string        `json:"token"`
}

func (s *Server) githubDir() string { return filepath.Join(s.cfg.StateDir, "github") }

// githubScope names a pool's dependency caches (cache/github/<key>), apart from the caches of
// clients' workspaces, whose keys clients choose.
func githubScope(key string) string { return "github:" + key }

var scaleSetNameRE = regexp.MustCompile(`[^a-z0-9-]+`)

// githubScaleSetName is this runner's scale set name, unique per runner within an owner: two
// runners of the same name must never share one (removing either would delete it).
func githubScaleSetName(runner, id string) string {
	n := strings.Trim(scaleSetNameRE.ReplaceAllString(strings.ToLower(runner), "-"), "-")
	if n == "" {
		n = "runner"
	}
	if len(id) > 6 {
		id = id[:6]
	}
	return "offload-" + n + "-" + id
}

func (p *githubPool) record() githubPoolRecord {
	return githubPoolRecord{Owner: p.Owner, URL: p.URL, ScaleSet: p.ScaleSet, ScaleSetID: p.ScaleSetID, Labels: p.Labels,
		Max: p.Max, Resources: p.Resources, TokenFrom: p.TokenFrom, Shared: p.Shared, CreatedAt: p.CreatedAt, Runs: p.Runs,
		LastRun: p.LastRun, Removing: p.State == api.GitHubRemoving, Forget: p.forget, Token: p.token}
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
		p := s.newPool(key, dir, rec, rec.Token)
		if rec.Removing {
			p.State, p.forget = api.GitHubRemoving, rec.Forget
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
		Resources: rec.Resources, TokenFrom: rec.TokenFrom, Shared: rec.Shared, CreatedAt: rec.CreatedAt, Runs: rec.Runs, LastRun: rec.LastRun,
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
	done := make(chan struct{})
	p.cancel, p.done = cancel, done
	go func() {
		defer close(done)
		s.runPool(pctx, p, done)
	}()
}

// restartPoolLocked reconnects a pool with new settings once its listener has let go of its
// session (GitHub allows one per scale set). One restart at a time; a pending one uses the newest
// settings.
func (s *Server) restartPoolLocked(p *githubPool) {
	if p.restarting {
		return
	}
	if p.cancel == nil {
		s.startPoolLocked(s.serveCtx, p)
		return
	}
	cancel, done := p.cancel, p.done
	p.cancel, p.restarting = nil, true
	go func() {
		cancel()
		<-done
		s.mu.Lock()
		defer s.mu.Unlock()
		p.restarting = false
		if p.State != api.GitHubRemoving {
			s.startPoolLocked(s.serveCtx, p)
		}
	}()
}

// runPool keeps a pool connected to GitHub: it reconnects after errors, with a growing delay.
// done identifies this listener generation: a replaced or removed one never publishes state.
func (s *Server) runPool(ctx context.Context, p *githubPool, done chan struct{}) {
	delay := 5 * time.Second
	for ctx.Err() == nil {
		started := time.Now()
		err := s.listenPool(ctx, p, done)
		s.mu.Lock()
		current := p.done == done && p.State != api.GitHubRemoving
		if current && ctx.Err() == nil {
			p.State, p.Detail = api.GitHubFailed, githubProblem(err, p.Owner)
		}
		s.mu.Unlock()
		if !current || ctx.Err() != nil {
			return
		}
		if time.Since(started) > 10*time.Minute {
			delay = 5 * time.Second
		}
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

func (s *Server) githubClient(url, token string) (*scaleset.Client, error) {
	return scaleset.NewClientWithPersonalAccessToken(scaleset.NewClientWithPersonalAccessTokenConfig{
		GitHubConfigURL: url, PersonalAccessToken: token,
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

// listenPool runs one connection to GitHub with the settings the pool had when it started.
func (s *Server) listenPool(ctx context.Context, p *githubPool, done chan struct{}) error {
	s.mu.Lock()
	if p.done != done || p.State == api.GitHubRemoving {
		s.mu.Unlock()
		return errors.New("replaced")
	}
	p.State, p.Detail = api.GitHubPreparing, "connecting to GitHub"
	url, token, name, labels := p.URL, p.token, p.ScaleSet, append([]string(nil), p.Labels...)
	s.mu.Unlock()
	c, err := s.githubClient(url, token)
	if err != nil {
		return err
	}
	ss, err := ensureScaleSet(ctx, c, name, labels)
	if err != nil {
		return err
	}
	s.mu.Lock()
	if p.done == done {
		p.ScaleSetID, p.gh = ss.ID, c
		p.save()
		p.Detail = "installing the GitHub Actions runner"
	}
	s.mu.Unlock()
	if _, err := s.actionsRunner(ctx, token); err != nil {
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
	if p.done != done {
		s.mu.Unlock()
		return errors.New("replaced")
	}
	p.listener = l
	s.mu.Unlock()
	err = l.Run(ctx, &githubScaler{s: s, p: p, session: session})
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
// plus as many more of its size as would be admitted now, each budget in its own unit (a job
// books its learned peak but counts its whole limit against mem_overcommit).
func (s *Server) githubCapacityLocked(p *githubPool) int {
	active := len(s.githubJobsLocked(p))
	if s.retireErr != "" || s.upgrading || s.paused {
		return active
	}
	a := s.allocatedLocked()
	var pend api.Resources
	var pendLimits int64
	for _, j := range s.jobs {
		if (j.State == api.StateQueued || j.State == api.StateUploading) && !j.StopRequested {
			pend.CPU += j.Resources.CPU
			pend.Mem += j.booked()
			pendLimits += j.Resources.Mem
		}
	}
	cpu := s.cfg.Capacity.CPU - a.CPU - pend.CPU
	if idle, ok := s.cpuIdleLocked(); ok && s.cfg.Policy.CPUOvercommit > 1 {
		cpu = max(cpu, min(s.cfg.Capacity.CPU*s.cfg.Policy.CPUOvercommit-a.CPU-pend.CPU, idle-pend.CPU))
	}
	book := float64(s.bookMemLocked(githubSpec(p), p.Resources.Mem))
	per := func(room, each float64) float64 {
		if each <= 0 {
			return math.Inf(1)
		}
		return room / each
	}
	more := min(
		per(cpu, p.Resources.CPU),
		per(float64(s.cfg.memCap-a.Mem-pend.Mem), book),
		per(float64(s.cfg.memCap)*s.cfg.Policy.MemOvercommit-float64(s.memLimitsLocked()+pendLimits), float64(p.Resources.Mem)),
		per(float64(memAvailable()-s.memGrowthLocked()-s.cfg.memReserve-pend.Mem), book),
	)
	n := active
	if more >= 1 && !math.IsInf(more, 1) {
		n += int(math.Floor(more))
	}
	if p.Max > 0 {
		n = min(n, max(p.Max, active))
	}
	return n
}

type githubScaler struct {
	s       *Server
	p       *githubPool
	session interface {
		AcquireJobs(ctx context.Context, requestIDs []int64) ([]int64, error)
	}
}

// Scale handles one poll of GitHub's message queue (m is nil when nothing happened): it claims
// the jobs GitHub offers, records which runner took which job, and starts or drops runners so
// there is one for every job GitHub assigned to this scale set.
func (sc *githubScaler) Scale(ctx context.Context, m *scaleset.RunnerScaleSetMessage) error {
	s, p := sc.s, sc.p
	if m != nil && len(m.JobAvailableMessages) > 0 {
		// An offer can answer a poll made before a pause or before other work took the room:
		// claim only what fits now. GitHub offers the rest to other scale sets.
		s.mu.Lock()
		assigned := p.Assigned
		if m.Statistics != nil { // this poll's count, not the previous one's
			assigned = m.Statistics.TotalAssignedJobs
		}
		room := s.githubCapacityLocked(p) - max(assigned, len(s.githubJobsLocked(p)))
		if time.Now().Before(p.pauseTil) || p.State == api.GitHubRemoving {
			room = 0
		}
		s.mu.Unlock()
		var ids []int64
		for _, ja := range m.JobAvailableMessages {
			if len(ids) < room {
				ids = append(ids, ja.RunnerRequestID)
			}
		}
		// Left unacknowledged on failure (the listener reconnects), so GitHub offers them again.
		if len(ids) > 0 {
			if _, err := sc.session.AcquireJobs(ctx, ids); err != nil {
				return fmt.Errorf("claiming jobs GitHub offered: %w", err)
			}
		}
	}
	s.mu.Lock()
	fresh := false
	if m != nil {
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
			j := s.githubJobByRunnerLocked(p, jc.RunnerName)
			if jc.RunnerName == "" || j == nil || j.GitHub.Result != "" {
				continue // cancelled before a runner took it, not ours, or a redelivered message
			}
			g := *j.GitHub
			g.Result = jc.Result
			j.GitHub = &g
			j.save()
			now := time.Now()
			p.Runs, p.LastRun = p.Runs+1, &now
			p.save()
		}
		if st := m.Statistics; st != nil {
			p.Assigned, fresh = st.TotalAssignedJobs, true
		}
	}
	live := s.githubJobsLocked(p)
	need := p.Assigned - len(live)
	var drop []*job
	if need < 0 && fresh {
		for _, j := range live {
			if len(drop) < -need && (j.GitHub == nil || j.GitHub.Job == "") {
				drop = append(drop, j)
			}
		}
	}
	if time.Now().Before(p.pauseTil) || p.State == api.GitHubRemoving {
		need = 0
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
// job's secrets (memory, then tmpfs), never in the job record. A pool whose jobs share state
// (one repository, or --shared-cache) gives it a free slot; otherwise it starts from nothing
// in its own job directory.
func (s *Server) startGitHubRunner(ctx context.Context, p *githubPool) error {
	id := newID("j-")
	s.mu.Lock()
	token := p.token
	slot, dir := -1, filepath.Join(s.jobDir(id), "github")
	if p.Shared {
		slot = 0
		for p.slots[slot] != "" {
			slot++
		}
		p.slots[slot], dir = id, p.slotDir(slot)
	}
	s.mu.Unlock()
	releaseLocked := func() {
		if slot >= 0 && p.slots[slot] == id {
			delete(p.slots, slot)
		}
	}
	work := filepath.Join(dir, "work")
	for _, d := range []string{work, filepath.Join(dir, "containers"), filepath.Join(dir, "rustup")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			s.mu.Lock()
			releaseLocked()
			s.mu.Unlock()
			return err
		}
	}
	name := p.ScaleSet + "-" + strings.TrimPrefix(id, "j-")
	jit, err := p.gh.GenerateJitRunnerConfig(ctx, &scaleset.RunnerScaleSetJitRunnerSetting{Name: name, WorkFolder: work}, p.ScaleSetID)
	var install string
	if err == nil {
		install, err = s.actionsRunner(ctx, token)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err == nil && (ctx.Err() != nil || p.State == api.GitHubRemoving) {
		err = errors.New("the pool is stopping")
	}
	if err != nil {
		releaseLocked()
		if slot < 0 {
			go removeAll(s.jobDir(id))
		}
		if jit != nil && jit.Runner != nil {
			go func(gh githubAPI, rid int64) { _ = gh.RemoveRunner(context.Background(), rid) }(p.gh, int64(jit.Runner.ID))
		}
		if ctx.Err() != nil || p.State == api.GitHubRemoving {
			return nil
		}
		return fmt.Errorf("starting a runner: %w", err)
	}
	spec := githubSpec(p)
	now := time.Now()
	j := &job{Job: api.Job{
		ID: id, Runner: s.cfg.Name, State: api.StateQueued, Spec: spec, Caller: "github",
		Resources: p.Resources, CreatedAt: now, Runtime: "container", Proto: api.Proto, MemFrom: "requested",
		GitHub: &api.GitHubJob{Owner: p.Owner, Runner: name, Slot: slot, Dir: dir, Install: install},
	}, queuedAt: now}
	if jit.Runner != nil {
		j.GitHub.RunnerID = jit.Runner.ID
	}
	j.Image, _, _ = envPlan(spec.Environment)
	j.dir = s.jobDir(j.ID)
	j.secrets = map[string]string{githubJITEnv: jit.EncodedJITConfig}
	j.Spec.SecretNames = []string{githubJITEnv}
	j.MemBooked = s.bookMemLocked(j.Spec, j.Resources.Mem)
	fail := func(err error) error {
		releaseLocked()
		go removeAll(j.dir)
		if j.GitHub.RunnerID != 0 {
			go func(gh githubAPI, rid int64) { _ = gh.RemoveRunner(context.Background(), rid) }(p.gh, int64(j.GitHub.RunnerID))
		}
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
	if slot >= 0 { // dates the pool's dependency caches for age-based GC
		marker := s.cacheDir("github", p.key, ".used")
		_ = os.MkdirAll(filepath.Dir(marker), 0o755)
		_ = os.WriteFile(marker, nil, 0o644)
	}
	s.kick()
	return nil
}

func githubSlotOf(j *job) (int, bool) {
	if j.GitHub == nil || j.GitHub.Slot < 0 {
		return 0, false
	}
	return j.GitHub.Slot, true
}

// githubFinishedLocked frees a finished runner job's slot, or deletes its own state (a container
// image store can be large). A runner that never took a GitHub job counts as a failure; several
// in a row pause the pool so a broken setup doesn't spin.
func (s *Server) githubFinishedLocked(j *job) {
	if j.GitHub != nil && j.GitHub.Slot < 0 && j.GitHub.Dir == filepath.Join(j.dir, "github") {
		go removeAll(j.GitHub.Dir)
	}
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
	if err := os.MkdirAll(base, 0o755); err != nil {
		return "", err
	}
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
	repo := strings.Contains(owner, "/")
	s.githubOp.Lock()
	defer s.githubOp.Unlock()
	// Local checks first: nothing changes on GitHub for a request this runner would refuse.
	s.mu.Lock()
	p := s.github[key]
	name := githubScaleSetName(s.cfg.Name, runnerID(s.cfg.StateDir))
	shared := repo || (req.SharedCache != nil && *req.SharedCache)
	problem, code := "", 0
	switch {
	case s.serveCtx == nil:
		problem, code = "runner is starting; try again in a few seconds", 503
	case p != nil && p.State == api.GitHubRemoving:
		problem, code = owner+" is still being removed here; try again in a minute", http.StatusConflict
	case p != nil && !repo && req.SharedCache != nil && *req.SharedCache != p.Shared:
		// Runners already started keep the state they were given: changing it means starting over.
		problem, code = "--shared-cache is chosen when "+owner+" is added: run offload github rm "+owner+" first", http.StatusConflict
	case p != nil:
		name, shared = p.ScaleSet, p.Shared
	}
	s.mu.Unlock()
	if problem != "" {
		writeErr(w, code, problem)
		return
	}
	c, err := s.githubClient(canonical, req.Token)
	var ss *scaleset.RunnerScaleSet
	if err == nil {
		ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
		ss, err = ensureScaleSet(ctx, c, name, req.Labels)
		cancel()
	}
	if err != nil {
		writeErr(w, http.StatusBadGateway, githubProblem(err, owner))
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if cur := s.github[key]; cur != p || (p != nil && p.State == api.GitHubRemoving) {
		writeErr(w, http.StatusConflict, owner+" changed on this runner meanwhile; run the command again")
		return
	}
	dir := filepath.Join(s.githubDir(), key)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	rec := githubPoolRecord{Owner: owner, URL: canonical, ScaleSet: name, CreatedAt: time.Now()}
	if p != nil {
		rec = p.record()
	}
	rec.Labels, rec.Max, rec.Resources, rec.TokenFrom, rec.Shared = req.Labels, req.Max, res, req.TokenFrom, shared
	if ss != nil && ss.Name == name {
		rec.ScaleSetID = ss.ID
	}
	rec.Token = req.Token
	if err := writeFileJSON(filepath.Join(dir, "pool.json"), rec); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if p == nil {
		p = s.newPool(key, dir, rec, req.Token)
		s.github[key] = p
	} else {
		p.token, p.Labels, p.Max, p.Resources, p.TokenFrom, p.Shared, p.ScaleSetID = req.Token, rec.Labels, rec.Max, rec.Resources, rec.TokenFrom, rec.Shared, rec.ScaleSetID
	}
	p.State, p.Detail = api.GitHubPreparing, "connecting to GitHub"
	s.restartPoolLocked(p)
	log.Printf("github %s: taking jobs labeled %s as scale set %s", owner, strings.Join(p.Labels, ","), name)
	writeJSON(w, s.githubViewLocked(p))
}

// handleGitHubRemove stops taking an owner's jobs here: it stops the runners, deletes this
// runner's scale set on GitHub, then the pool's files. ?forget=1 skips GitHub (a revoked token
// can't delete it), so a removal can always finish.
func (s *Server) handleGitHubRemove(w http.ResponseWriter, r *http.Request) {
	s.githubOp.Lock()
	defer s.githubOp.Unlock()
	s.mu.Lock()
	defer s.mu.Unlock()
	p := s.github[r.PathValue("key")]
	if p == nil {
		writeErr(w, 404, "this runner doesn't take GitHub jobs for that owner")
		return
	}
	forget := r.URL.Query().Get("forget") == "1"
	if p.State != api.GitHubRemoving || (forget && !p.forget) {
		state, detail, was := p.State, p.Detail, p.forget
		p.State, p.forget = api.GitHubRemoving, p.forget || forget
		if state != api.GitHubRemoving {
			p.Detail = "stopping its runners"
		}
		if err := p.save(); err != nil {
			p.State, p.Detail, p.forget = state, detail, was
			writeErr(w, 500, "can't record the removal: "+err.Error())
			return
		}
		if state == api.GitHubRemoving {
			writeJSON(w, s.githubViewLocked(p)) // finishRemoval picks up forget
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
	}
	if done != nil {
		<-done
	}
	for {
		s.mu.Lock()
		left := s.githubJobsLocked(p)
		for _, j := range left { // runners created while the listener was stopping too
			if !j.StopRequested {
				s.cancelJobLocked(j, "GitHub runners removed for "+p.Owner)
			}
		}
		s.mu.Unlock()
		if len(left) == 0 {
			break
		}
		time.Sleep(time.Second)
	}
	for wait := 10 * time.Second; ; wait = min(wait*2, 10*time.Minute) {
		s.mu.Lock()
		forget := p.forget
		s.mu.Unlock()
		if forget {
			break
		}
		err := s.deleteScaleSet(p)
		if err == nil {
			break
		}
		s.mu.Lock()
		p.Detail = fmt.Sprintf("couldn't delete scale set %s on GitHub (%s); retrying. If the token no longer works, delete it under Settings → Actions → Runners and run: offload github rm %s --forget",
			p.ScaleSet, githubProblem(err, p.Owner), p.Owner)
		s.mu.Unlock()
		log.Printf("github %s: %s", p.Owner, p.Detail)
		for end := time.Now().Add(wait); time.Now().Before(end); time.Sleep(time.Second) {
			s.mu.Lock()
			forget := p.forget
			s.mu.Unlock()
			if forget {
				break
			}
		}
	}
	err := removeAll(p.dir)
	if err == nil {
		err = removeAll(s.cacheDir("github", p.key))
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

// deleteScaleSet deletes this runner's scale set for the pool on GitHub (done if it's gone).
func (s *Server) deleteScaleSet(p *githubPool) error {
	s.mu.Lock()
	url, token, id, name := p.URL, p.token, p.ScaleSetID, p.ScaleSet
	s.mu.Unlock()
	c, err := s.githubClient(url, token)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if id == 0 {
		ss, err := c.GetRunnerScaleSet(ctx, 1, name)
		if err != nil || ss == nil {
			return err
		}
		id = ss.ID
	}
	if err := c.DeleteRunnerScaleSet(ctx, id); err != nil && !errors.Is(err, scaleset.NotFoundError) {
		return err
	}
	return nil
}
