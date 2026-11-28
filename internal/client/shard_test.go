package client

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
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
// prints what it learned about the earlier shards.
func shardTestChild() {
	i, n, ok := shardChild()
	if !ok {
		os.Exit(99)
	}
	openShardReport()
	reportPlaced(&Host{URL: fmt.Sprintf("http://r%d", i)})
	fmt.Printf("shard %d of %d after [%s]\npartial", i, n, os.Getenv(shardPlacedEnv))
	fmt.Fprintln(os.Stderr, "status")
	if i == 2 {
		os.Exit(3)
	}
	os.Exit(0)
}

func TestFanOutPlacesShardsInTurn(t *testing.T) {
	var children []child
	for i := 1; i <= 3; i++ {
		children = append(children, child{
			label: fmt.Sprintf("%d/3", i),
			args:  []string{"-test.run=^$"},
			env:   []string{"OFFLOAD_TEST_SHARD_CHILD=1", fmt.Sprintf("%s=%d/3", shardOfEnv, i)},
		})
	}
	var out, errOut bytes.Buffer
	codes, err := fanOut(context.Background(), os.Args[0], children, &out, &errOut, true, true)
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(codes) != "[0 3 0]" {
		t.Fatalf("codes %v", codes)
	}
	if worst(codes, children) != 3 {
		t.Fatal("the failed shard's code must win")
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	sort.Strings(lines)
	want := []string{
		"[1/3] partial", "[1/3] shard 1 of 3 after []",
		"[2/3] partial", "[2/3] shard 2 of 3 after [http://r1]",
		"[3/3] partial", "[3/3] shard 3 of 3 after [http://r1 http://r2]",
	}
	if fmt.Sprint(lines) != fmt.Sprint(want) {
		t.Fatalf("stdout:\n%s", out.String())
	}
	if strings.Count(errOut.String(), "] status\n") != 3 {
		t.Fatalf("stderr:\n%s", errOut.String())
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
	if !s.NoWriteBack || s.Shards != 0 || s.ArtifactDir != "shards/2" {
		t.Fatalf("got %+v", s)
	}
	if one := asShard(RunOptions{Argv: []string{"x"}}, 1, 1); one.ArtifactDir != "" {
		t.Fatal("a single shard's outputs belong where they always go")
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
}

func TestLineWriterKeepsLinesWhole(t *testing.T) {
	var buf bytes.Buffer
	l := &lineWriter{w: &buf, mu: &sync.Mutex{}, prefix: "[1/2] "}
	l.Write([]byte("a\nb"))
	l.Write([]byte("c\nd\n"))
	l.Write([]byte("e"))
	l.flush()
	if got := buf.String(); got != "[1/2] a\n[1/2] bc\n[1/2] d\n[1/2] e\n" {
		t.Fatalf("got %q", got)
	}
}
