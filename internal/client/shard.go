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
	"os/signal"
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
	shardPlacedEnv = "OFFLOAD_SHARD_PLACED" // runner URLs earlier shards went to, space separated
)

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
	if n > 1 {
		o.ArtifactDir = path.Join(shardsDir, si)
	}
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

// reportPlaced tells the parent which runner took this shard's job, once.
func reportPlaced(h *Host) {
	shardReportOnce.Do(func() {
		if shardReport != nil {
			fmt.Fprintln(shardReport, h.URL)
			shardReport.Close()
		}
	})
}

// markPlaced counts the earlier shards on each candidate, which pick holds against it.
func markPlaced(hosts []*Host) {
	placed := map[string]int{}
	for _, u := range strings.Fields(os.Getenv(shardPlacedEnv)) {
		placed[u]++
	}
	for _, h := range hosts {
		h.placed = placed[h.URL]
	}
}

// runShards starts one child `offload run` per shard and waits for them all. Each child starts
// once the previous one has a job on a runner, so it sees that job and prefers another runner.
// The exit code is the first failed shard's.
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
	var children []child
	for i := 1; i <= o.Shards; i++ {
		children = append(children, child{
			label: fmt.Sprintf("%d/%d", i, o.Shards),
			args:  os.Args[1:],
			env:   []string{fmt.Sprintf("%s=%d/%d", shardOfEnv, i, o.Shards)},
		})
	}
	codes, err := fanOut(ctx, self, children, os.Stdout, os.Stderr, !o.Detach, true)
	if err != nil {
		return 1, err
	}
	return worst(codes, children), nil
}

// makeShardsDir creates offload-shards/ ignoring itself, so shards' outputs are never synced
// to later jobs as source.
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

// AttachAll follows several jobs at once, each line prefixed with its job. Ctrl-C detaches.
func AttachAll(refs []string) (int, error) {
	signal.Ignore(syscall.SIGPIPE)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	self, err := os.Executable()
	if err != nil {
		return 1, err
	}
	var children []child
	for _, r := range refs {
		children = append(children, child{label: r, args: []string{"attach", r}})
	}
	codes, err := fanOut(ctx, self, children, os.Stdout, os.Stderr, true, false)
	if err != nil {
		return 1, err
	}
	return worst(codes, children), nil
}

func worst(codes []int, children []child) int {
	code := 0
	for k, c := range codes {
		if c != 0 {
			logf(false, "[%s] exited with %d", children[k].label, c)
			if code == 0 {
				code = c
			}
		}
	}
	return code
}

type child struct {
	label string
	args  []string
	env   []string
}

// fanOut runs children of this binary with their output prefixed by their label (stdout only
// with prefixOut, so detached job references stay plain). With placement, each child starts
// after the previous one reported its runner on fd 3 (or exited), and learns where the earlier
// ones went. An interrupt is passed on to every child, which cancels or detaches its own job.
func fanOut(ctx context.Context, self string, children []child, out, errOut io.Writer, prefixOut, placement bool) ([]int, error) {
	var outMu, errMu sync.Mutex
	var mu sync.Mutex
	var started []*exec.Cmd
	stopForward := context.AfterFunc(ctx, func() {
		mu.Lock()
		defer mu.Unlock()
		for _, c := range started {
			_ = c.Process.Signal(os.Interrupt)
		}
	})
	defer stopForward()

	codes := make([]int, len(children))
	var wg sync.WaitGroup
	var placed []string
	var startErr error
	for k, ch := range children {
		if ctx.Err() != nil {
			codes[k] = 130
			continue
		}
		cmd := exec.Command(self, ch.args...)
		cmd.Env = append(os.Environ(), ch.env...)
		prefix := "[" + ch.label + "] "
		stdout := &lineWriter{w: out, mu: &outMu}
		if prefixOut {
			stdout.prefix = prefix
		}
		stderr := &lineWriter{w: errOut, mu: &errMu, prefix: prefix}
		cmd.Stdout, cmd.Stderr = stdout, stderr
		var report *os.File
		if placement {
			r, w, err := os.Pipe()
			if err != nil {
				startErr = err
				break
			}
			report = r
			cmd.ExtraFiles = []*os.File{w}
			cmd.Env = append(cmd.Env, shardPlacedEnv+"="+strings.Join(placed, " "))
		}
		mu.Lock()
		err := cmd.Start()
		if err == nil {
			started = append(started, cmd)
		}
		mu.Unlock()
		if len(cmd.ExtraFiles) > 0 {
			cmd.ExtraFiles[0].Close()
		}
		if err != nil {
			if report != nil {
				report.Close()
			}
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
			if err := errors.Join(stdout.flush(), stderr.flush()); err != nil && codes[k] == 0 {
				codes[k] = 1 // its output didn't all arrive
			}
		}()
		if report != nil {
			line, _ := bufio.NewReader(report).ReadString('\n')
			report.Close()
			if u := strings.TrimSpace(line); u != "" {
				placed = append(placed, u)
			}
		}
	}
	if startErr != nil {
		mu.Lock()
		for _, c := range started {
			_ = c.Process.Signal(os.Interrupt)
		}
		mu.Unlock()
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

// lineWriter writes whole lines, each with a prefix, so children's output never interleaves
// within a line. A line longer than maxLine is written in pieces.
type lineWriter struct {
	w      io.Writer
	mu     *sync.Mutex
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
	l.mu.Lock()
	defer l.mu.Unlock()
	_, err := l.w.Write(line)
	return err
}
