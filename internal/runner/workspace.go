package runner

import (
	"archive/tar"
	"compress/gzip"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/RestartFU/offload/internal/api"
)

// slot is one persistent copy of a workspace. A workspace key can have several slots so that
// concurrent jobs on the same repository never share a build directory.
type slot struct {
	Key      string `json:"key"`
	Index    int    `json:"index"`
	Dir      string `json:"-"`
	LastUsed time.Time
	inUse    string // job or session id holding it

	// leases counts environment builds reading this slot as their context. Its job may end
	// meanwhile; the slot is only released once no build reads it (releaseAfterBuild).
	leases            int
	releaseAfterBuild bool
	dropAfterBuild    bool // the job that finished was ephemeral (or storage isn't persistent)
}

func (s *slot) id() string { return fmt.Sprintf("%s/%d", s.Key, s.Index) }

// slotRecord is what a slot remembers about the files it holds.
type slotRecord struct {
	Key      string               `json:"key"`
	LastUsed time.Time            `json:"last_used"`
	Rev      int64                `json:"rev"` // bumped on every save: plans are only valid against their revision
	Files    map[string]slotEntry `json:"files"`
	// Dirty lists files an apply was replacing when it was interrupted (recorded before the first
	// rename): their recorded metadata can't be trusted, so the next plan always re-requests them.
	Dirty []string `json:"dirty,omitempty"`
	// Stages are staging directories an apply created; an interrupted apply leaves them behind and
	// the next one removes exactly those (never anything else that merely looks like one).
	Stages []string `json:"stages,omitempty"`
}

func (r slotRecord) dirtySet() map[string]bool {
	m := map[string]bool{}
	for _, p := range r.Dirty {
		m[p] = true
	}
	return m
}

type slotEntry struct {
	Hash  string `json:"hash,omitempty"`
	Mode  uint32 `json:"mode"`
	Link  string `json:"link,omitempty"`
	Size  int64  `json:"size"`
	MTime int64  `json:"mtime"` // unix nanoseconds after we wrote it
	CTime int64  `json:"ctime"` // inode change time: no job can set it back, unlike mtime
}

var keyRE = regexp.MustCompile(`^[A-Za-z0-9._-]{1,100}$`)

func validKey(k string) bool { return keyRE.MatchString(k) && k != "." && k != ".." }

func (s *slot) recordPath() string { return s.Dir + ".json" }

func (s *slot) load() slotRecord {
	rec, _ := s.loadErr()
	return rec
}

// loadErr reads the slot record; a missing record is an empty slot, an unreadable one an error.
func (s *slot) loadErr() (slotRecord, error) {
	rec := slotRecord{Files: map[string]slotEntry{}}
	b, err := os.ReadFile(s.recordPath())
	if errors.Is(err, os.ErrNotExist) {
		return rec, nil
	}
	if err == nil {
		err = json.Unmarshal(b, &rec)
	}
	if rec.Files == nil {
		rec.Files = map[string]slotEntry{}
	}
	return rec, err
}

func (s *slot) save(rec slotRecord) error {
	rec.Key = s.Key
	rec.LastUsed = s.LastUsed
	rec.Rev++
	b, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	tmp := s.recordPath() + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.recordPath())
}

// wipe removes the slot's files and record, leaving an empty directory.
func (s *slot) wipe() error {
	_ = os.Remove(s.recordPath())
	if err := removeAll(s.Dir); err != nil {
		return err
	}
	return os.MkdirAll(s.Dir, 0o755)
}

// cleanRel validates a manifest path and returns it in clean slash form.
func cleanRel(p string) (string, error) {
	if p == "" || strings.HasPrefix(p, "/") || strings.Contains(p, "\x00") || strings.Contains(p, "\\") {
		return "", fmt.Errorf("invalid path %q", p)
	}
	c := path.Clean(p)
	if c == "." || c == ".." || strings.HasPrefix(c, "../") || c != p {
		return "", fmt.Errorf("invalid path %q", p)
	}
	return c, nil
}

// openTree opens the slot as an os.Root: every operation below is confined to it, so a symlink a
// previous job left behind (say, a parent directory pointing at /etc) can never redirect a write,
// rename or delete outside the workspace.
func (s *slot) openTree() (*os.Root, error) {
	if err := os.MkdirAll(s.Dir, 0o755); err != nil {
		return nil, err
	}
	return os.OpenRoot(s.Dir)
}

