package runner

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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

func TestCommandSig(t *testing.T) {
	for want, argv := range map[string][]string{
		"cargo test":  {"cargo", "+nightly", "test", "-p", "x"},
		"cargo build": {"sh", "-c", "RUSTFLAGS=-g cargo build --release && ls"},
		"python3":     {"python3", "-c", "print('hi')"},
		"ci.sh":       {"./scripts/ci.sh"},
		"go test":     {"/usr/bin/go", "test", "./..."},
	} {
		if got := commandSig(argv); got != want {
			t.Errorf("commandSig(%q) = %q, want %q", argv, got, want)
		}
	}
}

func TestEnvPlan(t *testing.T) {
	tag, cf, v := envPlan(nil)
	if tag != baseTag() || cf != baseContainerfile || !strings.HasPrefix(v, "b") {
		t.Fatalf("default plan: %s %s", tag, v)
	}
	tag2, cf2, v2 := envPlan(&api.EnvDef{Containerfile: "FROM offload/base\nRUN dnf install -y jq\n"})
	if !strings.Contains(cf2, "FROM "+baseTag()+"\n") || v2 != v || tag2 == tag {
		t.Fatalf("derived plan: %s %s\n%s", tag2, v2, cf2)
	}
	_, _, v3 := envPlan(&api.EnvDef{Containerfile: "FROM debian:12\n"})
	if !strings.HasPrefix(v3, "e") {
		t.Fatalf("standalone variant %s", v3)
	}
}

// A job that turns a synced directory into a symlink must not make the next sync delete (or
// write) files outside the workspace.
func TestSlotSyncStaysInside(t *testing.T) {
	dir := t.TempDir()
	outside := filepath.Join(dir, "outside")
	os.MkdirAll(outside, 0o755)
	os.WriteFile(filepath.Join(outside, "f"), []byte("precious"), 0o644)
	sl := &slot{Key: "k", Dir: filepath.Join(dir, "ws")}
	f, fb := entry("d/f", "inside")
	if err := sl.apply([]api.FileEntry{f}, []string{"d/f"}, tarOf(t, map[string][]byte{"d/f": fb}, []string{"d/f"})); err != nil {
		t.Fatal(err)
	}
	os.RemoveAll(filepath.Join(sl.Dir, "d"))
	os.Symlink(outside, filepath.Join(sl.Dir, "d")) // what a malicious or careless job could do
	if err := sl.apply(nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(filepath.Join(outside, "f")); err != nil || string(b) != "precious" {
		t.Fatalf("file outside the workspace was touched: %q %v", b, err)
	}
	g, gb := entry("d/g", "new")
	if err := sl.apply([]api.FileEntry{g}, []string{"d/g"}, tarOf(t, map[string][]byte{"d/g": gb}, []string{"d/g"})); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(outside, "g")); err == nil {
		t.Fatal("write went through the symlink")
	}
}

// The wrapper deduplicates stdin frames: a retried frame (same seq) is acknowledged, not re-sent.
func TestSessionFramesDeduplicate(t *testing.T) {
	dir := t.TempDir()
	sock := filepath.Join(dir, "in.sock")
	outFile := filepath.Join(dir, "stdout")
	done := make(chan int, 1)
	go func() { done <- RunPipe(dir, sock, "", []string{"cat"}) }()
	var c net.Conn
	var err error
	for i := 0; i < 50; i++ {
		if c, err = net.Dial("unix", sock); err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err != nil {
		t.Fatal(err)
	}
	r := bufio.NewReader(c)
	for _, f := range []struct {
		seq  int64
		body string
	}{{1, "one\n"}, {1, "one\n"}, {2, "two\n"}} {
		if err := sendFrame(c, r, f.seq, []byte(f.body)); err != nil {
			t.Fatal(err)
		}
	}
	c.Close()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if b, _ := os.ReadFile(outFile); string(b) == "one\ntwo\n" {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	b, _ := os.ReadFile(outFile)
	t.Fatalf("stdout = %q, want each frame once", b)
}
