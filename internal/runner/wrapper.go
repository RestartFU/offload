package runner

import (
	"archive/tar"
	"bufio"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/RestartFU/offload/internal/api"
)

// These functions run inside the systemd unit, as the unit's main process. They own the user's
// command, so the runner daemon can restart without killing work.

// ExecOptions configures "offload runner exec".
type ExecOptions struct {
	JobDir    string
	Artifacts []string
	WriteBack string // workspace root to scan for edited synced files ("" = off)
	Record    string // the slot's record of synced files
	Secrets   string // tmpfs directory with env.json (and secret files)
	Argv      []string
}

// loadSecrets exports the job's secret env vars to the command, then deletes env.json. Secret
// files stay (env vars point at them) until the runner removes the directory after the job.
func loadSecrets(dir string) error {
	p := filepath.Join(dir, "env.json")
	b, err := os.ReadFile(p)
	if err != nil {
		return fmt.Errorf("secrets: %w", err)
	}
	_ = os.Remove(p)
	var vars map[string]string
	if err := json.Unmarshal(b, &vars); err != nil {
		return fmt.Errorf("secrets: %w", err)
	}
	for k, v := range vars {
		if err := os.Setenv(k, v); err != nil {
			return fmt.Errorf("secret %s: %w", k, err)
		}
	}
	return nil
}

// RunExec is "offload runner exec": run argv, then record its exit status, peak memory and
// whether the cgroup OOM killer fired, pack requested artifacts, and collect write-back files.
func RunExec(o ExecOptions) int {
	if o.Secrets != "" {
		if err := loadSecrets(o.Secrets); err != nil {
			// Never run the command without the secrets it asked for.
			fmt.Fprintf(os.Stderr, "offload: %v\n", err)
			_ = writeFileJSON(filepath.Join(o.JobDir, "exit.json"), exitRecord{Code: 1, Error: err.Error()})
			return 1
		}
	}
	code, sig, runErr := runChild(o.Argv, nil, os.Stdout)
	rec := exitRecord{Code: code, Signal: sig, OOM: oomKilled(), PeakMem: cgroupInt("memory.peak")}
	if runErr != nil {
		rec.Error = runErr.Error()
		fmt.Fprintf(os.Stderr, "offload: %v\n", runErr)
	}
	if len(o.Artifacts) > 0 {
		missing, err := packArtifacts(filepath.Join(o.JobDir, "artifacts.tar.gz"), o.Artifacts)
		if err != nil {
			fmt.Fprintf(os.Stderr, "offload: artifacts: %v\n", err)
		} else {
			rec.Artifact = true
		}
		rec.ArtifactsMissing = missing
	}
	if o.WriteBack != "" && o.Record != "" {
		wb, err := collectWriteBack(o.WriteBack, o.Record, filepath.Join(o.JobDir, "writeback.tar.gz"))
		if err != nil {
			fmt.Fprintf(os.Stderr, "offload: write-back: %v\n", err)
		}
		rec.WriteBack = wb
	}
	if err := writeFileJSON(filepath.Join(o.JobDir, "exit.json"), rec); err != nil {
		fmt.Fprintf(os.Stderr, "offload: write exit status: %v\n", err)
	}
	return code
}

// writeBackLimit caps how much edited source a job can send back.
const writeBackLimit = 256 << 20

