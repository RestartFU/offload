package client

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/RestartFU/offload/internal/api"
)

func TestMain(m *testing.M) {
	if os.Getenv("OFFLOAD_TEST_SHARD_CHILD") != "" {
		shardTestChild()
	}
	os.Exit(m.Run())
}

// shardTestChild stands in for a shard's `offload run`: it reports a runner of its own and
// prints, as a shard's job output, what it learned about the earlier shards.
func shardTestChild() {
	i, n, ok := shardChild()
	if !ok {
		os.Exit(99)
	}
	openShardReport()
	reportPlaced(&Host{URL: fmt.Sprintf("http://r%d", i), Status: &api.Status{OS: "linux", Arch: fmt.Sprintf("a%d", i)}})
	w := &lineWriter{w: os.Stdout, prefix: fmt.Sprintf("[%d/%d] ", i, n)}
	fmt.Fprintf(w, "shard %d of %d after [%s] on [%s]\npartial", i, n, os.Getenv(shardPlacedEnv), os.Getenv(shardPlatformEnv))
	if w.flush() != nil {
		os.Exit(98)
	}
	if i == 2 {
		os.Exit(3)
	}
	os.Exit(0)
}

func TestFanOutPlacesShardsInTurn(t *testing.T) {
	t.Setenv("OFFLOAD_TEST_SHARD_CHILD", "1")
	out, err := os.Create(filepath.Join(t.TempDir(), "out"))
	if err != nil {
		t.Fatal(err)
	}
	codes, err := fanOut(context.Background(), os.Args[0], []string{"-test.run=^$"}, nil, 3, out, os.Stderr)
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(codes) != "[0 3 0]" {
		t.Fatalf("codes %v", codes)
	}
	b, _ := os.ReadFile(out.Name())
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	sort.Strings(lines)
	want := []string{
		"[1/3] partial", "[1/3] shard 1 of 3 after [] on []",
		"[2/3] partial", "[2/3] shard 2 of 3 after [http://r1] on [linux/a1]",
		"[3/3] partial", "[3/3] shard 3 of 3 after [http://r1 http://r2] on [linux/a1]",
	}
	if fmt.Sprint(lines) != fmt.Sprint(want) {
		t.Fatalf("stdout:\n%s", b)
	}
}

func TestAsShard(t *testing.T) {
	o := RunOptions{Env: map[string]string{"A": "1"}, Argv: []string{"nextest", "--partition", "count:{shard}/{shards}"}, Shards: 4}
	s := asShard(o, 2, 4)
	if got := strings.Join(s.Argv, " "); got != "nextest --partition count:2/4" {
		t.Fatalf("argv %q", got)
	}
	if s.Env["OFFLOAD_SHARD"] != "2" || s.Env["OFFLOAD_SHARDS"] != "4" || s.Env["A"] != "1" {
		t.Fatalf("env %v", s.Env)
	}
	if _, leaked := o.Env["OFFLOAD_SHARD"]; leaked {
		t.Fatal("asShard changed the caller's env")
	}
	if !s.NoWriteBack || !s.KeepOutputs || s.Shards != 0 {
		t.Fatalf("got %+v", s)
	}
}

func TestSourceDigest(t *testing.T) {
	ws := &Workspace{Dir: ".", Git: &api.GitRef{Head: "abc"}, Manifest: []api.FileEntry{{Path: "a", Hash: "1"}, {Path: "b", Hash: "2"}}}
	d := sourceDigest(ws)
	ws.Manifest[0], ws.Manifest[1] = ws.Manifest[1], ws.Manifest[0]
	if sourceDigest(ws) != d {
		t.Fatal("the order files were listed in must not matter")
	}
	ws.Manifest[0].Hash = "3"
	if sourceDigest(ws) == d {
		t.Fatal("a changed file must change the digest")
	}
	ws.Manifest[0].Hash = "2"
	ws.Manifest = append(ws.Manifest, api.FileEntry{Path: "new_test.go", Hash: "4"})
	if sourceDigest(ws) == d {
		t.Fatal("a new file must change the digest")
	}
}

