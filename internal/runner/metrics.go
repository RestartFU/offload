package runner

import (
	"bufio"
	"context"
	"encoding/json"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/sys/unix"

	"github.com/RestartFU/offload/internal/api"
	"github.com/RestartFU/offload/internal/units"
)

// The runner samples what the host and every workload use, so a dashboard can show real usage
// next to the guarantees the scheduler hands out. Samples are kept for an hour at full
// resolution and for a week as one-minute averages (persisted, so a restart keeps the week).

const (
	rawKeep    = int(time.Hour / api.MetricsStep)
	minuteKeep = 7 * 24 * 60
	maxPoints  = 720 // a series is downsampled to at most this many points
)

type metricsState struct {
	mu      sync.Mutex // guards the fields up to cpuSecs; held only briefly, never around I/O
	latest  api.Metrics
	raw     []api.MetricPoint // newest last, MetricsStep apart
	minute  []api.MetricPoint // one-minute averages, newest last
	pending []api.MetricPoint // raw points of the minute being filled
	saved   time.Time
	cpuSecs map[string]float64 // workload id -> CPU seconds last seen, for the ledger

	// Owned by the sampler goroutine alone.
	prevCPU  map[int]cpuTimes
	prevUnit map[string]unitSample // unit -> last usage reading
	cgPaths  map[string]string     // unit -> cgroup directory
}

type cpuTimes struct{ busy, total uint64 }

type unitSample struct {
	usec uint64
	at   time.Time
}

// workloadRef is what the sampler needs to know about a live job or session.
type workloadRef struct {
	id, kind, unit string
	cpus           []int
	mem            int64
}

func (s *Server) metricsPath() string { return filepath.Join(s.cfg.StateDir, "metrics.json") }

func (s *Server) loadMetrics() {
	m := &s.metrics
	m.prevCPU, m.prevUnit, m.cgPaths, m.cpuSecs = map[int]cpuTimes{}, map[string]unitSample{}, map[string]string{}, map[string]float64{}
	if b, err := os.ReadFile(s.metricsPath()); err == nil {
		var pts []api.MetricPoint
		if json.Unmarshal(b, &pts) == nil {
			cutoff := time.Now().Add(-time.Duration(minuteKeep) * time.Minute).Unix()
			for _, p := range pts {
				if p.T >= cutoff {
					m.minute = append(m.minute, p)
				}
			}
		}
	}
	m.saved = time.Now()
}

// sampleLoop takes a sample every MetricsStep until ctx ends. It is the only writer of
// metrics.json: saves never overlap, and the last one is done when samplerDone closes.
func (s *Server) sampleLoop(ctx context.Context) {
	defer func() {
		if s.samplerDone != nil {
			close(s.samplerDone)
		}
	}()
	tick := time.NewTicker(api.MetricsStep)
	defer tick.Stop()
	s.sample() // primes the CPU counters; the first busy values are since boot
	for {
		select {
		case <-ctx.Done():
			s.saveMetrics()
			return
		case <-tick.C:
			if s.sample() {
				s.saveMetrics()
			}
		}
	}
}

