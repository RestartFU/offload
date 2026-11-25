package api

import "time"

// Metrics is the body of GET /v1/metrics: what the host and every workload use right now,
// sampled every MetricsStep.
type Metrics struct {
	At           time.Time       `json:"at"`
	CPUs         []CPUUsage      `json:"cpus"`
	MemTotal     int64           `json:"mem_total"`
	MemAvailable int64           `json:"mem_available"`
	Workloads    []WorkloadUsage `json:"workloads"`
	Disk         []DiskUsage     `json:"disk,omitempty"`
}

// MetricsStep is how often a runner samples usage.
const MetricsStep = 5 * time.Second

// CPUUsage is one host CPU. Busy is the fraction of the last sample interval it spent working.
// Reserved CPUs are kept for the system and never given to jobs. Owners are the jobs and
// sessions pinned to it (cpuset).
type CPUUsage struct {
	ID       int      `json:"id"`
	Busy     float64  `json:"busy"`
	Reserved bool     `json:"reserved,omitempty"`
	Owners   []string `json:"owners,omitempty"`
}

// WorkloadUsage is a running job's or session's live usage, from its unit cgroup. CPU is in
// cores (1.5 = one and a half cores busy over the last interval).
type WorkloadUsage struct {
	ID      string  `json:"id"`
	Kind    string  `json:"kind"` // "job" or "session"
	CPU     float64 `json:"cpu"`
	Mem     int64   `json:"mem"`
	MemPeak int64   `json:"mem_peak,omitempty"`
}

// DiskUsage is the filesystem holding one of the runner's directories.
type DiskUsage struct {
	Name  string `json:"name"` // "state", "containers"
	Path  string `json:"path"`
	Total int64  `json:"total"`
	Free  int64  `json:"free"`
}

// MetricPoint is one sample (or a one-minute average) of a runner's usage. CPU values are in
// cores: Busy is the whole host, Used offload's workloads, Alloc their guarantees.
type MetricPoint struct {
	T        int64   `json:"t"` // unix seconds
	CPUBusy  float64 `json:"cpu_busy"`
	CPUUsed  float64 `json:"cpu_used"`
	CPUAlloc float64 `json:"cpu_alloc"`
	MemUsed  int64   `json:"mem_used"`  // host MemTotal - MemAvailable
	MemWork  int64   `json:"mem_work"`  // offload workloads' memory.current
	MemAlloc int64   `json:"mem_alloc"` // their limits
	Running  float64 `json:"running"`
	Queued   float64 `json:"queued"`
	Sessions float64 `json:"sessions"`
	N        int     `json:"n,omitempty"` // samples averaged into this point (0 means 1)
}

// Series is the body of GET /v1/metrics/series?range=1h: points Step seconds apart, oldest first.
type Series struct {
	Step   int           `json:"step"`
	Points []MetricPoint `json:"points"`
}

// LedgerEntry is one finished job or session, kept for statistics after its record is gone
// (GET /v1/ledger?since=RFC3339). Durations are in seconds.
type LedgerEntry struct {
	ID         string     `json:"id"`
	Kind       string     `json:"kind"`    // "job" or "session"
	Command    string     `json:"command"` // "cargo test"; a session's profile
	Workspace  string     `json:"workspace,omitempty"`
	Profile    string     `json:"profile,omitempty"`
	Client     string     `json:"client,omitempty"`
	Caller     string     `json:"caller,omitempty"`
	Runtime    string     `json:"runtime,omitempty"`
	State      string     `json:"state"`
	ExitCode   *int       `json:"exit_code,omitempty"`
	Reason     string     `json:"reason,omitempty"`
	OOM        bool       `json:"oom,omitempty"`
	CreatedAt  time.Time  `json:"created_at"`
	StartedAt  *time.Time `json:"started_at,omitempty"`
	FinishedAt time.Time  `json:"finished_at"`
	QueueWait  float64    `json:"queue_wait"`
	Duration   float64    `json:"duration"`
	Resources  Resources  `json:"resources"`
	MemFrom    string     `json:"mem_from,omitempty"`
	PeakMem    int64      `json:"peak_mem,omitempty"`
	CPUSeconds float64    `json:"cpu_seconds,omitempty"`
	SyncFiles  int        `json:"sync_files,omitempty"`
	Uploaded   int        `json:"uploaded,omitempty"`
}
