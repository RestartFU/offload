package runner

import (
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sync"

	"github.com/RestartFU/offload/internal/api"
)

// Data sets are directories the client keeps in sync on runners (game data, tools, agent code)
// and that sessions mount by name. They use the workspace sync machinery with one difference:
// files the runner changed are kept unless the client's copy changed too.

var dataLocks sync.Map // name -> *sync.Mutex

func (s *Server) dataSlot(name string) *slot {
	dir := filepath.Join(s.cfg.StateDir, "data", name)
	_ = os.MkdirAll(dir, 0o755)
	return &slot{Key: name, Dir: dir}
}

func dataLock(name string) *sync.Mutex {
	m, _ := dataLocks.LoadOrStore(name, &sync.Mutex{})
	return m.(*sync.Mutex)
}

// planData lists manifest paths whose content differs from what was last synced. Unlike a
// workspace, local changes on the runner don't trigger a re-upload.
func (sl *slot) planData(manifest []api.FileEntry) ([]string, error) {
	rec := sl.load()
	var needed []string
	for _, e := range manifest {
		p, err := cleanRel(e.Path)
		if err != nil {
			return nil, err
		}
		if e.Link != "" {
			continue
		}
		old, ok := rec.Files[p]
		if !ok || old.Hash != e.Hash {
			needed = append(needed, p)
			continue
		}
		if _, err := os.Lstat(filepath.Join(sl.Dir, filepath.FromSlash(p))); err != nil {
			needed = append(needed, p) // deleted on the runner: restore it
		}
	}
	return needed, nil
}

func (s *Server) handleDataPlan(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if !validKey(name) {
		writeErr(w, 400, "invalid data set name")
		return
	}
	var plan api.DataPlan
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 256<<20)).Decode(&plan); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	needed, err := s.dataSlot(name).planData(plan.Manifest)
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	writeJSON(w, map[string]any{"needed": needed})
}

// handleDataUpload applies a manifest (multipart: "manifest" JSON, then "files" tar.gz).
func (s *Server) handleDataUpload(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if !validKey(name) {
		writeErr(w, 400, "invalid data set name")
		return
	}
	mu := dataLock(name)
	mu.Lock()
	defer mu.Unlock()
	mr, err := r.MultipartReader()
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	var plan api.DataPlan
	var needed []string
	sl := s.dataSlot(name)
	for {
		part, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			writeErr(w, 400, err.Error())
			return
		}
		switch part.FormName() {
		case "manifest":
			if err := json.NewDecoder(io.LimitReader(part, 256<<20)).Decode(&plan); err != nil {
				writeErr(w, 400, err.Error())
				return
			}
			if needed, err = sl.planData(plan.Manifest); err != nil {
				writeErr(w, 400, err.Error())
				return
			}
		case "files":
			if err := sl.apply(plan.Manifest, needed, part); err != nil {
				writeErr(w, 400, "data sync: "+err.Error())
				return
			}
			writeJSON(w, map[string]any{"synced": len(needed), "files": len(plan.Manifest)})
			return
		}
	}
	writeErr(w, 400, "missing files part")
}

func (s *Server) dataPath(name string) string { return filepath.Join(s.cfg.StateDir, "data", name) }

func (s *Server) volumePath(name string) string {
	p := filepath.Join(s.cfg.StateDir, "volumes", name)
	_ = os.MkdirAll(p, 0o755)
	return p
}
