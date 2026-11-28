package runner

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/RestartFU/offload/internal/api"
)

func TestPauseRefusesNewWorkUntilResumed(t *testing.T) {
	dir := t.TempDir()
	cfg := &Config{StateDir: dir, memCap: 8 << 30, defMem: 1 << 30, memReserve: -1}
	cfg.Capacity.CPU, cfg.Defaults.CPU = 4, 1
	s := &Server{cfg: cfg, jobs: map[string]*job{}, sessions: map[string]*session{}, wake: make(chan struct{}, 1)}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/admin/pause", s.handlePause)
	mux.HandleFunc("POST /v1/jobs", s.handleCreateJob)
	call := func(method, path, body string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, httptest.NewRequest(method, path, strings.NewReader(body)))
		return w
	}
	pause := func(on bool) api.Status {
		t.Helper()
		w := call("POST", "/v1/admin/pause", map[bool]string{true: `{"paused":true}`, false: `{"paused":false}`}[on])
		var st api.Status
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &st) != nil {
			t.Fatalf("pause %v: %d %s", on, w.Code, w.Body)
		}
		return st
	}

	st := pause(true)
	if !st.Paused || st.Accepting || st.Reason != "paused" {
		t.Fatalf("paused status: paused %v accepting %v reason %q", st.Paused, st.Accepting, st.Reason)
	}
	if _, err := os.Stat(s.pausedPath()); err != nil {
		t.Fatalf("pause isn't kept across restarts: %v", err)
	}
	if w := call("POST", "/v1/jobs", `{"argv":["true"]}`); w.Code != 403 {
		t.Fatalf("job on a paused runner: %d %s", w.Code, w.Body)
	}
	if len(s.jobs) != 0 {
		t.Fatalf("paused runner recorded %d job(s)", len(s.jobs))
	}

	st = pause(false)
	if st.Paused || !st.Accepting {
		t.Fatalf("resumed status: paused %v accepting %v reason %q", st.Paused, st.Accepting, st.Reason)
	}
	if _, err := os.Stat(s.pausedPath()); !os.IsNotExist(err) {
		t.Fatalf("resume left the pause file: %v", err)
	}
	if w := call("POST", "/v1/jobs", `{"argv":["true"]}`); w.Code != 200 {
		t.Fatalf("job after resume: %d %s", w.Code, w.Body)
	}
}
