package runner

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/RestartFU/offload/internal/api"
)

func TestSliceCgroupPath(t *testing.T) {
	for in, want := range map[string]string{
		"offload.slice":      "offload.slice",
		"offload-jobs.slice": "offload.slice/offload-jobs.slice",
		"a-b-c.slice":        "a.slice/a-b.slice/a-b-c.slice",
		"-.slice":            "",
	} {
		if got := sliceCgroupPath(in); got != want {
			t.Errorf("sliceCgroupPath(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestBucketAverages(t *testing.T) {
	pts := []api.MetricPoint{{T: 60, CPUBusy: 2, MemUsed: 100, Running: 1}, {T: 90, CPUBusy: 4, MemUsed: 300, Running: 2}, {T: 120, CPUBusy: 1}}
	got := bucket(pts, 60)
	if len(got) != 2 || got[0].T != 60 || got[0].CPUBusy != 3 || got[0].MemUsed != 200 || got[0].Running != 1.5 || got[1].T != 120 {
		t.Fatalf("bucket: %+v", got)
	}
}

func TestSeriesRanges(t *testing.T) {
	s := &Server{}
	now := time.Now().Unix()
	for i := 0; i < 3*24*60; i++ { // three days of minutes
		s.metrics.minute = append(s.metrics.minute, api.MetricPoint{T: now - int64(3*24*60-i)*60, CPUBusy: 1})
	}
	for i := 0; i < rawKeep; i++ {
		s.metrics.raw = append(s.metrics.raw, api.MetricPoint{T: now - int64(rawKeep-i)*5})
	}
	if sr := s.series(time.Hour); sr.Step != 5 || len(sr.Points) < rawKeep-1 {
		t.Errorf("1h: step %d, %d points", sr.Step, len(sr.Points))
	}
	if sr := s.series(6 * time.Hour); sr.Step != 60 || len(sr.Points) > maxPoints+1 {
		t.Errorf("6h: step %d, %d points", sr.Step, len(sr.Points))
	}
	sr := s.series(7 * 24 * time.Hour)
	if sr.Step != 900 || len(sr.Points) > maxPoints || len(sr.Points) < 3*24*4-2 {
		t.Errorf("7d: step %d, %d points", sr.Step, len(sr.Points))
	}
	for _, p := range sr.Points {
		if p.CPUBusy < 0.999999 || p.CPUBusy > 1.000001 {
			t.Fatalf("7d: averaging changed a constant series: %+v", p)
		}
	}
}

func TestReadProcStat(t *testing.T) {
	cpus := readProcStat()
	if len(cpus) == 0 {
		t.Skip("no /proc/stat")
	}
	for id, c := range cpus {
		if c.total == 0 || c.busy > c.total {
			t.Errorf("cpu%d: %+v", id, c)
		}
	}
}

func TestLedgerAppendReadGC(t *testing.T) {
	dir := t.TempDir()
	s := &Server{cfg: &Config{StateDir: dir, ledgerRetain: 24 * time.Hour}}
	old := time.Now().Add(-48 * time.Hour)
	start := old.Add(time.Second)
	s.appendLedger(api.LedgerEntry{ID: "j-old", Kind: "job", CreatedAt: old, StartedAt: &start}, &start)
	created := time.Now().Add(-time.Minute)
	started := created.Add(10 * time.Second)
	fin := started.Add(30 * time.Second)
	s.appendLedger(api.LedgerEntry{ID: "j-new", Kind: "job", CreatedAt: created, StartedAt: &started}, &fin)
	// A torn last line (a crash mid-append) is skipped, not fatal.
	f, _ := os.OpenFile(s.ledgerPath(), os.O_WRONLY|os.O_APPEND, 0)
	f.WriteString(`{"id":"j-torn","fin`)
	f.Close()

	got, _, _ := s.readLedger(time.Now().Add(-time.Hour))
	if len(got) != 1 || got[0].ID != "j-new" || got[0].QueueWait != 10 || got[0].Duration != 30 {
		t.Fatalf("read: %+v", got)
	}
	if all, _, _ := s.readLedger(time.Time{}); len(all) != 2 {
		t.Fatalf("read all: %d entries", len(all))
	}
	s.gcLedger()
	b, _ := os.ReadFile(s.ledgerPath())
	if strings.Contains(string(b), "j-old") || !strings.Contains(string(b), "j-new") {
		t.Fatalf("gc kept %q", b)
	}

	w := httptest.NewRecorder()
	s.handleLedger(w, httptest.NewRequest("GET", "/v1/ledger?since=yesterday", nil))
	if w.Code != 400 {
		t.Errorf("bad since: %d", w.Code)
	}
}

func TestCgroupCPUUsec(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "cpu.stat"), []byte("usage_usec 1234567\nuser_usec 1000000\nsystem_usec 234567\n"), 0o600)
	if v, ok := cgroupCPUUsec(dir); !ok || v != 1234567 {
		t.Fatalf("got %d %v", v, ok)
	}
	if _, ok := cgroupCPUUsec(filepath.Join(dir, "gone")); ok {
		t.Fatal("missing cgroup reported usage")
	}
}

func TestLedgerRepairsTornLineAndDedups(t *testing.T) {
	s := &Server{cfg: &Config{StateDir: t.TempDir(), ledgerRetain: 24 * time.Hour}}
	now := time.Now()
	s.appendLedger(api.LedgerEntry{ID: "j-1", Kind: "job", CreatedAt: now, State: "exited"}, &now)
	f, _ := os.OpenFile(s.ledgerPath(), os.O_WRONLY|os.O_APPEND, 0)
	f.WriteString(`{"id":"j-torn","fin`) // a crash mid-append
	f.Close()
	// The next completion must survive, not be glued to the torn fragment.
	s.appendLedger(api.LedgerEntry{ID: "j-2", Kind: "job", CreatedAt: now}, &now)
	// Recorded twice (a crash before its terminal state was saved, then finished again after the
	// restart): counted once, with the first record's time.
	later := now.Add(8 * time.Hour)
	s.appendLedger(api.LedgerEntry{ID: "j-1", Kind: "job", CreatedAt: now, State: "failed"}, &later)
	// An oversized line is skipped without hiding what follows it.
	f, _ = os.OpenFile(s.ledgerPath(), os.O_WRONLY|os.O_APPEND, 0)
	f.WriteString(`{"id":"j-big","client":"` + strings.Repeat("x", ledgerMaxLine+10) + "\"}\n")
	f.Close()
	s.appendLedger(api.LedgerEntry{ID: "j-3", Kind: "job", CreatedAt: now, Client: strings.Repeat("é", 1000)}, &now)

	got, truncated, err := s.readLedger(time.Time{})
	if err != nil || truncated {
		t.Fatalf("read: %v truncated=%v", err, truncated)
	}
	var ids []string
	for _, e := range got {
		ids = append(ids, e.ID)
	}
	if strings.Join(ids, ",") != "j-1,j-2,j-3" {
		t.Fatalf("ids %v", ids)
	}
	if got[0].State != "exited" || !got[0].FinishedAt.Equal(now) {
		t.Errorf("dedup didn't keep the first entry: %+v", got[0])
	}
	if c := got[2].Client; len(c) > ledgerMaxField || !utf8.ValidString(c) {
		t.Errorf("client not clipped on a rune boundary: %d bytes", len(c))
	}
}

func TestMetricsSaveKeepsThePartialMinute(t *testing.T) {
	s := &Server{cfg: &Config{StateDir: t.TempDir()}}
	s.loadMetrics()
	m := &s.metrics
	t0 := time.Now().Unix() / 60 * 60
	m.minute = []api.MetricPoint{{T: t0 - 60, CPUBusy: 1}}
	m.pending = []api.MetricPoint{{T: t0, CPUBusy: 2}, {T: t0 + 5, CPUBusy: 4}}
	s.saveMetrics()

	s2 := &Server{cfg: s.cfg}
	s2.loadMetrics()
	if got := s2.metrics.minute; len(got) != 2 || got[1].T != t0 || got[1].CPUBusy != 3 {
		t.Fatalf("loaded %+v", got)
	}
	// The rest of that minute, sampled after the restart, merges into the same point.
	s2.metrics.minute = appendMinute(s2.metrics.minute, api.MetricPoint{T: t0, CPUBusy: 5})
	if got := s2.metrics.minute; len(got) != 2 || got[1].CPUBusy != 4 {
		t.Fatalf("merged %+v", got)
	}
}

func TestSessionObserveDoesNotAttach(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "stdout"), []byte("hello\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	se := &session{dir: dir}
	se.ID, se.State = "s-1", api.StateExited
	s := &Server{cfg: &Config{StateDir: dir}, sessions: map[string]*session{"s-1": se}}
	mux := http.NewServeMux() // the same patterns as Serve: they must not collide
	mux.HandleFunc("GET /v1/sessions/{id}/stdout", s.handleSessionStdout)
	mux.HandleFunc("GET /v1/sessions/{id}/observe/{stream}", s.handleSessionObserve)

	// A client stream counts as contact (LastSeen), which keeps an idle session alive.
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest("GET", "/v1/sessions/s-1/observe/stdout?follow=1", nil))
	if w.Code != 200 || w.Body.String() != "hello\n" || !se.LastSeen.IsZero() {
		t.Fatalf("observe: %d %q, last seen %v", w.Code, w.Body.String(), se.LastSeen)
	}
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest("GET", "/v1/sessions/s-1/stdout", nil))
	if w.Code != 200 || se.LastSeen.IsZero() || se.Attached != 0 {
		t.Fatalf("client stream: %d, last seen %v, attached %d", w.Code, se.LastSeen, se.Attached)
	}
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest("GET", "/v1/sessions/s-1/observe/secrets", nil))
	if w.Code != 404 {
		t.Errorf("unknown stream: %d", w.Code)
	}
}
