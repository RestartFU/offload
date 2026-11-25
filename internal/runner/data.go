package runner

import (
	"encoding/json"
	"fmt"
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
// changed reports manifest-only differences (deletions, symlinks, modes) that need an apply
// even when no file content has to be uploaded.
func (sl *slot) planData(manifest []api.FileEntry) (needed []string, changed bool, err error) {
	rec := sl.load()
	root, err := sl.openTree()
	if err != nil {
		return nil, false, err
	}
	defer root.Close()
	seen, dirty := map[string]bool{}, rec.dirtySet()
	for _, e := range manifest {
		seen[e.Path] = true
		old, ok := rec.Files[e.Path]
		if !ok || old.Link != e.Link || (e.Link == "" && old.Mode != e.Mode) {
			changed = true
		}
	}
	for p := range rec.Files {
		if !seen[p] {
			changed = true
		}
	}
	for _, e := range manifest {
		p, perr := cleanRel(e.Path)
		if perr != nil {
			return nil, false, perr
		}
		if e.Link != "" {
			continue
		}
		old, ok := rec.Files[p]
		if !ok || old.Hash != e.Hash || dirty[p] {
			needed = append(needed, p)
			continue
		}
		if _, err := root.Lstat(p); err != nil {
			needed = append(needed, p) // deleted on the runner: restore it
		}
	}
	return needed, changed || len(needed) > 0, nil
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
	sl := s.dataSlot(name)
	rec, err := sl.loadErr()
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	needed, changed, err := sl.planData(plan.Manifest)
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	writeJSON(w, map[string]any{"needed": needed, "changed": changed, "rev": rec.Rev})
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
	r.Body = struct {
		io.Reader
		io.Closer
	}{uploadBody(w, r, 64<<30, nil), r.Body}
	mr, err := r.MultipartReader()
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	var plan api.DataPlan
	var needed []string
	revOK := false
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
			if needed, _, err = sl.planData(plan.Manifest); err != nil {
				writeErr(w, 400, err.Error())
				return
			}
		case "rev":
			// The upload must answer the current plan: another client may have synced since.
			var rev int64
			b, _ := io.ReadAll(io.LimitReader(part, 32))
			rec, err := sl.loadErr()
			if _, perr := fmt.Sscan(string(b), &rev); perr != nil || err != nil || rev != rec.Rev {
				writeErr(w, http.StatusConflict, "data set changed since this plan; plan again")
				return
			}
			revOK = true
		case "files":
			if !revOK {
				writeErr(w, http.StatusConflict, "missing plan revision; plan again")
				return
			}
			if err := sl.applyMode(plan.Manifest, needed, part, true); err != nil {
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
