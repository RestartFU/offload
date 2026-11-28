package client

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"os/exec"
	"path"
	"strconv"
	"strings"
	"sync"
	"syscall"
)

const (
	MaxShards = 64
	shardsDir = "offload-shards"
	// Each shard is this same command line run again by a child process, which finds its shard
	// here ("i/n") and reports on fd 3 which runner took its job.
	shardOfEnv     = "OFFLOAD_SHARD_OF"
	shardPlacedEnv = "OFFLOAD_SHARD_PLACED" // runners earlier shards went to, space separated
)

// outPrefix is "[i/n] " in a shard's process: its job's output and its status lines carry it.
var outPrefix string

// asShard turns a run into shard i of n: OFFLOAD_SHARD and OFFLOAD_SHARDS in the job's
// environment, {shard} and {shards} replaced in the command, no write-back (shards would edit
// the same files), and --get outputs in offload-shards/<i>/.
func asShard(o RunOptions, i, n int) RunOptions {
	si, sn := strconv.Itoa(i), strconv.Itoa(n)
	env := maps.Clone(o.Env)
	if env == nil {
		env = map[string]string{}
	}
	env["OFFLOAD_SHARD"], env["OFFLOAD_SHARDS"] = si, sn
	r := strings.NewReplacer("{shards}", sn, "{shard}", si)
	argv := make([]string, len(o.Argv))
	for k, a := range o.Argv {
		argv[k] = r.Replace(a)
	}
	o.Env, o.Argv, o.Shards, o.NoWriteBack = env, argv, 0, true
	o.ArtifactDir = path.Join(shardsDir, si)
	return o
}

// shardChild reads the shard this process is, if a parent `offload run --shards` started it.
func shardChild() (i, n int, ok bool) {
	a, b, found := strings.Cut(os.Getenv(shardOfEnv), "/")
	if !found {
		return 0, 0, false
	}
	i, err1 := strconv.Atoi(a)
	n, err2 := strconv.Atoi(b)
	if err1 != nil || err2 != nil || i < 1 || i > n || n > MaxShards {
		return 0, 0, false
	}
	return i, n, true
}

// inShardsDir reports whether a synced path is a shard's downloaded output, which is never
// source for a later job.
func inShardsDir(rel string) bool {
	return strings.HasPrefix(rel, shardsDir+"/") || strings.Contains(rel, "/"+shardsDir+"/")
}

var (
	shardReport     *os.File
	shardReportOnce sync.Once
)

// openShardReport takes fd 3 for reporting, but only if it is the pipe a parent passed: with
// the variable set by hand, fd 3 may be anything this process opened.
func openShardReport() {
	f := os.NewFile(3, "shard-report")
	if f == nil {
		return
	}
	if fi, err := f.Stat(); err == nil && fi.Mode()&os.ModeNamedPipe != 0 {
		syscall.CloseOnExec(3) // inherited without it: commands this process runs must not hold it open
		shardReport = f
	}
}

// placeKey names a runner the same way whichever address reached it.
func placeKey(h *Host) string {
	if h.Status != nil && h.Status.ID != "" {
		return h.Status.ID
	}
	return h.URL
}

// reportPlaced tells the parent which runner took this shard's job, once.
func reportPlaced(h *Host) {
	shardReportOnce.Do(func() {
		if shardReport != nil {
			fmt.Fprintln(shardReport, placeKey(h))
			shardReport.Close()
		}
	})
}

// markPlaced counts the earlier shards on each candidate, which pick holds against it.
func markPlaced(hosts []*Host) {
	placed := map[string]int{}
	for _, k := range strings.Fields(os.Getenv(shardPlacedEnv)) {
		placed[k]++
	}
	for _, h := range hosts {
		h.placed = placed[placeKey(h)]
	}
}

// runShards starts one child `offload run` per shard and waits for them all. The children
// share this process's stdout and stderr, so each one meets a closed output itself and stops
// following as a single run does. The exit code is the first failed shard's.
func runShards(ctx context.Context, o RunOptions) (int, error) {
	self, err := os.Executable()
	if err != nil {
		return 1, err
	}
	if len(o.Artifacts) > 0 {
		if err := makeShardsDir(); err != nil {
			return 1, err
		}
	}
	codes, err := fanOut(ctx, self, os.Args[1:], o.Shards, os.Stdout, os.Stderr)
	if err != nil {
		return 1, err
	}
	code := 0
	for k, c := range codes {
		if c != 0 {
			logf(false, "shard %d/%d exited with %d", k+1, o.Shards, c)
			if code == 0 {
				code = c
			}
		}
	}
	return code, nil
}