// prepareParents makes every parent of rel a real directory, replacing files or symlinks that
// stand where a directory must be.
func prepareParents(root *os.Root, rel string) error {
	parts := strings.Split(rel, "/")
	for i := 1; i < len(parts); i++ {
		dir := strings.Join(parts[:i], "/")
		fi, err := root.Lstat(dir)
		switch {
		case errors.Is(err, os.ErrNotExist):
			if err := root.Mkdir(dir, 0o755); err != nil {
				return err
			}
		case err != nil:
			return err
		case !fi.IsDir():
			if err := root.RemoveAll(dir); err != nil { // removes a symlink itself, never its target
				return err
			}
			if err := root.Mkdir(dir, 0o755); err != nil {
				return err
			}
		}
	}
	return nil
}

// plan compares a client manifest with what the slot holds and returns the paths the client
// must upload. Files whose recorded size or mtime changed since we wrote them (a previous job
// edited them) are re-requested.
func (s *slot) plan(manifest []api.FileEntry) ([]string, error) {
	rec := s.load()
	root, err := s.openTree()
	if err != nil {
		return nil, err
	}
	defer root.Close()
	var needed []string
	seen, dirty := map[string]bool{}, rec.dirtySet()
	for _, e := range manifest {
		p, err := cleanRel(e.Path)
		if err != nil {
			return nil, err
		}
		if seen[p] {
			return nil, fmt.Errorf("duplicate path %q", p)
		}
		seen[p] = true
		if e.Link != "" {
			continue // symlinks travel in the manifest itself
		}
		old, ok := rec.Files[p]
		if !ok || old.Hash != e.Hash || old.Link != "" || dirty[p] {
			needed = append(needed, p)
			continue
		}
		fi, err := root.Lstat(p)
		if err != nil || !fi.Mode().IsRegular() || fi.Size() != old.Size || fi.ModTime().UnixNano() != old.MTime || ctimeOf(fi) != old.CTime || !realParents(root, p) {
			needed = append(needed, p)
		}
	}
	return needed, nil
}

// apply brings the slot in line with the manifest: removes files that left the manifest,
// extracts the uploaded files (verifying their hashes), recreates symlinks and fixes modes.
// Untracked build output (target/, node_modules/…) is left alone; that's the point of a slot.
func (s *slot) apply(manifest []api.FileEntry, needed []string, upload io.Reader) error {
	return s.applyMode(manifest, needed, upload, false)
}

