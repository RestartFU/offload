package runner

import (
	"net/http"
	"net/http/httptest"
	"testing"

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
