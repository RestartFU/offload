// Package api holds the wire types shared by the offload client and runner.
package api

import (
	"path"
	"strings"
	"time"
)

// DefaultPort is the TCP port a runner listens on inside the tailnet.
const DefaultPort = 7420

// Proto versions the runner protocol and how workloads are wired (mounts, relay sockets, the
// stdin wrapper). A client only uses runners of its own version (`offload host upgrade` brings
// them in line), and a runner stops workloads started by another version when it starts.
const Proto = 8

// ProtoHeader carries the client's protocol version on every request; runners refuse other
// versions (except for status, pairing and upgrades, which is how versions get back in step).
const ProtoHeader = "X-Offload-Proto"

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
	// Git gives the workspace a real .git at the client's HEAD. The runner keeps the history per
	// repository; the client uploads only missing commits as a git bundle.
	Git *GitRef `json:"git,omitempty"`
	// Relay lists credential relays the job may use while a client is attached:
	// "ssh-agent" (SSH_AUTH_SOCK) and "git" (credential helper). Answers come from the client.
	Relay []string `json:"relay,omitempty"`
	// Secrets (env name -> value) and SecretFiles (env name -> file content; the env var gets the
	// file's path) exist only while the job runs, in tmpfs. The runner never stores them.
	Secrets     map[string]string `json:"secrets,omitempty"`
	SecretFiles map[string][]byte `json:"secret_files,omitempty"`
	// Environment is the container environment to run in (nil = the runner's default base).
	Environment *EnvDef `json:"environment,omitempty"`
	// Runtime forces "host" or "container"; empty lets the runner decide (containers if available).
	Runtime string `json:"runtime,omitempty"`
	// Network "client" routes the job's connections through the attached client (its egress IP,
	// DNS, LAN, tailnet; 10.233.0.1 is the client's localhost). Empty means the runner's network.
	Network string `json:"network,omitempty"`
	// DNSSearch is the client's DNS search list (e.g. its tailnet domain), used with Network "client".
	DNSSearch []string `json:"dns_search,omitempty"`
	// Platform requires a runner OS/arch such as "linux/arm64"; empty means any.
	Platform string `json:"platform,omitempty"`
	// GitConfig is git configuration mirrored from the client (URL rewrites, identity), applied
	// to the job through GIT_CONFIG_* so git on the runner behaves like git on the client.
	GitConfig [][2]string `json:"git_config,omitempty"`
	// Detached means no client will stay attached, so relay requests fail at once.
	Detached bool `json:"detached,omitempty"`
	// SecretNames records which secrets a job got (names only); set by the runner.
	SecretNames []string `json:"secret_names,omitempty"`
	// WriteBack asks the runner to return synced files the job changed, so the client can
	// apply them locally (cargo fmt, lockfile updates, codegen).
	WriteBack bool   `json:"write_back,omitempty"`
	Fresh     bool   `json:"fresh,omitempty"`     // start from an empty workspace (cold build)
	Ephemeral bool   `json:"ephemeral,omitempty"` // drop the workspace when the job ends
	Client    string `json:"client,omitempty"`    // free-form label of the caller
	// GitTree says the manifest is git's view of the tree: then anything else in the slot that
	// git wouldn't ignore was made by an earlier job and is removed before this one runs.
	GitTree bool `json:"git_tree,omitempty"`
	// IgnoreRules and IgnoreGlobal are the client's ignore rules that don't travel in the tree
	// (.git/info/exclude; core.excludesFile), in gitignore syntax: pruning honors them too,
	// ranked as git ranks them.
	// SubmitID is chosen by the client: submitting the same spec again (after a lost answer)
	// returns the job it already created instead of starting a second one.
	SubmitID     string `json:"submit_id,omitempty"`
	IgnoreRules  string `json:"ignore_rules,omitempty"`
	IgnoreGlobal string `json:"ignore_global,omitempty"`
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
	// ArtifactsMissing lists requested artifact paths the job didn't produce (or that couldn't
	// be read): the archive, if any, holds the rest.
	ArtifactsMissing []string `json:"artifacts_missing,omitempty"`
	// WriteBackError says the runner couldn't collect the job's edits (none were sent back).
	WriteBackError string     `json:"write_back_error,omitempty"`
	WriteBack      *WriteBack `json:"write_back,omitempty"`
	// PeakMem is the job's peak memory use (cgroup memory.peak), once it has finished.
	PeakMem int64 `json:"peak_mem,omitempty"`
	// AutoMem is set when the runner chose the memory limit (from history or the profile)
	// rather than the caller; clients may retry such a job with more memory after an OOM.
	AutoMem bool `json:"auto_mem,omitempty"`
	// Runtime is "container" or "host"; Image is the environment image when containerized.
	Runtime string `json:"runtime,omitempty"`
	Image   string `json:"image,omitempty"`
	// ContextDigest identifies the workspace content a COPY/ADD environment is built from, so
	// only jobs with identical build contexts share an image.
	ContextDigest string `json:"context_digest,omitempty"`
	// Proto is the runner protocol version that started the job (mounts, relays, wrappers).
	Proto int `json:"proto,omitempty"`
	// StopRequested records a cancel before it is acknowledged, so a runner restart re-sends it.
	StopRequested bool `json:"stop_requested,omitempty"`
	// MemFrom says where the memory limit came from: "requested", "learned", "profile", "default".
	MemFrom string `json:"mem_from,omitempty"`
	// MemBooked is the memory admission counts for the job: the most its command used in recent
	// runs, at most its limit. 0 means the whole limit (no history, or an older runner).
	MemBooked int64 `json:"mem_booked,omitempty"`
	// Caller is the authenticated identity that created the job (tailnet login or tags, "token",
	// or "local"), set by the runner. Spec.Client is the caller's own free-form label.
	Caller string `json:"caller,omitempty"`
	// Uploaded is how many of SyncFiles the client had to send (the rest were already warm),
	// once the sync has succeeded. Absent before that, or from an older runner.
	Uploaded *int `json:"uploaded,omitempty"`
	// CPUSeconds is the CPU time the job used, once it has finished.
	CPUSeconds float64 `json:"cpu_seconds,omitempty"`
	// Needed lists the manifest paths the client must upload. Only set in the create response.
	Needed []string `json:"needed,omitempty"`
	// Manifest stats, for the create response.
	SyncFiles int `json:"sync_files,omitempty"`
	// GitNeeded means the runner lacks Spec.Git.Head: upload a bundle of HEAD minus GitHave.
	GitNeeded bool     `json:"git_needed,omitempty"`
	GitHave   []string `json:"git_have,omitempty"`
}