// sample takes one sample. It reports whether the minute averages are due to be saved.
func (s *Server) sample() (save bool) {
	now := time.Now()
	// Snapshot the scheduler's view under s.mu, then read /proc and cgroups without it.
	s.mu.Lock()
	var refs []workloadRef
	pt := api.MetricPoint{T: now.Unix()}
	for _, j := range s.jobs {
		switch j.State {
		case api.StateRunning:
			pt.Running++
			if j.Unit != "" {
				refs = append(refs, workloadRef{id: j.ID, kind: "job", unit: j.Unit, cpus: j.cpus, mem: j.Resources.Mem})
			}
		case api.StateQueued, api.StateUploading:
			pt.Queued++
		}
	}
	for _, se := range s.sessions {
		if !api.Terminal(se.State) {
			pt.Sessions++
			if se.Unit != "" {
				refs = append(refs, workloadRef{id: se.ID, kind: "session", unit: se.Unit, cpus: se.cpus, mem: se.Resources.Mem})
			}
		}
	}
	alloc := s.allocatedLocked()
	pt.CPUAlloc, pt.MemAlloc = alloc.CPU, alloc.Mem
	offered := map[int]bool{}
	for _, c := range s.cfg.cpus {
		offered[c] = true
	}
	s.mu.Unlock()

	// Read everything first: s.mu holders (finishing jobs) take m.mu, so it never waits on I/O.
	m := &s.metrics
	owners := map[int][]string{}
	for _, r := range refs {
		for _, c := range r.cpus {
			owners[c] = append(owners[c], r.id)
		}
	}
	cur := readProcStat()
	var cpus []api.CPUUsage
	for id, t := range cur {
		u := api.CPUUsage{ID: id, Reserved: len(offered) > 0 && !offered[id], Owners: owners[id]}
		if p, ok := m.prevCPU[id]; ok && t.total > p.total {
			u.Busy = float64(t.busy-p.busy) / float64(t.total-p.total)
		}
		pt.CPUBusy += u.Busy
		cpus = append(cpus, u)
	}
	sort.Slice(cpus, func(a, b int) bool { return cpus[a].ID < cpus[b].ID })
	m.prevCPU = cur

	var work []api.WorkloadUsage
	seen := map[string]bool{}
	cpuSecs := map[string]float64{}
	for _, r := range refs {
		dir := s.unitCgroupDir(r.unit)
		seen[r.unit] = true
		w := api.WorkloadUsage{ID: r.id, Kind: r.kind, Mem: cgroupFileInt(dir, "memory.current"), MemPeak: cgroupFileInt(dir, "memory.peak")}
		if usec, ok := cgroupCPUUsec(dir); ok {
			if p, ok := m.prevUnit[r.unit]; ok && usec >= p.usec {
				if dt := now.Sub(p.at).Seconds(); dt > 0 {
					w.CPU = float64(usec-p.usec) / 1e6 / dt
				}
			}
			m.prevUnit[r.unit] = unitSample{usec: usec, at: now}
			cpuSecs[r.id] = float64(usec) / 1e6
		}
		pt.CPUUsed += w.CPU
		pt.MemWork += w.Mem
		work = append(work, w)
	}
	for u := range m.prevUnit {
		if !seen[u] {
			delete(m.prevUnit, u)
			delete(m.cgPaths, u)
		}
	}
	sort.Slice(work, func(a, b int) bool { return work[a].ID < work[b].ID })

	total, avail := memTotal(), memAvailable()
	pt.MemUsed = total - avail
	disk := s.diskUsage()

	// Publish CPU time only for workloads still live: one that finished while we read has
	// already taken its value, and nothing would remove a late entry. Lock order: s.mu, m.mu.
	s.mu.Lock()
	defer s.mu.Unlock()
	m.mu.Lock()
	defer m.mu.Unlock()
	for id, v := range cpuSecs {
		if j := s.jobs[id]; j != nil && j.State == api.StateRunning {
			m.cpuSecs[id] = v
		} else if se := s.sessions[id]; se != nil && !api.Terminal(se.State) {
			m.cpuSecs[id] = v
		}
	}
	m.latest = api.Metrics{At: now, CPUs: cpus, MemTotal: total, MemAvailable: avail, Workloads: work, Disk: disk}

	m.raw = append(m.raw, pt)
	if len(m.raw) > rawKeep {
		m.raw = m.raw[len(m.raw)-rawKeep:]
	}
	if len(m.pending) > 0 && m.pending[0].T/60 != pt.T/60 {
		m.minute = appendMinute(m.minute, average(m.pending, m.pending[0].T/60*60))
		if len(m.minute) > minuteKeep {
			m.minute = m.minute[len(m.minute)-minuteKeep:]
		}
		m.pending = m.pending[:0]
	}
	m.pending = append(m.pending, pt)
	if time.Since(m.saved) > 5*time.Minute {
		m.saved = time.Now()
		return true
	}
	return false
}

// appendMinute adds a minute's average. The same minute can come twice: the part saved before a
// restart and the rest sampled after it. They are averaged into one point.
func appendMinute(pts []api.MetricPoint, p api.MetricPoint) []api.MetricPoint {
	if n := len(pts); n > 0 && pts[n-1].T == p.T {
		pts[n-1] = average([]api.MetricPoint{pts[n-1], p}, p.T)
		return pts
	}
	return append(pts, p)
}

func (s *Server) saveMetrics() {
	m := &s.metrics
	m.mu.Lock()
	pts := append([]api.MetricPoint(nil), m.minute...)
	if len(m.pending) > 0 { // the minute being filled, so a restart doesn't lose it
		pts = appendMinute(pts, average(m.pending, m.pending[0].T/60*60))
	}
	m.mu.Unlock()
	if err := writeFileJSON(s.metricsPath(), pts); err != nil {
		log.Printf("metrics: %v", err)
	}
}