// An earlier shard on a runner sends the next one elsewhere, unless only that runner can start it now.
func TestPickSpreadsShards(t *testing.T) {
	want := api.Resources{CPU: 2, Mem: 1 << 30}
	vps := testHost("vps", 22, api.Resources{}, api.Resources{}, "repo")
	zeno := testHost("zeno", 6, api.Resources{}, api.Resources{})
	vps.shard, zeno.shard = true, true // as markPlaced leaves them
	if h, _ := pick([]*Host{vps, zeno}, want, "", "repo", "job", nil); h.Name != "vps" {
		t.Fatalf("picked %s; want the warm, bigger vps first", h.Name)
	}
	vps.placed = 1
	if h, _ := pick([]*Host{vps, zeno}, want, "", "repo", "job", nil); h.Name != "zeno" {
		t.Fatalf("picked %s; want zeno for the second shard", h.Name)
	}
	zeno.Status.Allocated = api.Resources{CPU: 6}
	if h, _ := pick([]*Host{vps, zeno}, want, "", "repo", "job", nil); h.Name != "vps" {
		t.Fatalf("picked %s; zeno can't start it now", h.Name)
	}
	// However many shards it holds, a runner that can start one beats one that would queue it.
	vps.placed = 3
	if h, _ := pick([]*Host{vps, zeno}, want, "", "repo", "job", nil); h.Name != "vps" {
		t.Fatalf("picked %s with 3 shards on vps; zeno would queue it", h.Name)
	}
	// Even when the queueing one has the higher priority.
	zeno.Priority = 1
	if h, _ := pick([]*Host{vps, zeno}, want, "", "repo", "job", nil); h.Name != "vps" {
		t.Fatalf("picked %s; zeno would queue it", h.Name)
	}
}

func TestLineWriterBoundsLongLines(t *testing.T) {
	var buf bytes.Buffer
	l := &lineWriter{w: &buf, prefix: "> "}
	l.Write(bytes.Repeat([]byte("x"), pipeBuf+10))
	l.flush()
	first, _, _ := strings.Cut(buf.String(), "\n")
	if len(first)+1 != pipeBuf || strings.Count(buf.String(), "\n") != 2 {
		t.Fatalf("first write %d bytes, %d lines", len(first)+1, strings.Count(buf.String(), "\n"))
	}
}

// A long line arriving with its newline in the same write is still cut at pipeBuf.
func TestLineWriterBoundsLongLinesWithNewline(t *testing.T) {
	var buf bytes.Buffer
	l := &lineWriter{w: &buf, prefix: "> "}
	l.Write(append(bytes.Repeat([]byte("x"), pipeBuf), '\n'))
	for _, line := range strings.SplitAfter(buf.String(), "\n") {
		if len(line) > pipeBuf {
			t.Fatalf("a write of %d bytes", len(line))
		}
	}
}

func TestLineWriterTimedWriteErrorStopsFollowing(t *testing.T) {
	stopped := make(chan struct{})
	l := &lineWriter{w: failingWriter{}, onErr: func() { close(stopped) }}
	l.Write([]byte("partial"))
	select {
	case <-stopped:
	case <-time.After(10 * time.Second):
		t.Fatal("a failed timed write didn't stop following")
	}
	if l.flush() == nil {
		t.Fatal("the error must reach follow")
	}
}

func TestIsOutput(t *testing.T) {
	dir := t.TempDir()
	log, other := filepath.Join(dir, "log.txt"), filepath.Join(dir, "a.go")
	os.WriteFile(log, nil, 0o644)
	os.WriteFile(other, nil, 0o644)
	f, _ := os.OpenFile(log, os.O_WRONLY, 0)
	defer f.Close()
	fi, _ := f.Stat()
	if !isOutput(log, []os.FileInfo{fi}) || isOutput(other, []os.FileInfo{fi}) {
		t.Fatal("only the file output goes to is skipped")
	}
}

type chanWriter chan string

func (c chanWriter) Write(p []byte) (int, error) { c <- string(p); return len(p), nil }

func TestLineWriterShowsPartialLines(t *testing.T) {
	c := make(chanWriter, 1)
	l := &lineWriter{w: c, prefix: "[1/2] "}
	l.Write([]byte("Ready? "))
	select {
	case got := <-c:
		if got != "[1/2] Ready? \n" {
			t.Fatalf("got %q", got)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("a partial line never showed")
	}
}

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, errors.New("disk full") }

func TestLineWriterFlushReportsErrors(t *testing.T) {
	l := &lineWriter{w: failingWriter{}}
	l.Write([]byte("last line, no newline"))
	if l.flush() == nil {
		t.Fatal("a lost last line must be reported")
	}
}

func TestLineWriterKeepsLinesWhole(t *testing.T) {
	var buf bytes.Buffer
	l := &lineWriter{w: &buf, prefix: "[1/2] "}
	l.Write([]byte("a\nb"))
	l.Write([]byte("c\nd\n"))
	l.Write([]byte("e"))
	l.flush()
	if got := buf.String(); got != "[1/2] a\n[1/2] bc\n[1/2] d\n[1/2] e\n" {
		t.Fatalf("got %q", got)
	}
}
