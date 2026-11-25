package client

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/gob"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"syscall"

	"github.com/RestartFU/offload/internal/api"
)

// Workspace is the local source tree a job runs against.
type Workspace struct {
	Root     string // absolute path of the tree's root (git toplevel or cwd)
	Key      string // stable runner-side workspace name
	Dir      string // cwd relative to Root
	Manifest []api.FileEntry
	Git      *api.GitRef // HEAD and branch, nil outside git or before the first commit
}

// skipDirs are never synced when the tree is not a git repository.
var skipDirs = map[string]bool{".git": true, "target": true, "node_modules": true, ".venv": true, "__pycache__": true, ".zig-cache": true, "zig-out": true, "build": true, "dist": true}

// LoadWorkspace finds the tree around cwd and builds its manifest. In a git repository the
// manifest is every tracked file plus untracked files that aren't ignored, so build output
// (already ignored) stays on the runner and is never uploaded.
func LoadWorkspace(cwd string) (*Workspace, error) {
	ws := &Workspace{Root: cwd}
	var files []string
	if top, err := git(cwd, "rev-parse", "--show-toplevel"); err == nil {
		ws.Root = strings.TrimSpace(top)
		if head, err := git(ws.Root, "rev-parse", "--verify", "-q", "HEAD"); err == nil {
			ws.Git = &api.GitRef{Head: strings.TrimSpace(head)}
			if br, err := git(ws.Root, "symbolic-ref", "-q", "--short", "HEAD"); err == nil {
				ws.Git.Branch = strings.TrimSpace(br)
			}
		}
		files, err = gitFiles(ws.Root)
		if err != nil {
			return nil, err
		}
	} else {
		err := filepath.WalkDir(cwd, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			if d.IsDir() && p != cwd && (skipDirs[d.Name()] || strings.HasPrefix(d.Name(), ".cache")) {
				return filepath.SkipDir
			}
			if !d.IsDir() {
				rel, _ := filepath.Rel(cwd, p)
				files = append(files, filepath.ToSlash(rel))
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	rel, err := filepath.Rel(ws.Root, cwd)
	if err != nil {
		return nil, err
	}
	ws.Dir = filepath.ToSlash(rel)
	ws.Key = workspaceKey(ws.Root)
	ws.Manifest, err = hashFiles(ws.Root, files)
	return ws, err
}

func git(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	var out bytes.Buffer
	cmd.Stdout = &out
	err := cmd.Run()
	return out.String(), err
}

// gitFiles lists tracked and untracked-but-not-ignored files, descending into submodules.
func gitFiles(root string) ([]string, error) {
	out, err := git(root, "ls-files", "-z", "--cached", "--others", "--exclude-standard")
	if err != nil {
		return nil, fmt.Errorf("git ls-files: %w", err)
	}
	seen := map[string]bool{}
	var files []string
	for _, f := range strings.Split(out, "\x00") {
		if f == "" || seen[f] {
			continue
		}
		seen[f] = true
		full := filepath.Join(root, filepath.FromSlash(f))
		fi, err := os.Lstat(full)
		if err != nil {
			continue // deleted in the work tree but still in the index
		}
		if fi.IsDir() { // a submodule
			sub, err := gitFiles(full)
			if err != nil {
				return nil, err
			}
			for _, s := range sub {
				files = append(files, f+"/"+s)
			}
			continue
		}
		files = append(files, f)
	}
	return files, nil
}

// workspaceKey names the runner-side workspace: repo name plus a hash of its origin (or path),
// so every clone and worktree of a repository shares warm build directories.
func workspaceKey(root string) string {
	id := root
	if url, err := git(root, "config", "--get", "remote.origin.url"); err == nil && strings.TrimSpace(url) != "" {
		id = strings.TrimSpace(url)
	}
	sum := sha256.Sum256([]byte(id))
	name := filepath.Base(root)
	if url, err := git(root, "config", "--get", "remote.origin.url"); err == nil && strings.TrimSpace(url) != "" {
		name = strings.TrimSuffix(filepath.Base(strings.TrimSpace(url)), ".git")
	}
	var b strings.Builder
	for _, r := range name {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' || r == '.' {
			b.WriteRune(r)
		}
	}
	n := b.String()
	if len(n) > 40 {
		n = n[:40]
	}
	if n == "" || n == "." || n == ".." {
		n = "ws"
	}
	return n + "-" + hex.EncodeToString(sum[:])[:10]
}

// hashCache avoids rehashing files whose size and mtime haven't changed since the last run.
type hashCache map[string]cachedHash

type cachedHash struct {
	Size  int64
	MTime int64
	Ino   uint64
	Hash  string
}

func hashCachePath(root string) string {
	sum := sha256.Sum256([]byte(root))
	dir, _ := os.UserCacheDir()
	return filepath.Join(dir, "offload", "hashes", hex.EncodeToString(sum[:8]))
}

func hashFiles(root string, files []string) ([]api.FileEntry, error) {
	cache := hashCache{}
	if f, err := os.Open(hashCachePath(root)); err == nil {
		_ = gob.NewDecoder(f).Decode(&cache)
		f.Close()
	}
	entries := make([]api.FileEntry, len(files))
	keep := make([]bool, len(files))
	var mu sync.Mutex
	next := hashCache{}
	var firstErr error
	// Every read is confined to the tree: a tracked directory replaced by a symlink (to a sibling
	// checkout, say) must not upload what it points at.
	tree, err := os.OpenRoot(root)
	if err != nil {
		return nil, err
	}
	defer tree.Close()
	work := make(chan int)
	var wg sync.WaitGroup
	for w := 0; w < runtime.NumCPU(); w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range work {
				p := files[i]
				if checkRel(p) != nil || !realParents(tree, p) {
					continue // not really in the tree (behind a symlink): absent, like a deleted file
				}
				fi, err := tree.Lstat(p)
				if err != nil {
					continue
				}
				e := api.FileEntry{Path: p, Mode: uint32(fi.Mode().Perm())}
				switch {
				case fi.Mode()&fs.ModeSymlink != 0:
					e.Link, err = tree.Readlink(p)
				case fi.Mode().IsRegular():
					ino := inode(fi)
					c, ok := cache[p]
					if ok && c.Size == fi.Size() && c.MTime == fi.ModTime().UnixNano() && c.Ino == ino {
						e.Hash = c.Hash
					} else {
						var now os.FileInfo
						if e.Hash, now, err = hashFileIn(tree, p); err == nil && !os.SameFile(fi, now) {
							err = fmt.Errorf("changed while being hashed; run again")
						}
					}
					if err == nil {
						mu.Lock()
						next[p] = cachedHash{Size: fi.Size(), MTime: fi.ModTime().UnixNano(), Ino: ino, Hash: e.Hash}
						mu.Unlock()
					}
				default:
					continue // sockets, fifos, devices
				}
				if err != nil {
					mu.Lock()
					if firstErr == nil {
						firstErr = fmt.Errorf("%s: %w", p, err)
					}
					mu.Unlock()
					continue
				}
				entries[i], keep[i] = e, true
			}
		}()
	}
	for i := range files {
		work <- i
	}
	close(work)
	wg.Wait()
	if firstErr != nil {
		return nil, firstErr
	}
	var out []api.FileEntry
	for i, e := range entries {
		if keep[i] {
			out = append(out, e)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	if err := os.MkdirAll(filepath.Dir(hashCachePath(root)), 0o700); err == nil {
		if f, err := os.Create(hashCachePath(root)); err == nil {
			_ = gob.NewEncoder(f).Encode(next)
			f.Close()
		}
	}
	return out, nil
}

// readInTree reads a regular file inside root, through real directories only and never
// following a symlink.
func readInTree(root *os.Root, rel string) ([]byte, error) {
	if err := checkRel(rel); err != nil {
		return nil, err
	}
	parts := strings.Split(rel, "/")
	for i := 1; i < len(parts); i++ {
		fi, err := root.Lstat(strings.Join(parts[:i], "/"))
		if err != nil {
			return nil, err // missing parent: ErrNotExist, like a missing file
		}
		if !fi.IsDir() {
			return nil, fmt.Errorf("%s: %s is not a real directory (a symlink?)", rel, strings.Join(parts[:i], "/"))
		}
	}
	f, err := root.OpenFile(rel, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	if fi, err := f.Stat(); err != nil || !fi.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is not a regular file", rel)
	}
	return io.ReadAll(f)
}

// writeTar streams the needed files as a gzip tar, re-checking each file's hash so an edit made
// during upload is reported instead of silently producing a mismatched workspace.
func writeTar(w io.Writer, root string, manifest []api.FileEntry, needed []string) error {
	byPath := map[string]api.FileEntry{}
	for _, e := range manifest {
		byPath[e.Path] = e
	}
	tree, err := os.OpenRoot(root)
	if err != nil {
		return err
	}
	defer tree.Close()
	gz, _ := gzip.NewWriterLevel(w, gzip.BestSpeed)
	tw := tar.NewWriter(gz)
	for _, p := range needed {
		e, ok := byPath[p]
		if !ok {
			return fmt.Errorf("runner asked for unknown file %q", p)
		}
		b, err := readInTree(tree, p)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(b)
		if hex.EncodeToString(sum[:]) != e.Hash {
			return fmt.Errorf("%s changed while uploading; run again", p)
		}
		if err := tw.WriteHeader(&tar.Header{Name: p, Mode: int64(e.Mode), Size: int64(len(b)), Typeflag: tar.TypeReg, Format: tar.FormatPAX}); err != nil {
			return err
		}
		if _, err := tw.Write(b); err != nil {
			return err
		}
	}
	if err := tw.Close(); err != nil {
		return err
	}
	return gz.Close()
}

// gitBundle writes a bundle of HEAD minus the commits the runner already has (those we also
// have locally) to a temp file and returns its path.
func gitBundle(root string, have []string) (string, error) {
	f, err := os.CreateTemp("", "offload-*.bundle")
	if err != nil {
		return "", err
	}
	f.Close()
	args := []string{"bundle", "create", "-q", f.Name(), "HEAD"}
	if len(have) > 0 {
		cmd := exec.Command("git", "-C", root, "cat-file", "--batch-check=%(objectname) %(objecttype)")
		cmd.Stdin = strings.NewReader(strings.Join(have, "\n") + "\n")
		out, _ := cmd.Output()
		for _, line := range strings.Split(string(out), "\n") {
			if sha, typ, ok := strings.Cut(line, " "); ok && typ == "commit" {
				args = append(args, "^"+sha)
			}
		}
	}
	cmd := exec.Command("git", append([]string{"-C", root}, args...)...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		os.Remove(f.Name())
		return "", fmt.Errorf("git bundle: %v: %s", err, strings.TrimSpace(stderr.String()))
	}
	return f.Name(), nil
}