// applyMode syncs the slot. With keepRunner (data sets), only what changed on the client is
// touched: files the runner changed itself (a refreshed token, a symlink a session made) stay.
func (s *slot) applyMode(manifest []api.FileEntry, needed []string, upload io.Reader, keepRunner bool) error {
	rec := s.load()
	root, err := s.openTree()
	if err != nil {
		return err
	}
	defer root.Close()
	for _, l := range rec.Stages { // from an interrupted apply
		if strings.HasPrefix(l, ".offload-stage-") && !strings.ContainsAny(l, "/\\") {
			_ = root.RemoveAll(l)
		}
	}
	rec.Stages = nil
	want := map[string]api.FileEntry{}
	for _, e := range manifest {
		p, err := cleanRel(e.Path)
		if err != nil {
			return err
		}
		want[p] = e
	}
	pending := map[string]bool{}
	for _, p := range needed {
		pending[p] = true
	}
	// Stage the whole upload first; only a complete, verified upload changes the workspace.
	stage := ".offload-stage-" + randomSuffix()
	if len(pending) > 0 {
		rec.Stages = []string{stage}
		if err := s.save(rec); err != nil {
			return err
		}
		rec.Rev++ // save bumped the stored revision; keep ours in step
		if err := root.Mkdir(stage, 0o700); err != nil {
			return err
		}
		defer root.RemoveAll(stage)
	}
	staged, err := extract(root, upload, want, pending, stage)
	if err != nil {
		return err
	}
	for p := range pending {
		if staged[p] == "" {
			return fmt.Errorf("upload is missing %q", p)
		}
	}
	// Record everything about to change (new content, symlinks, modes, deletions) before changing
	// it, so a crash before the final save leaves those paths marked untrusted rather than
	// described by stale metadata. Paths an earlier interrupted apply marked stay marked.
	dirtyBefore := rec.dirtySet()
	dirty := rec.dirtySet()
	for p := range staged {
		dirty[p] = true
	}
	for p, e := range want {
		if old, had := rec.Files[p]; !had || old.Link != e.Link || old.Mode != e.Mode {
			dirty[p] = true
		}
	}
	for p := range rec.Files {
		if _, ok := want[p]; !ok {
			dirty[p] = true
		}
	}
	rec.Dirty = rec.Dirty[:0]
	for p := range dirty {
		rec.Dirty = append(rec.Dirty, p)
	}
	sort.Strings(rec.Dirty)
	if len(rec.Dirty) > 0 {
		if err := s.save(rec); err != nil {
			return err
		}
		rec.Rev++ // save bumped the stored revision; keep ours in step
	}
	written := map[string]bool{}
	for p, tmp := range staged {
		if err := prepareParents(root, p); err != nil {
			return err
		}
		if fi, err := root.Lstat(p); err == nil && !fi.Mode().IsRegular() {
			if err := root.RemoveAll(p); err != nil {
				return err
			}
		}
		if err := root.Rename(tmp, p); err != nil {
			return err
		}
		written[p] = true
	}
	// Only now, with the upload known complete, remove what left the manifest: recorded files,
	// and paths an interrupted apply may have created before recording them. A path whose
	// removal fails stays journaled, so the next apply tries again.
	gone := map[string]bool{}
	for p := range rec.Files {
		gone[p] = true
	}
	for p := range dirtyBefore {
		gone[p] = true
	}
	wantDirs := map[string]bool{} // parents of wanted paths: a file "a" that became "a/b"
	for p := range want {
		for d := path.Dir(p); d != "." && d != "/"; d = path.Dir(d) {
			wantDirs[d] = true
		}
	}
	var stillDirty []string
	for p := range gone {
		if _, ok := want[p]; ok {
			continue
		}
		delete(rec.Files, p)
		if wantDirs[p] {
			continue // now a directory of the tree, replaced by the staged files
		}
		if _, err := cleanRel(p); err != nil {
			continue
		}
		// Only through real directories: a parent the runner turned into a symlink (even one
		// pointing inside the tree) must not redirect this delete onto another tracked file. A
		// parent we can't inspect (permissions) keeps the path journaled, to retry next time.
		switch parentsState(root, p) {
		case parentsReal:
			if err := root.Remove(p); err != nil && !errors.Is(err, fs.ErrNotExist) {
				stillDirty = append(stillDirty, p)
			}
		case parentsUnknown:
			stillDirty = append(stillDirty, p)
		}
	}
	for _, e := range manifest {
		old, had := rec.Files[e.Path]
		if keepRunner && had && !written[e.Path] && !dirtyBefore[e.Path] && old.Link == e.Link && (e.Link != "" || (old.Hash == e.Hash && old.Mode == e.Mode)) {
			continue // unchanged on the client: leave the runner's copy alone
		}
		if err := prepareParents(root, e.Path); err != nil {
			return fmt.Errorf("%s: %w", e.Path, err)
		}
		if e.Link != "" {
			if cur, err := root.Readlink(e.Path); err != nil || cur != e.Link {
				_ = root.RemoveAll(e.Path)
				if err := root.Symlink(e.Link, e.Path); err != nil {
					return err
				}
			}
			rec.Files[e.Path] = slotEntry{Link: e.Link}
			continue
		}
		fi, err := root.Lstat(e.Path)
		if err != nil {
			return fmt.Errorf("%s: %w", e.Path, err)
		}
		if !fi.Mode().IsRegular() {
			if keepRunner {
				continue // the runner replaced it; the client didn't send new content
			}
			return fmt.Errorf("%s: not a regular file on the runner; sync again with --fresh", e.Path)
		}
		if fi.Mode().Perm() != os.FileMode(e.Mode).Perm() {
			if err := root.Chmod(e.Path, os.FileMode(e.Mode).Perm()); err != nil {
				return err
			}
			fi, _ = root.Lstat(e.Path)
		}
		ent := rec.Files[e.Path]
		if written[e.Path] || !keepRunner || !had {
			ent.Hash = e.Hash
		}
		ent.Mode, ent.Link = e.Mode, ""
		ent.Size, ent.MTime, ent.CTime = fi.Size(), fi.ModTime().UnixNano(), ctimeOf(fi)
		rec.Files[e.Path] = ent
	}
	s.LastUsed = time.Now()
	sort.Strings(stillDirty)
	rec.Dirty, rec.Stages = stillDirty, nil
	return s.save(rec)
}