// collectWriteBack finds synced files the job changed, packs them, and updates the slot record so
// the next sync doesn't re-upload what the client is about to receive anyway.
func collectWriteBack(dir, recordPath, dest string) (*api.WriteBack, error) {
	root, err := os.OpenRoot(dir) // confined: a job's symlinks can't redirect what is read back
	if err != nil {
		return nil, err
	}
	defer root.Close()
	b, err := os.ReadFile(recordPath)
	if err != nil {
		return nil, err
	}
	var rec slotRecord
	if err := json.Unmarshal(b, &rec); err != nil {
		return nil, err
	}
	wb := &api.WriteBack{}
	var total int64
	paths := make([]string, 0, len(rec.Files))
	for p := range rec.Files {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	for _, p := range paths {
		ent := rec.Files[p]
		if ent.Link != "" {
			continue
		}
		fi, err := root.Lstat(p)
		if err != nil {
			wb.Deleted = append(wb.Deleted, p)
			continue
		}
		if !fi.Mode().IsRegular() || (fi.Size() == ent.Size && fi.ModTime().UnixNano() == ent.MTime && ctimeOf(fi) == ent.CTime) {
			continue
		}
		h, err := fileHash(root, p)
		if err != nil {
			continue
		}
		if h != ent.Hash || os.FileMode(ent.Mode).Perm() != fi.Mode().Perm() { // content, or just chmod
			if total += fi.Size(); total > writeBackLimit {
				return nil, fmt.Errorf("edited files exceed %d MiB; not sending them back", writeBackLimit>>20)
			}
			wb.Modified = append(wb.Modified, api.ChangedFile{Path: p, Hash: h, BaseHash: ent.Hash, Mode: uint32(fi.Mode().Perm()), Size: fi.Size()})
		}
		ent.Hash, ent.Size, ent.MTime, ent.CTime, ent.Mode = h, fi.Size(), fi.ModTime().UnixNano(), ctimeOf(fi), uint32(fi.Mode().Perm())
		rec.Files[p] = ent
	}
	if len(wb.Modified) > 0 {
		if err := packFiles(dest, root, wb.Modified); err != nil {
			return nil, err
		}
	}
	if err := writeFileJSON(recordPath, rec); err != nil {
		return nil, err
	}
	if len(wb.Modified) == 0 && len(wb.Deleted) == 0 {
		return nil, nil
	}
	return wb, nil
}

func fileHash(root *os.Root, p string) (string, error) {
	f, err := root.Open(p)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func packFiles(dest string, root *os.Root, files []api.ChangedFile) error {
	f, err := os.Create(dest)
	if err != nil {
		return err
	}
	defer f.Close()
	gz := gzip.NewWriter(f)
	tw := tar.NewWriter(gz)
	for _, c := range files {
		src, err := root.Open(c.Path)
		if err != nil {
			return err
		}
		err = tw.WriteHeader(&tar.Header{Name: c.Path, Mode: int64(c.Mode), Size: c.Size, Typeflag: tar.TypeReg, Format: tar.FormatPAX})
		if err == nil {
			_, err = io.CopyN(tw, src, c.Size)
		}
		src.Close()
		if err != nil {
			return err
		}
	}
	if err := tw.Close(); err != nil {
		return err
	}
	return gz.Close()
}

// cgroupInt reads a single-number file from this job's unit cgroup (e.g. memory.peak).
func cgroupInt(name string) int64 {
	b, err := os.ReadFile(filepath.Join(unitCgroup(), name))
	if err != nil {
		return 0
	}
	v, _ := strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64)
	return v
}

// pipeGreeting starts every stdin connection, so the runner knows this wrapper speaks the framed
// protocol (an older wrapper sends nothing and takes raw bytes).
const pipeGreeting = "offload-pipe 2\n"

// RunPipe is "offload runner pipe": run argv with stdin fed from the unix socket sock and stdout
// appended to <dir>/stdout. Any number of connections may write to the socket over time.
func RunPipe(dir, sock, secrets string, noStdin bool, argv []string) int {
	if secrets != "" {
		if err := loadSecrets(secrets); err != nil {
			_ = writeFileJSON(filepath.Join(dir, "exit.json"), exitRecord{Code: 1, Error: err.Error()})
			fmt.Fprintf(os.Stderr, "offload: %v\n", err)
			return 1
		}
	}
	fail := func(err error) int {
		fmt.Fprintf(os.Stderr, "offload: %v\n", err)
		_ = writeFileJSON(filepath.Join(dir, "exit.json"), exitRecord{Code: 1, Error: err.Error()})
		return 1
	}
	_ = os.MkdirAll(filepath.Dir(sock), 0o700)
	_ = os.Remove(sock)
	ln, err := net.Listen("unix", sock)
	if err != nil {
		return fail(err)
	}
	defer os.Remove(sock)
	_ = os.Chmod(sock, 0o600)
	out, err := os.OpenFile(filepath.Join(dir, "stdout"), os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		return fail(err)
	}
	// A real pipe (not io.Pipe) so exec hands the fd straight to the child and Wait never
	// blocks on a stdin copier.
	pr, pw, err := os.Pipe()
	if err != nil {
		return fail(err)
	}
	// Frames are "<seq> <len>\n<payload>" with seq > 0. "ok\n" means queued for the process
	// (delivery is asynchronous; a process that already closed stdin is reported as
	// "stdin closed" from then on) (a duplicate seq, e.g. retried after a runner restart or a lost
	// acknowledgement, is acknowledged and dropped); "busy\n" if the process isn't reading and
	// the queue is full. lastSeq lives here, in the process that owns stdin.
	var mu sync.Mutex
	var lastSeq int64
	var ended bool         // the process exited; frames is closed
	var broken atomic.Bool // the process closed its stdin
	if noStdin {           // nothing will ever be sent: the command reads EOF at once
		pw.Close()
		broken.Store(true)
	}
	var queued atomic.Int64    // bytes waiting in frames
	const maxQueued = 16 << 20 // backpressure ("busy") beyond this
	frames := make(chan []byte, 64)
	go func() {
		defer pw.Close()
		for payload := range frames {
			queued.Add(-int64(len(payload)))
			if broken.Load() {
				continue // drain so senders never block; they are told "stdin closed"
			}
			if _, err := pw.Write(payload); err != nil {
				broken.Store(true)
			}
		}
	}()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				if _, err := io.WriteString(c, pipeGreeting); err != nil {
					return
				}
				r := bufio.NewReader(c)
				for {
					var seq int64
					var n int
					hdr, err := r.ReadString('\n')
					if err != nil {
						return
					}
					if _, err := fmt.Sscanf(hdr, "%d %d", &seq, &n); err != nil || seq <= 0 || n < 0 || n > maxStdinFrame {
						fmt.Fprintf(c, "bad frame\n")
						return
					}
					payload := make([]byte, n)
					if _, err := io.ReadFull(r, payload); err != nil {
						return
					}
					reply := "ok\n"
					mu.Lock()
					switch {
					case ended || broken.Load():
						reply = "stdin closed\n"
					case seq <= lastSeq:
						// a retry of a frame already queued: acknowledge, don't repeat it
					case seq != lastSeq+1:
						reply = "gap\n" // a frame was lost: refuse rather than reorder
					case queued.Load()+int64(len(payload)) > maxQueued:
						reply = "busy\n"
					default:
						select {
						case frames <- payload:
							lastSeq = seq
							queued.Add(int64(len(payload)))
						default:
							reply = "busy\n"
						}
					}
					mu.Unlock()
					if _, err := io.WriteString(c, reply); err != nil {
						return
					}
				}
			}(c)
		}
	}()
	code, sig, runErr := runChild(argv, pr, out)
	ln.Close()
	mu.Lock()
	ended = true
	close(frames) // the writer finishes (or drains) and closes the pipe
	mu.Unlock()
	rec := exitRecord{Code: code, Signal: sig, OOM: oomKilled()}
	if runErr != nil {
		rec.Error = runErr.Error()
		fmt.Fprintf(os.Stderr, "offload: %v\n", runErr)
	}
	_ = writeFileJSON(filepath.Join(dir, "exit.json"), rec)
	return code
}