// EnvDef is a job environment: a Containerfile, usually starting `FROM offload/base`, which the
// runner builds (layer-cached) and runs the job in. ContextDir (relative to the workspace root)
// is the build context for COPY/ADD; empty means no context.
type EnvDef struct {
	Name          string            `json:"name"` // display name, e.g. ".offload/Containerfile"
	Containerfile string            `json:"containerfile"`
	ContextDir    string            `json:"context_dir,omitempty"`
	Env           map[string]string `json:"env,omitempty"`     // extra environment (devcontainer containerEnv)
	Devices       []string          `json:"devices,omitempty"` // e.g. /dev/dri
}

// GitRef is the client's checkout: commit and branch (empty when detached).
type GitRef struct {
	Head   string `json:"head"`
	Branch string `json:"branch,omitempty"`
	// Tags reachable from Head (name -> object id), so `git describe` works on the runner.
	Tags map[string]string `json:"tags,omitempty"`
}

// WriteBack lists what a finished job changed in its workspace. Paths are relative to the
// workspace root. The files themselves are served by GET /v1/jobs/{id}/writeback as tar.gz.
type WriteBack struct {
	Modified []ChangedFile `json:"modified,omitempty"` // synced files the job edited
	Deleted  []string      `json:"deleted,omitempty"`  // synced files the job removed (reported only)
}

// ChangedFile is one file in a WriteBack. BaseHash is the content the client uploaded, so the
// client can refuse to overwrite a file that also changed locally.
type ChangedFile struct {
	Path     string `json:"path"`
	Hash     string `json:"hash"`
	BaseHash string `json:"base_hash,omitempty"`
	Mode     uint32 `json:"mode"`
	Size     int64  `json:"size"`
	// Link is set when the job left a symlink here (pointing at Link; no content in the
	// archive); BaseLink when the path was a symlink before the job (BaseHash otherwise).
	Link     string `json:"link,omitempty"`
	BaseLink string `json:"base_link,omitempty"`
}

