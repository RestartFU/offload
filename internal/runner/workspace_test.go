package runner

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"

	"github.com/RestartFU/offload/internal/api"
)

func entry(p, content string) (api.FileEntry, []byte) {
	sum := sha256.Sum256([]byte(content))
	return api.FileEntry{Path: p, Hash: hex.EncodeToString(sum[:]), Mode: 0o644}, []byte(content)
}

func tarOf(t *testing.T, files map[string][]byte, names []string) *bytes.Buffer {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, n := range names {
		b := files[n]
		if err := tw.WriteHeader(&tar.Header{Name: n, Mode: 0o644, Size: int64(len(b)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		tw.Write(b)
	}
	tw.Close()
	gz.Close()
	return &buf
}

func TestSlotSync(t *testing.T) {
	dir := t.TempDir()
	sl := &slot{Key: "k", Index: 0, Dir: filepath.Join(dir, "0")}
	os.MkdirAll(sl.Dir, 0o755)

	a, ab := entry("src/a.rs", "fn a() {}")
	b, bb := entry("b.txt", "bee")
	files := map[string][]byte{"src/a.rs": ab, "b.txt": bb}
	m := []api.FileEntry{a, b, {Path: "link", Link: "b.txt", Mode: 0o777}}

	need, err := sl.plan(m)
	if err != nil || len(need) != 2 {
		t.Fatalf("first plan: %v %v", need, err)
	}
	if err := sl.apply(m, need, tarOf(t, files, need)); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.Readlink(filepath.Join(sl.Dir, "link")); got != "b.txt" {
		t.Fatalf("symlink = %q", got)
	}
	// Build output survives and an unchanged tree needs nothing.
	os.MkdirAll(filepath.Join(sl.Dir, "target"), 0o755)
	os.WriteFile(filepath.Join(sl.Dir, "target", "out"), []byte("bin"), 0o644)
	if need, _ := sl.plan(m); len(need) != 0 {
		t.Fatalf("unchanged plan needs %v", need)
	}
	// A job edited a synced file: it must be re-sent.
	os.WriteFile(filepath.Join(sl.Dir, "b.txt"), []byte("edited by a job"), 0o644)
	if need, _ := sl.plan(m); len(need) != 1 || need[0] != "b.txt" {
		t.Fatalf("edited plan = %v", need)
	}
	// Remove a file and change another.
	a2, a2b := entry("src/a.rs", "fn a() { changed }")
	m2 := []api.FileEntry{a2}
	need, _ = sl.plan(m2)
	if err := sl.apply(m2, need, tarOf(t, map[string][]byte{"src/a.rs": a2b}, need)); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(sl.Dir, "b.txt")); !os.IsNotExist(err) {
		t.Fatal("b.txt should be gone")
	}
	if _, err := os.Stat(filepath.Join(sl.Dir, "target", "out")); err != nil {
		t.Fatal("build output was removed")
	}
	// A bad hash is rejected.
	bad := []api.FileEntry{{Path: "x", Hash: a.Hash, Mode: 0o644}}
	if err := sl.apply(bad, []string{"x"}, tarOf(t, map[string][]byte{"x": []byte("nope")}, []string{"x"})); err == nil {
		t.Fatal("hash mismatch accepted")
	}
}

func TestCleanRel(t *testing.T) {
	for _, p := range []string{"../x", "/etc/passwd", "a/../../b", "a//b", ""} {
		if _, err := cleanRel(p); err == nil {
			t.Errorf("cleanRel(%q) accepted", p)
		}
	}
	if _, err := cleanRel("a/b.c"); err != nil {
		t.Error(err)
	}
}
