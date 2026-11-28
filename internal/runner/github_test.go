package runner

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/actions/scaleset"

	"github.com/RestartFU/offload/internal/api"
)

type fakeGitHub struct {
	mu      sync.Mutex
	made    []scaleset.RunnerScaleSetJitRunnerSetting
	removed []int64
}

func (f *fakeGitHub) GenerateJitRunnerConfig(_ context.Context, set *scaleset.RunnerScaleSetJitRunnerSetting, _ int) (*scaleset.RunnerScaleSetJitRunnerConfig, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.made = append(f.made, *set)
	return &scaleset.RunnerScaleSetJitRunnerConfig{Runner: &scaleset.RunnerReference{ID: 100 + len(f.made), Name: set.Name},
		EncodedJITConfig: "JIT-SECRET-" + set.Name}, nil
}

func (f *fakeGitHub) RemoveRunner(_ context.Context, id int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.removed = append(f.removed, id)
	return nil
}

// newGitHubServer is a runner with one pool for acme and an actions runner already installed.
func newGitHubServer(t *testing.T) (*Server, *githubPool, *fakeGitHub) {
	t.Helper()
	dir := t.TempDir()
	cfg := &Config{StateDir: dir, memCap: 32 << 30, memReserve: -(1 << 50)}
	cfg.Capacity.CPU, cfg.Policy.CPUOvercommit, cfg.Policy.MemOvercommit = 8, 2, 2
	s := &Server{cfg: cfg, jobs: map[string]*job{}, github: map[string]*githubPool{}, cpuUse: map[int]int{}}
	install := filepath.Join(s.cacheDir("actions-runner"), "2.338.2")
	if err := os.MkdirAll(install, 0o755); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(install, "run.sh"), nil, 0o755)
	_ = os.WriteFile(filepath.Join(s.cacheDir("actions-runner"), "current"), []byte("2.338.2"), 0o644)
	_ = os.WriteFile(filepath.Join(s.cacheDir("actions-runner"), "checked"), nil, 0o644)
	key := api.GitHubKey("acme")
	p := s.newPool(key, filepath.Join(s.githubDir(), key), githubPoolRecord{Owner: "acme", URL: "https://github.com/acme",
		ScaleSet: "offload-box", ScaleSetID: 7, Labels: []string{"offload"}, Resources: api.Resources{CPU: 2, CPUMax: 8, Mem: 8 << 30}}, "TOKEN")
	f := &fakeGitHub{}
	p.gh = f
	_ = os.MkdirAll(p.dir, 0o700)
	s.github[key] = p
	return s, p, f
}

func stats(assigned int) *scaleset.RunnerScaleSetMessage {
	return &scaleset.RunnerScaleSetMessage{Statistics: &scaleset.RunnerScaleSetStatistic{TotalAssignedJobs: assigned}}
}

// GitHub assigns jobs; each gets a one-job runner in its own slot. Its config (a credential)
// reaches only the job's secrets, never a record on disk.
func TestGitHubRunnerPerAssignedJob(t *testing.T) {
	s, p, f := newGitHubServer(t)
	sc := &githubScaler{s: s, p: p}
	if err := sc.Scale(context.Background(), stats(2)); err != nil {
		t.Fatal(err)
	}
	live := s.githubJobsLocked(p)
	if len(live) != 2 || len(f.made) != 2 {
		t.Fatalf("%d runner jobs, %d runners made", len(live), len(f.made))
	}
	slots := map[string]bool{}
	for _, j := range live {
		if j.State != api.StateQueued || j.secrets[githubJITEnv] != "JIT-SECRET-"+j.GitHub.Runner || j.GitHub.RunnerID == 0 {
			t.Fatalf("runner job %+v", j.Job)
		}
		if b, _ := os.ReadFile(filepath.Join(j.dir, "job.json")); strings.Contains(string(b), "JIT-SECRET") || len(b) == 0 {
			t.Fatalf("job record holds the runner config: %s", b)
		}
		slots[j.Spec.Env["OFFLOAD_GITHUB_SLOT"]] = true
	}
	if len(slots) != 2 || !strings.HasPrefix(f.made[0].WorkFolder, p.slotDir(0)) && !strings.HasPrefix(f.made[0].WorkFolder, p.slotDir(1)) {
		t.Fatalf("slots %v, work folder %q", slots, f.made[0].WorkFolder)
	}
	if err := sc.Scale(context.Background(), stats(2)); err != nil || len(s.githubJobsLocked(p)) != 2 {
		t.Fatalf("the same assignment again started more runners: %v", err)
	}

	busy, idle := live[0], live[1]
	started := &scaleset.JobStarted{RunnerName: busy.GitHub.Runner, JobMessageBase: scaleset.JobMessageBase{
		OwnerName: "acme", RepositoryName: "app", JobDisplayName: "test (linux)", WorkflowRunID: 42,
		JobWorkflowRef: "acme/app/.github/workflows/ci.yml@refs/heads/main"}}
	if err := sc.Scale(context.Background(), &scaleset.RunnerScaleSetMessage{JobStartedMessages: []*scaleset.JobStarted{started}}); err != nil {
		t.Fatal(err)
	}
	g := busy.GitHub
	if g.Repo != "acme/app" || g.Job != "test (linux)" || g.Workflow != "ci.yml" || g.URL != "https://github.com/acme/app/actions/runs/42" {
		t.Fatalf("GitHub job %+v", g)
	}

	// The other job was cancelled on GitHub: its idle runner goes, the busy one stays.
	if err := sc.Scale(context.Background(), stats(1)); err != nil {
		t.Fatal(err)
	}
	if idle.State != api.StateCancelled || busy.State != api.StateQueued || len(f.removed) != 1 || f.removed[0] != int64(idle.GitHub.RunnerID) {
		t.Fatalf("idle %s, busy %s, removed %v", idle.State, busy.State, f.removed)
	}
	if n, _ := githubSlotOf(idle); p.slots[n] != "" {
		t.Fatal("cancelled runner kept its slot")
	}

	done := &scaleset.JobCompleted{Result: "succeeded", RunnerName: busy.GitHub.Runner}
	if err := sc.Scale(context.Background(), &scaleset.RunnerScaleSetMessage{JobCompletedMessages: []*scaleset.JobCompleted{done}}); err != nil {
		t.Fatal(err)
	}
	if p.Runs != 1 || p.LastRun == nil || busy.GitHub.Result != "succeeded" {
		t.Fatalf("runs %d, result %q", p.Runs, busy.GitHub.Result)
	}
	if got := ledgerCommand(busy); got != "GitHub acme/app · test (linux)" {
		t.Fatalf("ledger command %q", got)
	}
}

