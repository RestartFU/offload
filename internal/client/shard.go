package client

import (
	"bufio"
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
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
	shardSourceEnv   = "OFFLOAD_SHARD_SOURCE"   // file with the tree as the run found it (shardSource)
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

// shardSource is the source a job would get. Every shard must partition the same suite, so
// each compares what it found with what the run found (sourceChanges).
type shardSource struct {
	Dir      string          `json:"dir"`
	Git      *api.GitRef     `json:"git,omitempty"`
	Manifest []api.FileEntry `json:"manifest"`
}

// sourceChanges lists what differs between two views of the source: paths, and "git" for the
// commit, branch or tags.
func sourceChanges(want, got shardSource) []string {
	var out []string
	if want.Dir != got.Dir || !reflect.DeepEqual(want.Git, got.Git) {
		out = append(out, "git")
	}
	files := map[string]api.FileEntry{}
	for _, f := range want.Manifest {
		files[f.Path] = f
	}
	for _, f := range got.Manifest {
		if w, ok := files[f.Path]; !ok || w != f {
			out = append(out, f.Path)
		}
		delete(files, f.Path)
	}
	for p := range files {
		out = append(out, p)
	}
	slices.Sort(out)
	return out
}

// checkShardSource fails a shard whose source isn't what the run found when it started.
func checkShardSource(file string, ws *Workspace) error {
	b, err := os.ReadFile(file)
	if err != nil {
		return fmt.Errorf("reading the run's source list: %w", err)
	}
	var want shardSource
	if err := json.Unmarshal(b, &want); err != nil {
		return err
	}
	changed := sourceChanges(want, shardSource{Dir: ws.Dir, Git: ws.Git, Manifest: ws.Manifest})
	if len(changed) == 0 {
		return nil
	}
	return fmt.Errorf("%s changed while the shards were starting, and shards must all test the same code: "+
		"keep output files out of the tree or ignore them in git, or run again after editing", strings.Join(firstN(changed, 5), ", "))
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

var (
	shardDetach *os.File
	errDetached = errors.New("another shard's output closed, so this one stopped following")
)

// watchDetach ends ctx with errDetached once the parent closes fd 4: it does when one shard
// found the shared output closed (a quiet shard would never find out itself), or when it dies.
func watchDetach(ctx context.Context) context.Context {
	if shardDetach == nil {
		return ctx
	}
	ctx, detach := context.WithCancelCause(ctx)
	go func() {
		_, _ = io.Copy(io.Discard, shardDetach)
		detach(errDetached)
	}()
	return ctx
}

// openShardPipes takes fd 3 for reporting and fd 4 for detaching, but only if they are the
// pipes a parent passed: with the variable set by hand, they may be anything this process opened.
func openShardPipes() {
	shardReport, shardDetach = inheritedPipe(3), inheritedPipe(4)
}

func inheritedPipe(fd int) *os.File {
	f := os.NewFile(uintptr(fd), "shard-pipe")
	if f == nil {
		return nil
	}
	if fi, err := f.Stat(); err != nil || fi.Mode()&os.ModeNamedPipe == 0 {
		return nil
	}
	syscall.CloseOnExec(fd) // inherited without it: commands this process runs must not hold it open
	return f
}

// OutPrefix is what a shard's process puts before each line it prints ("" outside shards).
func OutPrefix() string { return outPrefix }

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
		file, err := saveShardSource(shardSource{Dir: ws.Dir, Git: ws.Git, Manifest: ws.Manifest})
		if err != nil {
			return 1, err
		}
		defer os.Remove(file)
		env = append(env, shardSourceEnv+"="+file)
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

func saveShardSource(src shardSource) (string, error) {
	b, err := json.Marshal(src)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(stateDir(), 0o700); err != nil {
		return "", err
	}
	f, err := os.CreateTemp(stateDir(), ".shard-source-*")
	if err != nil {
		return "", err
	}
	_, err = f.Write(b)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(f.Name())
		return "", err
	}
	return f.Name(), nil
}

// fanOut runs n children of this binary with args, child i as shard i. Each starts after the
// previous one reported its runner on fd 3 (or exited) and learns where the earlier ones went.
// An interrupt is passed on to every child, which cancels its own job. A child that reports its
// output closed makes them all detach (the parent closes their fd 4), and no more start.
func fanOut(ctx context.Context, self string, args, env []string, n int, stdout, stderr *os.File) ([]int, error) {
	var mu sync.Mutex
	var started []*exec.Cmd
	var detachers []*os.File
	detached := false
	interrupt := func() {
		mu.Lock()
		defer mu.Unlock()
		for _, c := range started {
			_ = c.Process.Signal(os.Interrupt)
		}
	}
	detach := func() {
		mu.Lock()
		defer mu.Unlock()
		detached = true
		for _, d := range detachers {
			d.Close()
		}
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
		mu.Lock()
		stop := detached
		mu.Unlock()
		if stop {
			codes[k] = 1
			continue
		}
		r, w, err := os.Pipe()
		if err != nil {
			startErr = err
			break
		}
		dr, dw, err := os.Pipe()
		if err != nil {
			r.Close()
			w.Close()
			startErr = err
			break
		}
		cmd := exec.Command(self, args...)
		cmd.Env = append(append(os.Environ(), env...), fmt.Sprintf("%s=%d/%d", shardOfEnv, k+1, n),
			shardPlacedEnv+"="+strings.Join(placed, " "), shardPlatformEnv+"="+platform)
		cmd.Stdout, cmd.Stderr, cmd.ExtraFiles = stdout, stderr, []*os.File{w, dr}
		mu.Lock()
		err = cmd.Start()
		if err == nil {
			started = append(started, cmd)
			detachers = append(detachers, dw)
			if detached { // a report came in while it started
				dw.Close()
			}
		}
		mu.Unlock()
		w.Close()
		dr.Close()
		if err != nil {
			r.Close()
			dw.Close()
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
					detach()
				}
				if err != nil {
					return
				}
			}
		}()
		if strings.TrimSpace(line) == "closed" {
			detach()
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
	for _, d := range detachers {
		d.Close()
	}
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
	wait   time.Duration // before a partial line is written; partialWait if 0
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
				l.timer = time.AfterFunc(cmp.Or(l.wait, partialWait), l.flushPartial)
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