// takeCPUSeconds returns (and forgets) the CPU time last sampled for a workload.
func (s *Server) takeCPUSeconds(id string) float64 {
	s.metrics.mu.Lock()
	defer s.metrics.mu.Unlock()
	v := s.metrics.cpuSecs[id]
	delete(s.metrics.cpuSecs, id)
	return v
}

// average folds points into one, stamped t.
func average(pts []api.MetricPoint, t int64) api.MetricPoint {
	out := api.MetricPoint{T: t}
	var memUsed, memWork, memAlloc int64
	for _, p := range pts {
		out.CPUBusy += p.CPUBusy
		out.CPUUsed += p.CPUUsed
		out.CPUAlloc += p.CPUAlloc
		memUsed += p.MemUsed
		memWork += p.MemWork
		memAlloc += p.MemAlloc
		out.Running += p.Running
		out.Queued += p.Queued
		out.Sessions += p.Sessions
	}
	n := float64(len(pts))
	out.CPUBusy, out.CPUUsed, out.CPUAlloc = out.CPUBusy/n, out.CPUUsed/n, out.CPUAlloc/n
	out.Running, out.Queued, out.Sessions = out.Running/n, out.Queued/n, out.Sessions/n
	k := int64(len(pts))
	out.MemUsed, out.MemWork, out.MemAlloc = memUsed/k, memWork/k, memAlloc/k
	return out
}

// seriesSteps are the bucket sizes (seconds) a long series is averaged into.
var seriesSteps = []int{60, 120, 300, 600, 900, 1800, 3600}

// series returns the last span of samples, downsampled to at most maxPoints.
func (s *Server) series(span time.Duration) api.Series {
	m := &s.metrics
	m.mu.Lock()
	defer m.mu.Unlock()
	cutoff := time.Now().Add(-span).Unix()
	src, step := m.minute, 60
	if span <= time.Hour {
		src, step = m.raw, int(api.MetricsStep/time.Second)
	} else if len(m.pending) > 0 {
		src = append(append([]api.MetricPoint(nil), src...), average(m.pending, m.pending[0].T/60*60))
	}
	var pts []api.MetricPoint
	for _, p := range src {
		if p.T >= cutoff {
			pts = append(pts, p)
		}
	}
	if need := int(span.Seconds()) / maxPoints; need > step {
		step = seriesSteps[len(seriesSteps)-1]
		for _, st := range seriesSteps {
			if st >= need {
				step = st
				break
			}
		}
		pts = bucket(pts, int64(step))
	}
	if pts == nil {
		pts = []api.MetricPoint{}
	}
	return api.Series{Step: step, Points: pts}
}

// bucket averages points into step-aligned buckets.
func bucket(pts []api.MetricPoint, step int64) []api.MetricPoint {
	var out []api.MetricPoint
	for i := 0; i < len(pts); {
		b := pts[i].T / step
		j := i
		for j < len(pts) && pts[j].T/step == b {
			j++
		}
		out = append(out, average(pts[i:j], b*step))
		i = j
	}
	return out
}

func (s *Server) handleMetrics(w http.ResponseWriter, r *http.Request) {
	s.metrics.mu.Lock()
	m := s.metrics.latest
	s.metrics.mu.Unlock()
	if m.CPUs == nil {
		m.CPUs, m.Workloads = []api.CPUUsage{}, []api.WorkloadUsage{}
	}
	writeJSON(w, m)
}

func (s *Server) handleSeries(w http.ResponseWriter, r *http.Request) {
	span := time.Hour
	if v := r.URL.Query().Get("range"); v != "" {
		d, err := units.ParseDuration(v)
		if err != nil || d <= 0 {
			writeErr(w, 400, "bad range: use e.g. 1h, 6h, 24h or 7d")
			return
		}
		span = min(d, time.Duration(minuteKeep)*time.Minute)
	}
	writeJSON(w, s.series(span))
}