// makeShardsDir creates offload-shards/, ignored by git so it stays out of `git status`.
func makeShardsDir() error {
	tree, err := os.OpenRoot(".")
	if err != nil {
		return err
	}
	defer tree.Close()
	if err := mkdirReal(tree, shardsDir); err != nil {
		return fmt.Errorf("%s: %w (it must be a real directory here)", shardsDir, err)
	}
	f, err := tree.OpenFile(path.Join(shardsDir, ".gitignore"), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if errors.Is(err, fs.ErrExist) {
		return nil
	}
	if err != nil {
		return err
	}
	_, err = f.WriteString("*\n")
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return err
}

// fanOut runs n children of this binary with args, child i as shard i. Each starts after the
// previous one reported its runner on fd 3 (or exited) and learns where the earlier ones went.
// An interrupt is passed on to every child, which cancels its own job.
func fanOut(ctx context.Context, self string, args []string, n int, stdout, stderr *os.File) ([]int, error) {
	var mu sync.Mutex
	var started []*exec.Cmd
	interrupt := func() {
		mu.Lock()
		defer mu.Unlock()
		for _, c := range started {
			_ = c.Process.Signal(os.Interrupt)
		}
	}
	defer context.AfterFunc(ctx, interrupt)()

	codes := make([]int, n)
	var wg sync.WaitGroup
	var placed []string
	var startErr error
	for k := range n {
		if ctx.Err() != nil {
			codes[k] = 130
			continue
		}
		r, w, err := os.Pipe()
		if err != nil {
			startErr = err
			break
		}
		cmd := exec.Command(self, args...)
		cmd.Env = append(os.Environ(), fmt.Sprintf("%s=%d/%d", shardOfEnv, k+1, n), shardPlacedEnv+"="+strings.Join(placed, " "))
		cmd.Stdout, cmd.Stderr, cmd.ExtraFiles = stdout, stderr, []*os.File{w}
		mu.Lock()
		err = cmd.Start()
		if err == nil {
			started = append(started, cmd)
		}
		mu.Unlock()
		w.Close()
		if err != nil {
			r.Close()
			startErr = err
			break
		}
		if ctx.Err() != nil { // raced with an interrupt the forwarder already sent
			_ = cmd.Process.Signal(os.Interrupt)
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			codes[k] = exitCode(cmd.Wait())
		}()
		line, _ := bufio.NewReader(r).ReadString('\n')
		r.Close()
		if p := strings.TrimSpace(line); p != "" {
			placed = append(placed, p)
		}
	}
	if startErr != nil {
		interrupt()
	}
	wg.Wait()
	if startErr != nil {
		return nil, fmt.Errorf("starting a shard: %w", startErr)
	}
	return codes, nil
}

func exitCode(err error) int {
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		if ws, ok := ee.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
			return 128 + int(ws.Signal())
		}
		return ee.ExitCode()
	}
	if err != nil {
		return 1
	}
	return 0
}

// lineWriter writes whole lines, each with a prefix and in one write, so lines of shards sharing
// an output don't interleave. A line longer than maxLine is written in pieces.
type lineWriter struct {
	w      io.Writer
	prefix string
	buf    []byte
}

const maxLine = 64 << 10

func (l *lineWriter) Write(p []byte) (int, error) {
	n := len(p)
	for len(p) > 0 {
		i := bytes.IndexByte(p, '\n')
		if i < 0 {
			room := maxLine - len(l.buf)
			if len(p) < room {
				l.buf = append(l.buf, p...)
				return n, nil
			}
			l.buf = append(l.buf, p[:room]...)
			p = p[room:]
		} else {
			l.buf = append(l.buf, p[:i]...)
			p = p[i+1:]
		}
		if err := l.emit(); err != nil {
			return 0, err
		}
	}
	return n, nil
}

func (l *lineWriter) flush() error {
	if len(l.buf) == 0 {
		return nil
	}
	return l.emit()
}

func (l *lineWriter) emit() error {
	line := append(append([]byte(l.prefix), l.buf...), '\n')
	l.buf = l.buf[:0]
	_, err := l.w.Write(line)
	return err
}
