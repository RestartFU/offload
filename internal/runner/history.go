package runner

import (
	"encoding/json"
	"log"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/RestartFU/offload/internal/api"
)

// The runner learns how much memory a command needs, so callers never have to say. Each
// (workspace, command) pair keeps its last few peaks; the next run asks for the largest of them
// plus headroom. An out-of-memory kill counts as a peak of twice the limit it hit.

type histEntry struct {
	Peaks   []int64   `json:"peaks"` // newest last
	OOMs    int       `json:"ooms,omitempty"`
	Updated time.Time `json:"updated"`
}

const histKeep = 5

var subcommandRE = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,30}$`)

func (s *Server) historyPath() string { return filepath.Join(s.cfg.StateDir, "history.json") }

func (s *Server) loadHistory() {
	s.history = map[string]*histEntry{}
	if b, err := os.ReadFile(s.historyPath()); err == nil {
		_ = json.Unmarshal(b, &s.history)
	}
}

// commandSig names a command for history: the program plus its subcommand ("cargo test").
// Shell wrappers are looked through: `sh -c "cargo build && …"` is "cargo build".
func commandSig(argv []string) string {
	words := effectiveArgv(argv)
	if len(words) == 0 {
		return ""
	}
	sig := path.Base(words[0])
	for _, w := range words[1:] {
		if strings.HasPrefix(w, "-") || strings.HasPrefix(w, "+") {
			continue
		}
		if subcommandRE.MatchString(w) { // "cargo test", but not `python3 -c 'big script'`
			return sig + " " + w
		}
		return sig
	}
	return sig
}

// effectiveArgv returns the command a shell wrapper would run first, minus VAR=value prefixes.
func effectiveArgv(argv []string) []string {
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

func histKey(spec api.JobSpec) string {
	k := spec.WorkspaceKey
	if k == "" {
		k = "-"
	}
	return k + "|" + commandSig(spec.Argv)
}

// suggestMemLocked returns a memory limit learned from previous runs, or 0.
func (s *Server) suggestMemLocked(spec api.JobSpec) int64 {
	e := s.history[histKey(spec)]
	if e == nil || len(e.Peaks) == 0 {
		return 0
	}
	var peak int64
	for _, p := range e.Peaks {
		peak = max(peak, p)
	}
	const step = 256 << 20
	want := peak + peak/4 // 25% headroom
	want = (want + step - 1) / step * step
	return min(max(want, 512<<20), s.cfg.memCap)
}

// learn records a finished job's peak memory (or its OOM) for future sizing.
func (s *Server) learn(j *job, rec *exitRecord) {
	key := histKey(j.Spec)
	e := s.history[key]
	if e == nil {
		e = &histEntry{}
		s.history[key] = e
	}
	peak := rec.PeakMem
	if rec.OOM {
		e.OOMs++
		peak = max(peak, j.Resources.Mem*2)
	}
	if peak <= 0 {
		return
	}
	e.Peaks = append(e.Peaks, peak)
	if len(e.Peaks) > histKeep {
		e.Peaks = e.Peaks[len(e.Peaks)-histKeep:]
	}
	e.Updated = time.Now()
	for k, v := range s.history {
		if time.Since(v.Updated) > 60*24*time.Hour {
			delete(s.history, k)
		}
	}
	if err := writeFileJSON(s.historyPath(), s.history); err != nil {
		log.Printf("history: %v", err)
	}
}

// matchProfile picks the job profile whose `match` globs cover the command, e.g. cargo -> "cargo".
func matchProfile(profiles map[string]*Profile, argv []string) *Profile {
	words := effectiveArgv(argv)
	if len(words) == 0 {
		return nil
	}
	cmd := path.Base(words[0])
	var best *Profile
	for _, p := range profiles {
		if p.Kind != "job" {
			continue
		}
		for _, m := range p.Match {
			if ok, _ := path.Match(m, cmd); ok && (best == nil || p.Name < best.Name) {
				best = p
			}
		}
	}
	return best
}