// SessionSpec is the body of POST /v1/sessions.
type SessionSpec struct {
	Profile     string            `json:"profile"`
	Resources   Resources         `json:"resources"`
	Client      string            `json:"client,omitempty"`
	Secrets     map[string]string `json:"secrets,omitempty"` // as in JobSpec
	SecretFiles map[string][]byte `json:"secret_files,omitempty"`
	Environment *EnvDef           `json:"environment,omitempty"` // overrides the profile's
	Runtime     string            `json:"runtime,omitempty"`     // "host" or "container"
	DNSSearch   []string          `json:"dns_search,omitempty"`  // the client's search list, for network = "client"
	// Def is a session defined on the client (~/.config/offload/sessions/NAME.toml); when set,
	// the runner needs no profile of that name.
	Def     *SessionDef `json:"def,omitempty"`
	NoStdin bool        `json:"no_stdin,omitempty"` // the command's stdin is at EOF from the start (mcp --prepare)
}

// SessionDef is a client-side session definition. Data maps synced data sets (by name) to
// container paths; Volumes maps runner-side persistent volumes (by name) to container paths.
type SessionDef struct {
	Command       []string          `json:"command"`
	Env           map[string]string `json:"env,omitempty"`
	Data          map[string]string `json:"data,omitempty"`
	Volumes       map[string]string `json:"volumes,omitempty"`
	Devices       []string          `json:"devices,omitempty"`
	NestedSandbox bool              `json:"nested_sandbox,omitempty"` // allow bubblewrap/flatpak inside
	Network       string            `json:"network,omitempty"`        // "client": as in JobSpec.Network
	DNSSearch     []string          `json:"dns_search,omitempty"`
}

// DataPlan is the body of POST /v1/data/{name}: a data set's manifest. The runner answers with
// the paths to upload (PUT /v1/data/{name}). Files the runner changed itself (a refreshed token,
// game settings) are kept unless the client's copy changed since the last sync.
type DataPlan struct {
	Manifest []FileEntry `json:"manifest"`
}

// Session is a long-lived process (usually an MCP server) whose stdio is bridged over HTTP.
type Session struct {
	ID            string     `json:"id"`
	Runner        string     `json:"runner"`
	Profile       string     `json:"profile"`
	State         string     `json:"state"`
	Resources     Resources  `json:"resources"`
	Unit          string     `json:"unit,omitempty"`
	Client        string     `json:"client,omitempty"` // the caller's free-form label
	Caller        string     `json:"caller,omitempty"` // authenticated identity, as in Job
	CreatedAt     time.Time  `json:"created_at"`
	LastSeen      time.Time  `json:"last_seen"`
	Attached      int        `json:"attached"`
	Runtime       string     `json:"runtime,omitempty"`
	Image         string     `json:"image,omitempty"`
	Network       string     `json:"network,omitempty"`        // "client": the attached client must serve dial callbacks
	Proto         int        `json:"proto,omitempty"`          // runner protocol version that started it
	StopRequested bool       `json:"stop_requested,omitempty"` // stop asked for; re-sent after a restart
	FinishedAt    *time.Time `json:"finished_at,omitempty"`
	ExitCode      *int       `json:"exit_code,omitempty"`
	Reason        string     `json:"reason,omitempty"`
	DataSets      []string   `json:"data_sets,omitempty"` // data sets it has mounted (never changed under it)
	PeakMem       int64      `json:"peak_mem,omitempty"`  // once finished: cgroup memory.peak
	CPUSeconds    float64    `json:"cpu_seconds,omitempty"`
}

// ProfileInfo summarizes a runner profile in /v1/status.
type ProfileInfo struct {
	Kind        string    `json:"kind"`            // "job" or "session"
	Match       []string  `json:"match,omitempty"` // commands that select this profile automatically
	Description string    `json:"description,omitempty"`
	Resources   Resources `json:"resources"`
}

