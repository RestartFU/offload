package client

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

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
	reportPlaced(&Host{URL: fmt.Sprintf("http://r%d", i)})
	w := &lineWriter{w: os.Stdout, prefix: fmt.Sprintf("[%d/%d] ", i, n)}
	fmt.Fprintf(w, "shard %d of %d after [%s]\npartial", i, n, os.Getenv(shardPlacedEnv))
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
	codes, err := fanOut(context.Background(), os.Args[0], []string{"-test.run=^$"}, 3, out, os.Stderr)
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
		"[1/3] partial", "[1/3] shard 1 of 3 after []",
		"[2/3] partial", "[2/3] shard 2 of 3 after [http://r1]",
		"[3/3] partial", "[3/3] shard 3 of 3 after [http://r1 http://r2]",
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
	if !s.NoWriteBack || s.Shards != 0 || s.ArtifactDir != "offload-shards/2" {
		t.Fatalf("got %+v", s)
	}
	for _, p := range []string{"offload-shards/1/out.txt", "sub/offload-shards/2/x"} {
		if !inShardsDir(p) {
			t.Fatalf("%s is synced", p)
		}
	}
	if inShardsDir("offload-shards.go") || inShardsDir("src/offload-shardsx/a") {
		t.Fatal("other paths must stay synced")
	}
}

// An earlier shard on a runner sends the next one elsewhere, unless only that runner can start it now.
func TestPickSpreadsShards(t *testing.T) {
	want := api.Resources{CPU: 2, Mem: 1 << 30}
	vps := testHost("vps", 22, api.Resources{}, api.Resources{}, "repo")
	zeno := testHost("zeno", 6, api.Resources{}, api.Resources{})
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

// Shard outputs go under offload-shards/<i> of the destination, never through a symlink there.
func TestFetchArtifactsUnderStaysInside(t *testing.T) {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	tw.WriteHeader(&tar.Header{Name: "out.txt", Mode: 0o644, Size: 2, Typeflag: tar.TypeReg})
	tw.Write([]byte("hi"))
	tw.Close()
	gz.Close()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write(buf.Bytes()) }))
	defer srv.Close()
	h := newHost("r", srv.URL, "tok", 0)

	dest, outside := t.TempDir(), t.TempDir()
	if _, err := fetchArtifacts(context.Background(), h, "j-1", dest, "offload-shards/1", nil); err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(filepath.Join(dest, "offload-shards/1/out.txt")); err != nil || string(b) != "hi" {
		t.Fatalf("got %q, %v", b, err)
	}
	dest = t.TempDir()
	os.Symlink(outside, filepath.Join(dest, "offload-shards"))
	fetchArtifacts(context.Background(), h, "j-2", dest, "offload-shards/1", nil)
	if _, err := os.Stat(filepath.Join(outside, "1/out.txt")); err == nil {
		t.Fatal("an artifact went through the symlink, outside the destination")
	}
}

func TestLineWriterBoundsLongLines(t *testing.T) {
	var buf bytes.Buffer
	l := &lineWriter{w: &buf, prefix: "> "}
	l.Write(bytes.Repeat([]byte("x"), maxLine+10))
	if len(l.buf) != 10 || buf.Len() != len("> ")+maxLine+1 {
		t.Fatalf("pending %d, written %d", len(l.buf), buf.Len())
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
