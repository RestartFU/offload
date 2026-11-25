package runner

import (
	"math"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/RestartFU/offload/internal/api"
)

// ?moved=1 cancels only a job still waiting in the queue; a started one is left running.
func TestMoveCancelsOnlyQueuedJobs(t *testing.T) {
	dir := t.TempDir()
	run := &job{dir: dir}
	run.ID, run.State = "j-run", api.StateRunning
	s := &Server{cfg: &Config{StateDir: dir}, jobs: map[string]*job{"j-run": run}}
	mux := http.NewServeMux()
	mux.HandleFunc("DELETE /v1/jobs/{id}", s.handleCancelJob)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest("DELETE", "/v1/jobs/j-run?moved=1", nil))
	if w.Code != 409 || run.State != api.StateRunning || run.StopRequested || run.cancelled.Load() {
		t.Fatalf("running job: %d, state %s, stop %v", w.Code, run.State, run.StopRequested)
	}
}

func TestStatusCountsPendingJobs(t *testing.T) {
	dir := t.TempDir()
	q := &job{dir: dir}
	q.ID, q.State, q.Resources = "j-q", api.StateQueued, api.Resources{CPU: 16, Mem: 4 << 30}
	u := &job{dir: dir}
	u.ID, u.State, u.Resources = "j-u", api.StateUploading, api.Resources{CPU: 2, Mem: 1 << 30}
	gone := &job{dir: dir}
	gone.ID, gone.State, gone.StopRequested, gone.Resources = "j-g", api.StateQueued, true, api.Resources{CPU: 8}
	s := &Server{cfg: &Config{StateDir: dir}, jobs: map[string]*job{"j-q": q, "j-u": u, "j-g": gone}}
	st := s.status()
	if st.Pending.CPU != 18 || st.Pending.Mem != 5<<30 || !st.Requeue {
		t.Fatalf("pending %+v, requeue %v", st.Pending, st.Requeue)
	}
}

// Past capacity, CPU guarantees are overbooked only while the cores are really idle, and never
// beyond the overcommit limit.
func TestCPUOvercommit(t *testing.T) {
	cfg := &Config{memCap: 64 << 30}
	cfg.Capacity.CPU, cfg.Policy.CPUOvercommit = 30, 2
	long := time.Now().Add(-time.Hour)
	run := &job{}
	run.ID, run.State, run.StartedAt, run.Resources = "j-1", api.StateRunning, &long, api.Resources{CPU: 16}
	s := &Server{cfg: cfg, jobs: map[string]*job{"j-1": run}}
	want := api.Resources{CPU: 16, Mem: 1 << 20}
	setBusy := func(cores float64, at time.Time) {
		s.poolBusy.Store(math.Float64bits(cores))
		s.poolBusyAt.Store(at.UnixNano())
	}

	setBusy(2, time.Now()) // the running job's 16 are mostly unused
	if ok, why := s.fitsLocked(want); !ok {
		t.Fatalf("idle cores: refused (%s)", why)
	}
	setBusy(20, time.Now())
	if ok, _ := s.fitsLocked(want); ok {
		t.Fatal("busy host: admitted past capacity")
	}
	setBusy(0, time.Now().Add(-time.Minute))
	if ok, _ := s.fitsLocked(want); ok {
		t.Fatal("stale sample: admitted past capacity")
	}
	// Work started since the sample counts as busy: it can't show in the sample yet.
	// (A 2-cpu job would still fit in the 12 cores left.)
	setBusy(2, time.Now().Add(-2*time.Second))
	now := time.Now()
	fresh := &job{}
	fresh.ID, fresh.State, fresh.StartedAt, fresh.Resources = "j-2", api.StateRunning, &now, api.Resources{CPU: 16}
	s.jobs["j-2"] = fresh
	if ok, _ := s.fitsLocked(api.Resources{CPU: 14, Mem: 1 << 20}); ok { // 30 - 2 busy - 16 just started = 12 idle
		t.Fatal("admitted on cores a just-started job holds")
	}
	// Never past capacity × overcommit, however idle.
	delete(s.jobs, "j-2")
	run.Resources.CPU = 50
	setBusy(0, time.Now())
	if ok, _ := s.fitsLocked(want); ok {
		t.Fatal("admitted past the overcommit limit")
	}
	// Within capacity nothing changes: no sample needed.
	run.Resources.CPU = 4
	s.poolBusyAt.Store(0)
	if ok, why := s.fitsLocked(want); !ok {
		t.Fatalf("within capacity: refused (%s)", why)
	}
}
