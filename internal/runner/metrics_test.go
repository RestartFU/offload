package runner

import (
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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

	got := s.readLedger(time.Now().Add(-time.Hour))
	if len(got) != 1 || got[0].ID != "j-new" || got[0].QueueWait != 10 || got[0].Duration != 30 {
		t.Fatalf("read: %+v", got)
	}
	if all := s.readLedger(time.Time{}); len(all) != 2 {
		t.Fatalf("read all: %d entries", len(all))
	}
	s.gcLedgerLocked()
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
