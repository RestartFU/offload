package client

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/RestartFU/offload/internal/api"
)

const (
	MaxShards = 64
	// Each shard is this same command line run again by a child process, which finds its shard
	// here ("i/n") and reports on fd 3 which runner took its job.
	shardOfEnv       = "OFFLOAD_SHARD_OF"
	shardPlacedEnv   = "OFFLOAD_SHARD_PLACED"   // runners earlier shards went to, space separated
	shardSourceEnv   = "OFFLOAD_SHARD_SOURCE"   // sourceDigest of the tree when the run started
	shardPlatformEnv = "OFFLOAD_SHARD_PLATFORM" // os/arch of the first shard's runner
)

// outPrefix is "[i/n] " in a shard's process: its job's output and its status lines carry it.
var outPrefix string

// asShard turns a run into shard i of n: OFFLOAD_SHARD and OFFLOAD_SHARDS in the job's
// environment, {shard} and {shards} replaced in the command, no write-back (shards would edit
// the same files), and --get outputs kept on the runner (shards would download them over each
// other).
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
	o.Env, o.Argv, o.Shards, o.NoWriteBack, o.KeepOutputs = env, argv, 0, true, true
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

// sourceDigest identifies the source a job would get: every shard must partition the same
// suite, even if files change while they start.
func sourceDigest(ws *Workspace) string {
	files := slices.Clone(ws.Manifest)
	slices.SortFunc(files, func(a, b api.FileEntry) int { return strings.Compare(a.Path, b.Path) })
	h := sha256.New()
	fmt.Fprintf(h, "%s\x00", ws.Dir)
	if g := ws.Git; g != nil {
		fmt.Fprintf(h, "%s\x00%s\x00", g.Head, g.Branch)
		for _, t := range slices.Sorted(maps.Keys(g.Tags)) {
			fmt.Fprintf(h, "%s=%s\x00", t, g.Tags[t])
		}
	}
	for _, f := range files {
		fmt.Fprintf(h, "%s\x00%s\x00%o\x00%s\x00", f.Path, f.Hash, f.Mode, f.Link)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// dropOutputs leaves this process's own stdout and stderr out of a shard's source: with
// `> log.txt` inside the tree, that file grows while later shards upload it.
func dropOutputs(ws *Workspace) {
	var outputs []os.FileInfo
	for _, f := range []*os.File{os.Stdout, os.Stderr} {
		if fi, err := f.Stat(); err == nil && fi.Mode().IsRegular() {
			outputs = append(outputs, fi)
		}
	}
	if len(outputs) == 0 {
		return
	}
	ws.Manifest = slices.DeleteFunc(ws.Manifest, func(f api.FileEntry) bool {
		return isOutput(filepath.Join(ws.Root, filepath.FromSlash(f.Path)), outputs)
	})
}

func isOutput(p string, outputs []os.FileInfo) bool {
	fi, err := os.Lstat(p)
	return err == nil && slices.ContainsFunc(outputs, func(o os.FileInfo) bool { return os.SameFile(fi, o) })
}

var (
	shardReport     *os.File
	shardReportOnce sync.Once
	shardReportMu   sync.Mutex
)

var errDetached = errors.New("another shard's output closed, so this one stopped following")

// detachOnSignal ends ctx with errDetached on SIGUSR1, which the parent sends every shard once
// one of them found the shared output closed: a quiet shard would never find out itself.
func detachOnSignal(ctx context.Context) context.Context {
	ctx, detach := context.WithCancelCause(ctx)
	c := make(chan os.Signal, 1)
	signal.Notify(c, syscall.SIGUSR1)
	go func() {
		<-c
		detach(errDetached)
	}()
	return ctx
}

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
		plat := ""
		if h.Status != nil {
			plat = h.Status.OS + "/" + h.Status.Arch
		}
		report(placeKey(h) + " " + plat)
	})
}

// reportClosed tells the parent this shard's output is closed.
func reportClosed() { report("closed") }

func report(line string) {
	shardReportMu.Lock()
	defer shardReportMu.Unlock()
	if shardReport != nil {
		fmt.Fprintln(shardReport, line)
	}
}