// Capacity is the pool's runners plus as many more of its size as would be admitted now.
func TestGitHubCapacity(t *testing.T) {
	s, p, _ := newGitHubServer(t)
	if n := s.githubCapacityLocked(p); n != 4 {
		t.Fatalf("empty 8-core runner, 2 cores a job: capacity %d, want 4", n)
	}
	other := &job{}
	other.ID, other.State, other.Resources, other.StartedAt = "j-other", api.StateRunning, api.Resources{CPU: 5, Mem: 1 << 30}, &time.Time{}
	s.jobs[other.ID] = other
	if n := s.githubCapacityLocked(p); n != 1 {
		t.Fatalf("with 5 cores taken: capacity %d, want 1", n)
	}
	sc := &githubScaler{s: s, p: p}
	if err := sc.Scale(context.Background(), stats(1)); err != nil {
		t.Fatal(err)
	}
	if n := s.githubCapacityLocked(p); n != 1 {
		t.Fatalf("its own queued runner counts once: capacity %d, want 1", n)
	}
	delete(s.jobs, other.ID)
	s.paused = true
	if n := s.githubCapacityLocked(p); n != 1 {
		t.Fatalf("paused runner: capacity %d, want only its own runner", n)
	}
	s.paused = false
	p.Max = 2
	if n := s.githubCapacityLocked(p); n != 2 {
		t.Fatalf("--max 2: capacity %d", n)
	}
}

// Runners that keep ending without a job pause the pool instead of spinning.
func TestGitHubFailingRunnersPause(t *testing.T) {
	s, p, _ := newGitHubServer(t)
	sc := &githubScaler{s: s, p: p}
	for i := 0; i < 3; i++ {
		if err := sc.Scale(context.Background(), stats(1)); err != nil {
			t.Fatal(err)
		}
		j := s.githubJobsLocked(p)[0]
		code := 1
		j.State = api.StateRunning
		s.finishLocked(j, api.StateExited, &code, "")
	}
	if !time.Now().Before(p.pauseTil) {
		t.Fatalf("not paused after %d failures", p.failures)
	}
	if err := sc.Scale(context.Background(), stats(1)); err != nil || len(s.githubJobsLocked(p)) != 0 {
		t.Fatalf("started a runner while paused: %v", err)
	}
}

// Pools and their runners' slots come back after a restart.
func TestGitHubRestore(t *testing.T) {
	s, p, _ := newGitHubServer(t)
	if err := writeSecretFile(filepath.Join(p.dir, "token"), "TOKEN"); err != nil || p.save() != nil {
		t.Fatal(err)
	}
	if err := (&githubScaler{s: s, p: p}).Scale(context.Background(), stats(1)); err != nil {
		t.Fatal(err)
	}
	j := s.githubJobsLocked(p)[0]
	if fi, err := os.Stat(filepath.Join(p.dir, "token")); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("token file: %v %v", fi, err)
	}
	s2 := &Server{cfg: s.cfg, jobs: map[string]*job{j.ID: j}, github: map[string]*githubPool{}}
	s2.restoreGitHub()
	r := s2.github["acme"]
	if r == nil || r.token != "TOKEN" || r.ScaleSetID != 7 || r.Labels[0] != "offload" {
		t.Fatalf("restored %+v", r)
	}
	if n, _ := githubSlotOf(j); r.slots[n] != j.ID {
		t.Fatalf("slots %v", r.slots)
	}
}

func TestGitHubScaleSetName(t *testing.T) {
	for in, want := range map[string]string{"vps-2": "offload-vps-2", "Big Box_1": "offload-big-box-1", "∆": "offload-runner"} {
		if got := githubScaleSetName(in); got != want {
			t.Errorf("%q: %q, want %q", in, got, want)
		}
	}
}
