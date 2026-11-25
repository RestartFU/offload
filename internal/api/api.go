// Package api holds the wire types shared by the offload client and runner.
package api

import "time"

// DefaultPort is the TCP port a runner listens on inside the tailnet.
const DefaultPort = 7420

// Job states.
const (
	StateUploading = "uploading" // waiting for the client to send changed source files
	StateQueued    = "queued"    // source is synced; waiting for capacity
	StateRunning   = "running"
	StateExited    = "exited" // the command finished; see ExitCode
	StateFailed    = "failed" // killed (out of memory, runner restart, upload error…)
	StateCancelled = "cancelled"
)

// Terminal reports whether a job or session state is final.
func Terminal(state string) bool {
	return state == StateExited || state == StateFailed || state == StateCancelled
}

// Resources is what a job asks for. CPU is a guaranteed share, CPUMax a burst ceiling,
// Mem a hard memory limit in bytes. Zero values mean "use the profile or runner default".
type Resources struct {
	CPU    float64 `json:"cpu,omitempty"`
	CPUMax float64 `json:"cpu_max,omitempty"`
	Mem    int64   `json:"mem,omitempty"`
}

// FileEntry describes one workspace file in a source manifest.
type FileEntry struct {
	Path string `json:"path"`           // slash-separated, relative to the workspace root
	Hash string `json:"hash,omitempty"` // sha256 hex of the content (regular files)
	Mode uint32 `json:"mode"`           // permission bits
	Link string `json:"link,omitempty"` // symlink target; set for symlinks only
}

// JobSpec is the body of POST /v1/jobs.
type JobSpec struct {
	Argv      []string          `json:"argv"`
	Env       map[string]string `json:"env,omitempty"`
	Profile   string            `json:"profile,omitempty"`
	Resources Resources         `json:"resources"`
	// WorkspaceKey names the persistent workspace (repository) this job runs in. Empty means a
	// throwaway directory with no source sync.
	WorkspaceKey string      `json:"workspace_key,omitempty"`
	Manifest     []FileEntry `json:"manifest,omitempty"`
	Dir          string      `json:"dir,omitempty"` // working directory relative to the workspace root
	Artifacts    []string    `json:"artifacts,omitempty"`
	Fresh        bool        `json:"fresh,omitempty"`     // start from an empty workspace (cold build)
	Ephemeral    bool        `json:"ephemeral,omitempty"` // drop the workspace when the job ends
	Client       string      `json:"client,omitempty"`    // free-form label of the caller
}

// Job is a job record as returned by the runner.
type Job struct {
	ID          string     `json:"id"`
	Runner      string     `json:"runner"`
	State       string     `json:"state"`
	Spec        JobSpec    `json:"spec"`
	Resources   Resources  `json:"resources"` // effective, after defaults
	Slot        string     `json:"slot,omitempty"`
	Unit        string     `json:"unit,omitempty"`
	AllowedCPUs string     `json:"allowed_cpus,omitempty"`
	CreatedAt   time.Time  `json:"created_at"`
	StartedAt   *time.Time `json:"started_at,omitempty"`
	FinishedAt  *time.Time `json:"finished_at,omitempty"`
	ExitCode    *int       `json:"exit_code,omitempty"`
	Signal      string     `json:"signal,omitempty"`
	Reason      string     `json:"reason,omitempty"`
	HasArtifact bool       `json:"has_artifacts,omitempty"`
	// Needed lists the manifest paths the client must upload. Only set in the create response.
	Needed []string `json:"needed,omitempty"`
	// Manifest stats, for the create response.
	SyncFiles int `json:"sync_files,omitempty"`
}

// SessionSpec is the body of POST /v1/sessions.
type SessionSpec struct {
	Profile   string    `json:"profile"`
	Resources Resources `json:"resources"`
	Client    string    `json:"client,omitempty"`
}

// Session is a long-lived process (usually an MCP server) whose stdio is bridged over HTTP.
type Session struct {
	ID         string     `json:"id"`
	Runner     string     `json:"runner"`
	Profile    string     `json:"profile"`
	State      string     `json:"state"`
	Resources  Resources  `json:"resources"`
	Unit       string     `json:"unit,omitempty"`
	CreatedAt  time.Time  `json:"created_at"`
	LastSeen   time.Time  `json:"last_seen"`
	Attached   int        `json:"attached"`
	FinishedAt *time.Time `json:"finished_at,omitempty"`
	ExitCode   *int       `json:"exit_code,omitempty"`
	Reason     string     `json:"reason,omitempty"`
}

// ProfileInfo summarizes a runner profile in /v1/status.
type ProfileInfo struct {
	Kind        string    `json:"kind"` // "job" or "session"
	Description string    `json:"description,omitempty"`
	Resources   Resources `json:"resources"`
}

// Status is the body of GET /v1/status.
type Status struct {
	Name         string                 `json:"name"`
	Version      string                 `json:"version"`
	Hostname     string                 `json:"hostname"`
	Capacity     Resources              `json:"capacity"`  // CPU and Mem the runner hands out
	Allocated    Resources              `json:"allocated"` // sum of running jobs' and sessions' guarantees
	Defaults     Resources              `json:"defaults"`
	MemAvailable int64                  `json:"mem_available"` // host MemAvailable right now
	MemReserve   int64                  `json:"mem_reserve"`   // never admit below this MemAvailable
	Load1        float64                `json:"load1"`
	Running      int                    `json:"running"`
	Queued       int                    `json:"queued"`
	Sessions     int                    `json:"sessions"`
	WarmKeys     []string               `json:"warm_keys"` // workspace keys with a persistent slot
	Profiles     map[string]ProfileInfo `json:"profiles"`
	Persistent   bool                   `json:"persistent"`
	Features     []string               `json:"features"` // cgroup controllers in use: cpuset, io…
	Accepting    bool                   `json:"accepting"`
	Reason       string                 `json:"reason,omitempty"`
}

// Error is the JSON body of a non-2xx response.
type Error struct {
	Error string `json:"error"`
}
