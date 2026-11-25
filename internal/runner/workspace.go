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
	"os"
	"path"
	"regexp"
	"strings"
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
	Files    map[string]slotEntry `json:"files"`
}

type slotEntry struct {
	Hash  string `json:"hash,omitempty"`
	Mode  uint32 `json:"mode"`
	Link  string `json:"link,omitempty"`
	Size  int64  `json:"size"`
	MTime int64  `json:"mtime"` // unix nanoseconds after we wrote it
}

var keyRE = regexp.MustCompile(`^[A-Za-z0-9._-]{1,100}$`)

func validKey(k string) bool { return keyRE.MatchString(k) && k != "." && k != ".." }

func (s *slot) recordPath() string { return s.Dir + ".json" }

func (s *slot) load() slotRecord {
	rec := slotRecord{Files: map[string]slotEntry{}}
	b, err := os.ReadFile(s.recordPath())
	if err == nil {
		_ = json.Unmarshal(b, &rec)
	}
	if rec.Files == nil {
		rec.Files = map[string]slotEntry{}
	}
	return rec
}

func (s *slot) save(rec slotRecord) error {
	rec.Key = s.Key
	rec.LastUsed = s.LastUsed
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
	seen := map[string]bool{}
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
		if !ok || old.Hash != e.Hash || old.Link != "" {
			needed = append(needed, p)
			continue
		}
		fi, err := root.Lstat(p)
		if err != nil || !fi.Mode().IsRegular() || fi.Size() != old.Size || fi.ModTime().UnixNano() != old.MTime {
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
	want := map[string]api.FileEntry{}
	for _, e := range manifest {
		p, err := cleanRel(e.Path)
		if err != nil {
			return err
		}
		want[p] = e
	}
	for p := range rec.Files {
		if _, ok := want[p]; !ok {
			// Only through real directories: a parent the runner turned into a symlink (even one
			// pointing inside the tree) must not redirect this delete onto another tracked file.
			if _, err := cleanRel(p); err == nil && realParents(root, p) {
				_ = root.Remove(p)
			}
			delete(rec.Files, p)
		}
	}
	pending := map[string]bool{}
	for _, p := range needed {
		pending[p] = true
	}
	written := map[string]bool{}
	if err := extract(root, upload, want, pending, written); err != nil {
		return err
	}
	for p := range pending {
		return fmt.Errorf("upload is missing %q (and %d more)", p, len(pending)-1)
	}
	for _, e := range manifest {
		old, had := rec.Files[e.Path]
		if keepRunner && had && !written[e.Path] && old.Link == e.Link && (e.Link != "" || (old.Hash == e.Hash && old.Mode == e.Mode)) {
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
		ent.Size, ent.MTime = fi.Size(), fi.ModTime().UnixNano()
		rec.Files[e.Path] = ent
	}
	s.LastUsed = time.Now()
	return s.save(rec)
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

// extract writes the uploaded files (verifying each hash) through the confined root, each via an
// exclusively created temporary file and an atomic rename.
// Entries outside pending are accepted too when they match the manifest (another client may have
// synced some of them between this client's plan and its upload).
func extract(root *os.Root, upload io.Reader, want map[string]api.FileEntry, pending, written map[string]bool) error {
	if upload == nil {
		return nil
	}
	gz, err := gzip.NewReader(upload)
	if err != nil {
		if errors.Is(err, io.EOF) {
			return nil
		}
		return fmt.Errorf("upload: %w", err)
	}
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("upload: %w", err)
		}
		p, err := cleanRel(h.Name)
		if err != nil {
			return err
		}
		e, ok := want[p]
		if !ok || e.Link != "" || h.Typeflag != tar.TypeReg {
			return fmt.Errorf("upload: unexpected entry %q", h.Name)
		}
		if err := prepareParents(root, p); err != nil {
			return err
		}
		if fi, err := root.Lstat(p); err == nil && !fi.Mode().IsRegular() {
			if err := root.RemoveAll(p); err != nil {
				return err
			}
		}
		if err := writeConfined(root, p, tr, os.FileMode(e.Mode).Perm(), e.Hash); err != nil {
			return err
		}
		delete(pending, p)
		written[p] = true
	}
}

// writeConfined writes r to rel inside root via a fresh temporary file, checking its sha256.
func writeConfined(root *os.Root, rel string, r io.Reader, mode os.FileMode, wantHash string) error {
	tmp := path.Join(path.Dir(rel), ".offload-"+randomSuffix())
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
		err = fmt.Errorf("upload: %s: hash mismatch", rel)
	}
	if err == nil {
		err = root.Chmod(tmp, mode)
	}
	if err == nil {
		err = root.Rename(tmp, rel)
	}
	if err != nil {
		_ = root.Remove(tmp)
	}
	return err
}

func randomSuffix() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
