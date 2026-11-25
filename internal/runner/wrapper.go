package runner

import (
	"archive/tar"
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
func loadSecrets(dir string) {
	p := filepath.Join(dir, "env.json")
	b, err := os.ReadFile(p)
	if err != nil {
		fmt.Fprintf(os.Stderr, "offload: secrets: %v\n", err)
		return
	}
	_ = os.Remove(p)
	var vars map[string]string
	if json.Unmarshal(b, &vars) == nil {
		for k, v := range vars {
			_ = os.Setenv(k, v)
		}
	}
}

// RunExec is "offload runner exec": run argv, then record its exit status, peak memory and
// whether the cgroup OOM killer fired, pack requested artifacts, and collect write-back files.
func RunExec(o ExecOptions) int {
	if o.Secrets != "" {
		loadSecrets(o.Secrets)
	}
	code, sig, runErr := runChild(o.Argv, nil, os.Stdout)
	rec := exitRecord{Code: code, Signal: sig, OOM: oomKilled(), PeakMem: cgroupInt("memory.peak")}
	if runErr != nil {
		rec.Error = runErr.Error()
		fmt.Fprintf(os.Stderr, "offload: %v\n", runErr)
	}
	if len(o.Artifacts) > 0 {
		if err := packArtifacts(filepath.Join(o.JobDir, "artifacts.tar.gz"), o.Artifacts); err != nil {
			fmt.Fprintf(os.Stderr, "offload: artifacts: %v\n", err)
		} else {
			rec.Artifact = true
		}
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
func collectWriteBack(root, recordPath, dest string) (*api.WriteBack, error) {
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
		full := filepath.Join(root, filepath.FromSlash(p))
		fi, err := os.Lstat(full)
		if err != nil {
			wb.Deleted = append(wb.Deleted, p)
			continue
		}
		if !fi.Mode().IsRegular() || (fi.Size() == ent.Size && fi.ModTime().UnixNano() == ent.MTime) {
			continue
		}
		h, err := fileHash(full)
		if err != nil {
			continue
		}
		if h != ent.Hash {
			if total += fi.Size(); total > writeBackLimit {
				return nil, fmt.Errorf("edited files exceed %d MiB; not sending them back", writeBackLimit>>20)
			}
			wb.Modified = append(wb.Modified, api.ChangedFile{Path: p, Hash: h, BaseHash: ent.Hash, Mode: uint32(fi.Mode().Perm()), Size: fi.Size()})
		}
		ent.Hash, ent.Size, ent.MTime, ent.Mode = h, fi.Size(), fi.ModTime().UnixNano(), uint32(fi.Mode().Perm())
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

func fileHash(p string) (string, error) {
	f, err := os.Open(p)
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

func packFiles(dest, root string, files []api.ChangedFile) error {
	f, err := os.Create(dest)
	if err != nil {
		return err
	}
	defer f.Close()
	gz := gzip.NewWriter(f)
	tw := tar.NewWriter(gz)
	for _, c := range files {
		src, err := os.Open(filepath.Join(root, filepath.FromSlash(c.Path)))
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

// cgroupInt reads a single-number file from this unit's cgroup (e.g. memory.peak).
func cgroupInt(name string) int64 {
	b, err := os.ReadFile(filepath.Join(ownCgroup(), name))
	if err != nil {
		return 0
	}
	v, _ := strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64)
	return v
}

// RunPipe is "offload runner pipe": run argv with stdin fed from the unix socket sock and stdout
// appended to <dir>/stdout. Any number of connections may write to the socket over time.
func RunPipe(dir, sock, secrets string, argv []string) int {
	if secrets != "" {
		loadSecrets(secrets)
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
	var mu sync.Mutex
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				buf := make([]byte, 64<<10)
				for {
					n, err := c.Read(buf)
					if n > 0 {
						mu.Lock()
						_, werr := pw.Write(buf[:n])
						mu.Unlock()
						if werr != nil {
							return
						}
					}
					if err != nil {
						return
					}
				}
			}(c)
		}
	}()
	code, sig, runErr := runChild(argv, pr, out)
	ln.Close()
	pw.Close()
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

// oomKilled reports whether the OOM killer fired inside this unit's cgroup.
func oomKilled() bool {
	b, err := os.ReadFile(filepath.Join(ownCgroup(), "memory.events"))
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
func packArtifacts(dest string, paths []string) error {
	f, err := os.Create(dest)
	if err != nil {
		return err
	}
	defer f.Close()
	gz := gzip.NewWriter(f)
	tw := tar.NewWriter(gz)
	found := 0
	for _, p := range paths {
		clean := filepath.Clean(p)
		if filepath.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, "../") {
			return fmt.Errorf("artifact path %q must stay inside the working directory", p)
		}
		err := filepath.WalkDir(clean, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			info, err := d.Info()
			if err != nil {
				return err
			}
			link := ""
			if info.Mode()&fs.ModeSymlink != 0 {
				link, _ = os.Readlink(path)
			}
			h, err := tar.FileInfoHeader(info, link)
			if err != nil {
				return err
			}
			h.Name = filepath.ToSlash(path)
			if err := tw.WriteHeader(h); err != nil {
				return err
			}
			if info.Mode().IsRegular() {
				src, err := os.Open(path)
				if err != nil {
					return err
				}
				_, err = io.Copy(tw, src)
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
		}
	}
	if err := tw.Close(); err != nil {
		return err
	}
	if err := gz.Close(); err != nil {
		return err
	}
	if found == 0 {
		return errors.New("none of the requested artifacts exist")
	}
	return nil
}

// checkBinary makes sure an uploaded binary runs on this machine before we swap it in.
func checkBinary(path string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, path, "version").CombinedOutput()
	if err != nil {
		return fmt.Errorf("new binary does not run: %v: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}
