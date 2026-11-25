package runner

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
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
	if !readGreeting(c, r) {
		t.Fatal("wrapper did not greet with the framed protocol")
	}
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

func TestContextProblem(t *testing.T) {
	m := []api.FileEntry{{Path: "ctx/a", Hash: "x"}, {Path: "alias", Link: "ctx"}, {Path: "empty/.keep", Link: "../x"}}
	if p := contextProblem(m, "ctx"); p != "" {
		t.Errorf("real dir rejected: %s", p)
	}
	if p := contextProblem(m, "alias"); p == "" {
		t.Error("symlinked context accepted")
	}
	if p := contextProblem(m, "nothing"); p == "" {
		t.Error("empty context accepted")
	}
	if p := contextProblem(m, "."); p != "" {
		t.Errorf("root context rejected: %s", p)
	}
}

// Frames must arrive in order: a gap is refused, and a closed stdin is reported, not acknowledged.
func TestSessionFramesGapAndClosedStdin(t *testing.T) {
	dir := t.TempDir()
	sock := fmt.Sprintf("/tmp/oft-%d.sock", time.Now().UnixNano()) // short: unix socket paths are limited
	go RunPipe(dir, sock, "", []string{"sh", "-c", "exec 0<&-; sleep 2"})
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
	defer c.Close()
	r := bufio.NewReader(c)
	if !readGreeting(c, r) {
		t.Fatal("no greeting")
	}
	if err := sendFrame(c, r, 2, []byte("x\n")); err == nil || !strings.Contains(err.Error(), "gap") {
		t.Fatalf("gap accepted: %v", err)
	}
}

func TestUsesContext(t *testing.T) {
	for cf, want := range map[string]bool{
		"FROM offload/base\nCOPY . /src\n":                                     true,
		"FROM a AS builder\nRUN make\nFROM b\nCOPY --from=builder /out /out\n": false,
		"FROM b\nRUN --mount=type=bind,target=/src make -C /src\n":             true,
		"FROM b\nRUN --mount=type=bind,from=builder,target=/x ls\n":            false,
		"FROM offload/base\nRUN apt-get install -y jq\n":                       false,
		"FROM b\nRUN echo \\\n  hi\nCOPY \\\n  . /src\n":                       true,  // continued COPY
		"FROM b\nRUN apt-get install -y \\\n  # a comment\\\n  jq\n":           false, // comment inside a continuation
		"FROM b\nCOPY --chown=1:1 --from=builder /a /a\n":                      false,
		"FROM b\nCOPY ./--from=x /a\n":                                         true, // not a flag
		"FROM b\nRUN --mount=target=/src make\n":                               true, // bind is the default mount type
		"FROM b\nRUN --mount=type=bind,source=from=docs,target=/d ls\n":        true, // a source named from=…
		"FROM b\nRUN --mount=type=cache,target=/root/.cache make\n":            false,
		"# escape=`\nFROM b\nCOPY `\n  . /src\n":                               true,
	} {
		if got := usesContext(cf); got != want {
			t.Errorf("usesContext(%q) = %v, want %v", cf, got, want)
		}
	}
}

func TestInterruptedApplyIsDistrusted(t *testing.T) {
	dir := t.TempDir()
	sl := &slot{Key: "k", Index: 0, Dir: filepath.Join(dir, "0")}
	os.MkdirAll(sl.Dir, 0o755)
	a, ab := entry("a.txt", "one")
	m := []api.FileEntry{a}
	need, _ := sl.plan(m)
	if err := sl.apply(m, need, tarOf(t, map[string][]byte{"a.txt": ab}, need)); err != nil {
		t.Fatal(err)
	}
	// Simulate a crash after the dirty record was saved: a.txt looks unchanged, but its record
	// can't be trusted, so it is requested again; a recorded stage is cleaned, a lookalike not.
	rec := sl.load()
	rec.Dirty, rec.Stages = []string{"a.txt"}, []string{".offload-stage-dead"}
	if err := sl.save(rec); err != nil {
		t.Fatal(err)
	}
	os.MkdirAll(filepath.Join(sl.Dir, ".offload-stage-dead"), 0o755)
	os.WriteFile(filepath.Join(sl.Dir, ".offload-stage-mine"), []byte("job file"), 0o644)
	if need, _ := sl.plan(m); len(need) != 1 {
		t.Fatalf("dirty file not re-requested: %v", need)
	}
	need, _ = sl.plan(m)
	if err := sl.apply(m, need, tarOf(t, map[string][]byte{"a.txt": ab}, need)); err != nil {
		t.Fatal(err)
	}
	if need, _ := sl.plan(m); len(need) != 0 {
		t.Fatalf("still distrusted after a complete apply: %v", need)
	}
	if _, err := os.Stat(filepath.Join(sl.Dir, ".offload-stage-dead")); !os.IsNotExist(err) {
		t.Fatal("recorded stage left behind")
	}
	if _, err := os.Stat(filepath.Join(sl.Dir, ".offload-stage-mine")); err != nil {
		t.Fatal("removed a file that only looked like a stage")
	}
}

func TestAgentRequestAllowed(t *testing.T) {
	frame := func(b ...byte) []byte { return append([]byte{0, 0, 0, byte(len(b))}, b...) }
	for _, c := range []struct {
		msg  []byte
		want bool
	}{
		{frame(11), true},               // list identities
		{frame(13, 0, 0, 0), true},      // sign
		{frame(17, 1, 2), false},        // add identity
		{frame(19), false},              // remove all
		{frame(22, 0), false},           // lock
		{frame(27, 1), false},           // extension
		{[]byte{0, 0, 0, 9, 11}, false}, // length mismatch
		{nil, false},
	} {
		if got := api.AgentRequestAllowed(c.msg); got != c.want {
			t.Errorf("AgentRequestAllowed(%v) = %v, want %v", c.msg, got, c.want)
		}
	}
}

func TestInterruptedDataApplyRepairsSymlink(t *testing.T) {
	dir := t.TempDir()
	sl := &slot{Key: "d", Index: 0, Dir: filepath.Join(dir, "0")}
	os.MkdirAll(sl.Dir, 0o755)
	m := []api.FileEntry{{Path: "cur", Link: "v1", Mode: 0o777}}
	if err := sl.applyMode(m, nil, tarOf(t, nil, nil), true); err != nil {
		t.Fatal(err)
	}
	// A crash mid-apply towards v2: the link already points at v2, the record still says v1 and
	// marks it dirty. The client is back at v1: the plan must still repair it.
	rec := sl.load()
	rec.Dirty = []string{"cur"}
	sl.save(rec)
	os.Remove(filepath.Join(sl.Dir, "cur"))
	os.Symlink("v2", filepath.Join(sl.Dir, "cur"))
	needed, changed, err := sl.planData(m)
	if err != nil || len(needed) != 0 || !changed {
		t.Fatalf("plan: %v %v %v", needed, changed, err)
	}
	if err := sl.applyMode(m, nil, tarOf(t, nil, nil), true); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.Readlink(filepath.Join(sl.Dir, "cur")); got != "v1" {
		t.Fatalf("link = %q, want v1", got)
	}
	if len(sl.load().Dirty) != 0 {
		t.Fatal("still dirty")
	}
}