// runChild runs argv, forwarding SIGTERM/SIGINT, and returns a shell-style exit code.
func runChild(argv []string, stdin io.Reader, stdout io.Writer) (int, string, error) {
	path, err := exec.LookPath(argv[0])
	if err != nil {
		return 127, "", err
	}
	cmd := exec.Command(path, argv[1:]...)
	cmd.Args[0] = argv[0]
	cmd.Stdin, cmd.Stdout, cmd.Stderr = stdin, stdout, os.Stderr
	if err := cmd.Start(); err != nil {
		return 126, "", err
	}
	if f, ok := stdin.(*os.File); ok {
		f.Close() // the child holds its own copy of the read end
	}
	sigs := make(chan os.Signal, 4)
	signal.Notify(sigs, syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP)
	go func() {
		for s := range sigs {
			_ = cmd.Process.Signal(s)
		}
	}()
	err = cmd.Wait()
	signal.Stop(sigs)
	close(sigs)
	if err == nil {
		return 0, "", nil
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		if ws, ok := ee.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
			return 128 + int(ws.Signal()), ws.Signal().String(), nil
		}
		return ee.ExitCode(), "", nil
	}
	return 1, "", err
}

// unitCgroup is the job's unit cgroup. With podman's --cgroups=split, the wrapper itself is moved
// into a child cgroup, so walk up to the offload-*.service directory: its counters cover the
// container too.
func unitCgroup() string {
	dir := ownCgroup()
	for d := dir; d != "/sys/fs/cgroup" && d != "/" && d != "."; d = filepath.Dir(d) {
		if b := filepath.Base(d); strings.HasPrefix(b, "offload-") && strings.HasSuffix(b, ".service") {
			return d
		}
	}
	return dir
}