const (
	parentsReal    = iota
	parentsGone    // a parent doesn't exist: so doesn't the path
	parentsAliased // a parent is a symlink or not a directory
	parentsUnknown // a parent couldn't be inspected
)

func parentsState(root *os.Root, rel string) int {
	parts := strings.Split(rel, "/")
	for i := 1; i < len(parts); i++ {
		fi, err := root.Lstat(strings.Join(parts[:i], "/"))
		switch {
		case errors.Is(err, fs.ErrNotExist):
			return parentsGone
		case err != nil:
			return parentsUnknown
		case !fi.IsDir():
			return parentsAliased
		}
	}
	return parentsReal
}

// realParents reports whether every parent directory of rel is a real directory (no symlinks).
func realParents(root *os.Root, rel string) bool {
	parts := strings.Split(rel, "/")
	for i := 1; i < len(parts); i++ {
		fi, err := root.Lstat(strings.Join(parts[:i], "/"))
		if err != nil || !fi.IsDir() {
			return false
		}
	}
	return true
}

// extract stages every requested file of the upload in a private directory inside the slot,
// verifying each hash, and returns path -> staged file. Nothing in the workspace changes here:
// applyMode renames the staged files into place only once the whole upload has arrived. Entries
// in the manifest but not requested are read past and ignored.
func extract(root *os.Root, upload io.Reader, want map[string]api.FileEntry, pending map[string]bool, stage string) (map[string]string, error) {
	staged := map[string]string{}
	if upload == nil {
		return staged, nil
	}
	gz, err := gzip.NewReader(upload)
	if err != nil {
		if errors.Is(err, io.EOF) {
			return staged, nil
		}
		return nil, fmt.Errorf("upload: %w", err)
	}
	tr := tar.NewReader(gz)
	for n := 0; ; n++ {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return staged, nil
		}
		if err != nil {
			return nil, fmt.Errorf("upload: %w", err)
		}
		p, err := cleanRel(h.Name)
		if err != nil {
			return nil, err
		}
		e, ok := want[p]
		if !ok || e.Link != "" || h.Typeflag != tar.TypeReg {
			return nil, fmt.Errorf("upload: unexpected entry %q", h.Name)
		}
		if !pending[p] || staged[p] != "" {
			if _, err := io.Copy(io.Discard, tr); err != nil {
				return nil, fmt.Errorf("upload: %w", err)
			}
			continue
		}
		tmp := path.Join(stage, strconv.Itoa(n))
		if err := stageFile(root, tmp, tr, os.FileMode(e.Mode).Perm(), e.Hash, p); err != nil {
			return nil, err
		}
		staged[p] = tmp
	}
}

// stageFile writes r to the new file tmp inside root, checking its sha256 and setting its mode.
func stageFile(root *os.Root, tmp string, r io.Reader, mode os.FileMode, wantHash, name string) error {
	f, err := root.OpenFile(tmp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	hash := sha256.New()
	_, err = io.Copy(io.MultiWriter(f, hash), r)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil && hex.EncodeToString(hash.Sum(nil)) != wantHash {
		err = fmt.Errorf("upload: %s: hash mismatch", name)
	}
	if err == nil {
		err = root.Chmod(tmp, mode)
	}
	if err != nil {
		_ = root.Remove(tmp)
	}
	return err
}

// writeConfined writes r to rel inside root via a fresh temporary file, checking its sha256.
func writeConfined(root *os.Root, rel string, r io.Reader, mode os.FileMode, wantHash string) error {
	tmp := path.Join(path.Dir(rel), ".offload-"+randomSuffix())
	if err := stageFile(root, tmp, r, mode, wantHash, rel); err != nil {
		return err
	}
	if err := root.Rename(tmp, rel); err != nil {
		_ = root.Remove(tmp)
		return err
	}
	return nil
}

// ctimeOf returns a file's inode change time in nanoseconds (0 if unavailable).
func ctimeOf(fi os.FileInfo) int64 {
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		return st.Ctim.Nano()
	}
	return 0
}

func randomSuffix() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
