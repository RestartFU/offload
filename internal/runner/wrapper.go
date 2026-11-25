package runner

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// These functions run inside the systemd unit, as the unit's main process. They own the user's
// command, so the runner daemon can restart without killing work.

// RunExec is "offload runner exec": run argv, then record its exit status (and whether the
// cgroup OOM killer fired) and pack requested artifacts into the job directory.
func RunExec(jobDir string, artifacts []string, argv []string) int {
	code, sig, runErr := runChild(argv, nil, os.Stdout)
	rec := exitRecord{Code: code, Signal: sig, OOM: oomKilled()}
	if runErr != nil {
		rec.Error = runErr.Error()
		fmt.Fprintf(os.Stderr, "offload: %v\n", runErr)
	}
	if len(artifacts) > 0 {
		if err := packArtifacts(filepath.Join(jobDir, "artifacts.tar.gz"), artifacts); err != nil {
			fmt.Fprintf(os.Stderr, "offload: artifacts: %v\n", err)
		} else {
			rec.Artifact = true
		}
	}
	if err := writeFileJSON(filepath.Join(jobDir, "exit.json"), rec); err != nil {
		fmt.Fprintf(os.Stderr, "offload: write exit status: %v\n", err)
	}
	return code
}

// RunPipe is "offload runner pipe": run argv with stdin fed from the unix socket sock and stdout
// appended to <dir>/stdout. Any number of connections may write to the socket over time.
func RunPipe(dir, sock string, argv []string) int {
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