// readProcStat returns cumulative busy and total ticks for every online CPU.
func readProcStat() map[int]cpuTimes {
	out := map[int]cpuTimes{}
	f, err := os.Open("/proc/stat")
	if err != nil {
		return out
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 9 || !strings.HasPrefix(fields[0], "cpu") || fields[0] == "cpu" {
			continue
		}
		id, err := strconv.Atoi(strings.TrimPrefix(fields[0], "cpu"))
		if err != nil {
			continue
		}
		var v [8]uint64 // user nice system idle iowait irq softirq steal (guest is inside user)
		for i := range v {
			v[i], _ = strconv.ParseUint(fields[i+1], 10, 64)
		}
		var total uint64
		for _, x := range v {
			total += x
		}
		out[id] = cpuTimes{busy: total - v[3] - v[4], total: total}
	}
	return out
}

// userManagerCgroup is the cgroup of this user's systemd manager (user@UID.service), under
// which every offload slice and unit lives.
func userManagerCgroup() string {
	for d := ownCgroup(); d != "" && d != "/sys/fs/cgroup" && d != "/"; d = filepath.Dir(d) {
		if strings.HasPrefix(filepath.Base(d), "user@") {
			return d
		}
	}
	return ""
}

// sliceCgroupPath expands a systemd slice name into its cgroup path: "a-b.slice" lives in
// "a.slice/a-b.slice".
func sliceCgroupPath(slice string) string {
	name := strings.TrimSuffix(slice, ".slice")
	if name == "" || name == "-" {
		return ""
	}
	parts := strings.Split(name, "-")
	var dirs []string
	for i := range parts {
		dirs = append(dirs, strings.Join(parts[:i+1], "-")+".slice")
	}
	return filepath.Join(dirs...)
}

// unitCgroupDir finds a unit's cgroup directory: where its slice says it is, else wherever
// systemd reports it. Results are cached while the unit lives. Sampler goroutine only.
func (s *Server) unitCgroupDir(unit string) string {
	m := &s.metrics
	if d, ok := m.cgPaths[unit]; ok {
		return d
	}
	var dir string
	if base := userManagerCgroup(); base != "" {
		if d := filepath.Join(base, sliceCgroupPath(s.cfg.Slice), unit); dirExists(d) {
			dir = d
		}
	}
	if dir == "" {
		if out, err := systemctl(context.Background(), "show", "-p", "ControlGroup", "--value", unit); err == nil && out != "" {
			dir = filepath.Join("/sys/fs/cgroup", out)
		}
	}
	if dir != "" {
		m.cgPaths[unit] = dir
	}
	return dir
}

func dirExists(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && fi.IsDir()
}

func cgroupFileInt(dir, name string) int64 {
	if dir == "" {
		return 0
	}
	b, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		return 0
	}
	v, _ := strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64)
	return v
}

// cgroupCPUUsec reads usage_usec from a cgroup's cpu.stat (it covers every descendant, so a
// podman container moved into a child cgroup is included).
func cgroupCPUUsec(dir string) (uint64, bool) {
	if dir == "" {
		return 0, false
	}
	b, err := os.ReadFile(filepath.Join(dir, "cpu.stat"))
	if err != nil {
		return 0, false
	}
	for _, line := range strings.Split(string(b), "\n") {
		if v, ok := strings.CutPrefix(line, "usage_usec "); ok {
			n, err := strconv.ParseUint(strings.TrimSpace(v), 10, 64)
			return n, err == nil
		}
	}
	return 0, false
}

// diskUsage reports the filesystems holding the state directory and container storage.
func (s *Server) diskUsage() []api.DiskUsage {
	var out []api.DiskUsage
	seen := map[uint64]bool{}
	add := func(name, path string) {
		var st unix.Statfs_t
		if path == "" || unix.Statfs(path, &st) != nil {
			return
		}
		var fi unix.Stat_t
		if unix.Stat(path, &fi) == nil {
			if seen[fi.Dev] {
				return // same filesystem as one already listed
			}
			seen[fi.Dev] = true
		}
		out = append(out, api.DiskUsage{Name: name, Path: path,
			Total: int64(st.Blocks) * st.Bsize, Free: int64(st.Bavail) * st.Bsize})
	}
	add("state", s.cfg.StateDir)
	if s.containersAvailable() {
		home, _ := os.UserHomeDir()
		dir := filepath.Join(home, ".local", "share", "containers", "storage")
		if d := os.Getenv("XDG_DATA_HOME"); d != "" {
			dir = filepath.Join(d, "containers", "storage")
		}
		add("containers", dir)
	}
	return out
}