// markPlaced counts the earlier shards on each candidate, which pick holds against it.
func markPlaced(hosts []*Host) {
	for _, h := range hosts {
		h.shard = true
	}
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
	var env []string
	if !o.NoSync {
		cwd, _ := os.Getwd()
		ws, err := LoadWorkspace(cwd)
		if err != nil {
			return 1, fmt.Errorf("reading workspace: %w", err)
		}
		dropOutputs(ws)
		env = append(env, shardSourceEnv+"="+sourceDigest(ws))
	}
	codes, err := fanOut(ctx, self, os.Args[1:], env, o.Shards, os.Stdout, os.Stderr)
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

// fanOut runs n children of this binary with args, child i as shard i. Each starts after the
// previous one reported its runner on fd 3 (or exited) and learns where the earlier ones went.
// An interrupt is passed on to every child, which cancels its own job. A child that reports its
// output closed makes them all detach, and no more start.
func fanOut(ctx context.Context, self string, args, env []string, n int, stdout, stderr *os.File) ([]int, error) {
	var mu sync.Mutex
	var started []*exec.Cmd
	signalAll := func(sig os.Signal) {
		mu.Lock()
		defer mu.Unlock()
		for _, c := range started {
			_ = c.Process.Signal(sig)
		}
	}
	interrupt := func() { signalAll(os.Interrupt) }
	var closed sync.Once
	var detached atomic.Bool
	detach := func() {
		detached.Store(true)
		signalAll(syscall.SIGUSR1)
	}
	defer context.AfterFunc(ctx, interrupt)()

	codes := make([]int, n)
	var wg sync.WaitGroup
	var placed []string
	platform := ""
	var startErr error
	for k := range n {
		if ctx.Err() != nil {
			codes[k] = 130
			continue
		}
		if detached.Load() {
			codes[k] = 1
			continue
		}
		r, w, err := os.Pipe()
		if err != nil {
			startErr = err
			break
		}
		cmd := exec.Command(self, args...)
		cmd.Env = append(append(os.Environ(), env...), fmt.Sprintf("%s=%d/%d", shardOfEnv, k+1, n),
			shardPlacedEnv+"="+strings.Join(placed, " "), shardPlatformEnv+"="+platform)
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
		br := bufio.NewReader(r)
		line, _ := br.ReadString('\n')
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer r.Close()
			for {
				l, err := br.ReadString('\n')
				if strings.TrimSpace(l) == "closed" {
					closed.Do(detach)
				}
				if err != nil {
					return
				}
			}
		}()
		if strings.TrimSpace(line) == "closed" {
			closed.Do(detach)
		} else if f := strings.Fields(line); len(f) > 0 {
			placed = append(placed, f[0])
			if platform == "" && len(f) > 1 {
				platform = f[1]
			}
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

// lineWriter writes whole lines, each with a prefix and in one write of at most pipeBuf, so
// shards sharing a pipe never mix within a line. A longer line is written in pieces, and a
// partial one after partialWait, so prompts and progress still show.
type lineWriter struct {
	w      io.Writer
	prefix string
	mu     sync.Mutex
	buf    []byte
	timer  *time.Timer
	err    error  // from a write the timer made
	onErr  func() // called when the timer's write fails, so following stops
}

const (
	pipeBuf     = 4096 // Linux's PIPE_BUF: writes up to it are atomic
	partialWait = 300 * time.Millisecond
)

func (l *lineWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.err != nil {
		return 0, l.err
	}
	n, limit := len(p), pipeBuf-len(l.prefix)-1
	for len(p) > 0 {
		room := limit - len(l.buf)
		switch i := bytes.IndexByte(p, '\n'); {
		case i >= 0 && i <= room:
			l.buf = append(l.buf, p[:i]...)
			p = p[i+1:]
		case len(p) < room:
			l.buf = append(l.buf, p...)
			if l.timer == nil {
				l.timer = time.AfterFunc(partialWait, l.flushPartial)
			}
			return n, nil
		default:
			l.buf = append(l.buf, p[:room]...)
			p = p[room:]
		}
		if err := l.emit(); err != nil {
			return 0, err
		}
	}
	return n, nil
}

func (l *lineWriter) flushPartial() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.timer = nil
	if len(l.buf) > 0 && l.err == nil {
		if l.err = l.emit(); l.err != nil && l.onErr != nil {
			l.onErr()
		}
	}
}

func (l *lineWriter) flush() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.timer != nil {
		l.timer.Stop()
		l.timer = nil
	}
	if l.err != nil || len(l.buf) == 0 {
		return l.err
	}
	return l.emit()
}

// emit writes the pending line; l.mu is held.
func (l *lineWriter) emit() error {
	line := append(append([]byte(l.prefix), l.buf...), '\n')
	l.buf = l.buf[:0]
	_, err := l.w.Write(line)
	return err
}