// oomKilled reports whether the OOM killer fired anywhere in this job's unit.
func oomKilled() bool {
	b, err := os.ReadFile(filepath.Join(unitCgroup(), "memory.events"))
	if err != nil {
		return false
	}
	for _, line := range strings.Split(string(b), "\n") {
		if v, ok := strings.CutPrefix(line, "oom_kill "); ok {
			n, _ := strconv.Atoi(strings.TrimSpace(v))
			return n > 0
		}
	}
	return false
}

// packArtifacts tars the given paths (files or directories, relative to the working directory).
// packArtifacts archives the requested paths and returns those it couldn't pack (missing or
// unreadable), so a partial set never looks complete.
func packArtifacts(dest string, paths []string) (missing []string, err error) {
	// Confined to the working directory: an artifact path (or a parent of it) that a job turned
	// into a symlink can't make the runner pack files from elsewhere on the host.
	root, err := os.OpenRoot(".")
	if err != nil {
		return nil, err
	}
	defer root.Close()
	f, err := os.Create(dest)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	gz := gzip.NewWriter(f)
	tw := tar.NewWriter(gz)
	found := 0
	packed := map[string]bool{} // overlapping requests (dist and dist/app) pack each entry once
	for _, p := range paths {
		clean := filepath.ToSlash(filepath.Clean(p))
		if filepath.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, "../") {
			return nil, fmt.Errorf("artifact path %q must stay inside the working directory", p)
		}
		err := fs.WalkDir(root.FS(), clean, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if packed[path] {
				return nil
			}
			packed[path] = true
			info, err := root.Lstat(path)
			if err != nil {
				return err
			}
			link := ""
			if info.Mode()&fs.ModeSymlink != 0 {
				link, _ = root.Readlink(path) // stored as a link, never followed
			}
			h, err := tar.FileInfoHeader(info, link)
			if err != nil {
				return err
			}
			h.Name = path
			if err := tw.WriteHeader(h); err != nil {
				return err
			}
			if info.Mode().IsRegular() {
				src, err := root.Open(path)
				if err != nil {
					return err
				}
				_, err = io.CopyN(tw, src, info.Size())
				src.Close()
				if err != nil {
					return err
				}
			}
			found++
			return nil
		})
		if err != nil {
			fmt.Fprintf(os.Stderr, "offload: artifact %s: %v\n", p, err)
			missing = append(missing, p)
		}
	}
	if err := tw.Close(); err != nil {
		return missing, err
	}
	if err := gz.Close(); err != nil {
		return missing, err
	}
	if found == 0 {
		return missing, errors.New("none of the requested artifacts exist")
	}
	return missing, nil
}

// checkBinary makes sure an uploaded binary runs on this machine before we swap it in, and
// returns the protocol version it speaks.
func checkBinary(path string) (int, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, path, "proto").CombinedOutput()
	if err != nil {
		return 0, fmt.Errorf("new binary does not run (or predates protocol checks): %v: %s", err, strings.TrimSpace(string(out)))
	}
	n, err := strconv.Atoi(strings.TrimSpace(string(out)))
	if err != nil {
		return 0, fmt.Errorf("new binary reports protocol %q", strings.TrimSpace(string(out)))
	}
	return n, nil
}
