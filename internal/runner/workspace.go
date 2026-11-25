package runner

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
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
	if err := os.RemoveAll(s.Dir); err != nil {
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

// safePath joins rel under root, replacing any symlink or file found where a parent directory
// must be, so writes can never escape the workspace through a stale symlink.
func safePath(root, rel string) (string, error) {
	parts := strings.Split(rel, "/")
	cur := root
	for _, p := range parts[:len(parts)-1] {
		cur = filepath.Join(cur, p)
		fi, err := os.Lstat(cur)
		switch {
		case errors.Is(err, os.ErrNotExist):
			if err := os.Mkdir(cur, 0o755); err != nil {
				return "", err
			}
		case err != nil:
			return "", err
		case !fi.IsDir():
			if err := os.RemoveAll(cur); err != nil {
				return "", err
			}
			if err := os.Mkdir(cur, 0o755); err != nil {
				return "", err
			}
		}
	}
	return filepath.Join(cur, parts[len(parts)-1]), nil
}

// plan compares a client manifest with what the slot holds and returns the paths the client
// must upload. Files whose recorded size or mtime changed since we wrote them (a previous job
// edited them) are re-requested.
func (s *slot) plan(manifest []api.FileEntry) ([]string, error) {
	rec := s.load()
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
		fi, err := os.Lstat(filepath.Join(s.Dir, filepath.FromSlash(p)))
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
	rec := s.load()
	want := map[string]api.FileEntry{}
	for _, e := range manifest {
		want[e.Path] = e
	}
	for p := range rec.Files {
		if _, ok := want[p]; !ok {
			_ = os.Remove(filepath.Join(s.Dir, filepath.FromSlash(p)))
			delete(rec.Files, p)
		}
	}
	pending := map[string]bool{}
	for _, p := range needed {
		pending[p] = true
	}
	if err := s.extract(upload, want, pending, rec); err != nil {
		return err
	}
	if len(pending) > 0 {
		for p := range pending {
			return fmt.Errorf("upload is missing %q (and %d more)", p, len(pending)-1)
		}
	}
	for _, e := range manifest {
		full, err := safePath(s.Dir, e.Path)
		if err != nil {
			return err
		}
		if e.Link != "" {
			if cur, err := os.Readlink(full); err != nil || cur != e.Link {
				_ = os.RemoveAll(full)
				if err := os.Symlink(e.Link, full); err != nil {
					return err
				}
			}
			rec.Files[e.Path] = slotEntry{Link: e.Link}
			continue
		}
		fi, err := os.Lstat(full)
		if err != nil {
			return fmt.Errorf("%s: %w", e.Path, err)
		}
		if fi.Mode().Perm() != os.FileMode(e.Mode).Perm() {
			if err := os.Chmod(full, os.FileMode(e.Mode).Perm()); err != nil {
				return err
			}
			fi, _ = os.Lstat(full)
		}
		ent := rec.Files[e.Path]
		ent.Hash, ent.Mode, ent.Link = e.Hash, e.Mode, ""
		ent.Size, ent.MTime = fi.Size(), fi.ModTime().UnixNano()
		rec.Files[e.Path] = ent
	}
	s.LastUsed = time.Now()
	return s.save(rec)
}

func (s *slot) extract(upload io.Reader, want map[string]api.FileEntry, pending map[string]bool, rec slotRecord) error {
	if upload == nil || len(pending) == 0 {
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
		if !ok || !pending[p] || h.Typeflag != tar.TypeReg {
			return fmt.Errorf("upload: unexpected entry %q", h.Name)
		}
		full, err := safePath(s.Dir, p)
		if err != nil {
			return err
		}
		if fi, err := os.Lstat(full); err == nil && !fi.Mode().IsRegular() {
			if err := os.RemoveAll(full); err != nil {
				return err
			}
		}
		tmp, err := os.CreateTemp(filepath.Dir(full), ".offload-*")
		if err != nil {
			return err
		}
		hash := sha256.New()
		_, err = io.Copy(io.MultiWriter(tmp, hash), tr)
		cerr := tmp.Close()
		if err == nil {
			err = cerr
		}
		if err == nil && hex.EncodeToString(hash.Sum(nil)) != e.Hash {
			err = fmt.Errorf("upload: %s: hash mismatch", p)
		}
		if err == nil {
			err = os.Chmod(tmp.Name(), os.FileMode(e.Mode).Perm())
		}
		if err == nil {
			err = os.Rename(tmp.Name(), full)
		}
		if err != nil {
			_ = os.Remove(tmp.Name())
			return err
		}
		delete(pending, p)
		rec.Files[p] = slotEntry{Hash: e.Hash, Mode: e.Mode}
	}
}