// Status is the body of GET /v1/status.
type Status struct {
	ID        string    `json:"id,omitempty"` // stable per runner (machine and state dir)
	Name      string    `json:"name"`
	Version   string    `json:"version"`
	Hostname  string    `json:"hostname"`
	Capacity  Resources `json:"capacity"`  // CPU and Mem the runner hands out
	Allocated Resources `json:"allocated"` // guarantees of running work; Mem is booked, not limits
	Pending   Resources `json:"pending"`   // guarantees of jobs still uploading or queued
	Requeue   bool      `json:"requeue,omitempty"`
	// CPU overbooking: guarantees may add up to Capacity.CPU*CPUOvercommit, but past capacity a
	// job only starts while CPUIdle (the cores really idle now) covers its guarantee.
	CPUOvercommit float64 `json:"cpu_overcommit,omitempty"`
	CPUIdle       float64 `json:"cpu_idle,omitempty"` // DELETE /v1/jobs/{id}?moved=1 cancels only a queued job
	// MemRoom is the memory a job could be given now: by bookings, the limit on overbooking and
	// what the host has left once running jobs grow to their bookings. Pending jobs not counted.
	MemRoom      *int64                 `json:"mem_room,omitempty"`
	Defaults     Resources              `json:"defaults"`
	MemAvailable int64                  `json:"mem_available"` // host MemAvailable right now
	MemReserve   int64                  `json:"mem_reserve"`   // never admit below this MemAvailable
	Load1        float64                `json:"load1"`
	Running      int                    `json:"running"`
	Queued       int                    `json:"queued"`
	Sessions     int                    `json:"sessions"`
	WarmKeys     []string               `json:"warm_keys"`           // workspace keys with a persistent slot
	DataSets     []string               `json:"data_sets,omitempty"` // data sets synced to this runner
	Profiles     map[string]ProfileInfo `json:"profiles"`
	Persistent   bool                   `json:"persistent"`
	OS           string                 `json:"os"`
	Arch         string                 `json:"arch"`
	Runtime      string                 `json:"runtime"` // default runtime: "container" or "host"
	Images       int                    `json:"images,omitempty"`
	Features     []string               `json:"features"` // cgroup controllers in use: cpuset, io…
	Accepting    bool                   `json:"accepting"`
	Proto        int                    `json:"proto"`
	Reason       string                 `json:"reason,omitempty"`
	Started      time.Time              `json:"started"`             // when this runner process started
	HostCPUs     int                    `json:"host_cpus,omitempty"` // every CPU on the host, offload's or not
	Metrics      bool                   `json:"metrics,omitempty"`   // serves /v1/metrics and /v1/ledger
}

// Error is the JSON body of a non-2xx response.
type Error struct {
	Error string `json:"error"`
}

// AgentRequestAllowed reports whether a framed SSH agent message is one a job may relay to the
// user's agent: SSH_AGENTC_REQUEST_IDENTITIES (11) and SSH_AGENTC_SIGN_REQUEST (13) only. Adding
// or removing keys, locking, and extensions never leave the runner.
func AgentRequestAllowed(msg []byte) bool {
	if len(msg) < 5 || int(msg[0])<<24|int(msg[1])<<16|int(msg[2])<<8|int(msg[3]) != len(msg)-4 {
		return false
	}
	return msg[4] == 11 || msg[4] == 13
}

// SkipDir reports whether a directory of a tree that isn't a git repository is build output the
// client never syncs (and the runner never prunes).
func SkipDir(name string) bool {
	switch name {
	case ".git", "target", "node_modules", ".venv", "__pycache__", ".zig-cache", "zig-out", "build", "dist":
		return true
	}
	return strings.HasPrefix(name, ".cache")
}

// CommandWords is the command a job's argv really runs: past "sh -c '…'" (its first command)
// and leading VAR=value assignments. Profiles match on its first word.
func CommandWords(argv []string) []string {
	if len(argv) >= 3 {
		switch path.Base(argv[0]) {
		case "sh", "bash", "zsh", "dash":
			for i := 1; i+1 < len(argv); i++ {
				if argv[i] == "-c" || argv[i] == "-lc" || argv[i] == "-ec" {
					first := strings.FieldsFunc(argv[i+1], func(r rune) bool { return r == ';' || r == '&' || r == '|' || r == '\n' })
					if len(first) > 0 {
						argv = strings.Fields(first[0])
					}
					break
				}
			}
		}
	}
	for len(argv) > 0 && strings.Contains(argv[0], "=") && !strings.HasPrefix(argv[0], "-") {
		argv = argv[1:]
	}
	return argv
}

// MatchProfile is the job profile a runner picks for argv when none is named ("" if none):
// the alphabetically first job profile whose match globs fit the command's base name.
func MatchProfile(profiles map[string]ProfileInfo, argv []string) string {
	words := CommandWords(argv)
	if len(words) == 0 {
		return ""
	}
	cmd := path.Base(words[0])
	best := ""
	for name, p := range profiles {
		if p.Kind != "job" {
			continue
		}
		for _, m := range p.Match {
			if ok, _ := path.Match(m, cmd); ok && (best == "" || name < best) {
				best = name
			}
		}
	}
	return best
}
