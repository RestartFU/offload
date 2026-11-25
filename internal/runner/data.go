package runner

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/RestartFU/offload/internal/api"
)

// Data sets are directories the client keeps in sync on runners (game data, tools, agent code)
// and that sessions mount by name. They use the workspace sync machinery with one difference:
// files the runner changed are kept unless the client's copy changed too.

var dataLocks sync.Map // name -> *sync.Mutex

// dataSlot is a data set's slot. Clients name data sets "<client id>.<name>", so two laptops'
// sets of the same name never mix.
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
			// Deleted on the runner: restore it, unless the runner replaced a directory above it
			// with a file of its own (a migration): unchanged client data doesn't undo that.
			if parentsState(root, p) != parentsAliased {
				needed = append(needed, p)
			}
		}
	}
	// An interrupted apply may have left any marked path half-done: apply again to repair it.
	// A file the client changed below an ancestor the runner turned into a file: apply moves
	// that file aside and recreates the directory, so everything the client has below it must
	// come along, not only the changed file.
	aliased := map[string]bool{}
	changedEntries := append([]string(nil), needed...) // contents, and links/modes that changed
	for _, e := range manifest {
		if old, ok := rec.Files[e.Path]; !ok || old.Link != e.Link || (e.Link == "" && old.Mode != e.Mode) { // a link's mode means nothing
			changedEntries = append(changedEntries, e.Path)
		}
	}
	for _, p := range changedEntries {
		parts := strings.Split(p, "/")
		for i := 1; i < len(parts); i++ {
			anc := strings.Join(parts[:i], "/")
			if fi, err := root.Lstat(anc); err == nil && !fi.IsDir() {
				aliased[anc] = true
				break
			}
		}
	}
	if len(aliased) > 0 {
		have := map[string]bool{}
		for _, p := range needed {
			have[p] = true
		}
		for _, e := range manifest {
			if e.Link == "" && !have[e.Path] && underAny(e.Path, aliased) {
				needed = append(needed, e.Path)
				have[e.Path] = true
			}
		}
	}
	return needed, changed || len(needed) > 0 || len(rec.Dirty) > 0, nil
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
