package runner

import (
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/RestartFU/offload/internal/api"
)

// Container runtime: jobs run in rootless podman inside their systemd unit (--cgroups=split), so
// the unit's CPU/memory limits, OOM accounting and keep-alive protections still apply. Agents are
// root inside the container and can install anything; anything worth keeping goes into the
// environment's Containerfile, which every runner builds the same way (layer-cached).
//
// Lessons baked in (see docs/design.md): label=disable (SELinux would block the bind mounts),
// explicit --memory/--cpuset-cpus (tools inside size themselves from them), --pids-limit=-1,
// secrets via a tmpfs mount (podman writes -e values to disk), identical mount paths (cargo and
// .git store absolute paths), podman unshare for files owned by container users.

//go:embed envs/base.Containerfile
var baseContainerfile string

var fromBaseRE = regexp.MustCompile(`(?mi)^(\s*FROM\s+)offload/base(\s|$)`)

const imageRepo = "localhost/offload"

// envBuild tracks one image build; jobs waiting on it get its log appended to their stderr.
type envBuild struct {
	tag     string
	done    bool
	err     error
	waiters map[string]*job
	logPath string
	read    int64  // bytes of the log already copied to the waiters
	slot    *slot  // the workspace a COPY/ADD build snapshots its context from
	dir     string // this attempt's private directory
	ctxRel  string // that context's directory in the workspace ("" = no context)

	unit         string    // the unit building this image (not the base or bootstrap steps)
	hadWaiters   bool      // jobs asked for it (a build only sessions asked for has none)
	sessionAsked time.Time // last time a session launch asked for it
	abandoned    bool      // every job that wanted it left: stopped, not an error to report
	prebuild     bool      // the runner's own base prebuild: nobody needs to keep asking
}

func hash12(parts ...string) string {
	h := sha256.New()
	for _, p := range parts {
		io.WriteString(h, p)
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))[:12]
}

func (s *Server) containersAvailable() bool {
	if s.cfg.Container.Runtime == "host" {
		return false
	}
	_, err := exec.LookPath("podman")
	return err == nil
}

// baseTag is the image tag of the built-in base environment for this runner's offload version.
func baseTag() string { return imageRepo + "/base:" + hash12(baseContainerfile) }

// envPlan resolves a job's environment to an image tag, the Containerfile to build it from, and
// the workspace variant (build output is kept apart per base, so a different base never reuses
// stale artifacts, while adding tools on top of the same base keeps target/ warm).
func envPlan(def *api.EnvDef) (tag, containerfile, variant string) {
	if def == nil || strings.TrimSpace(def.Containerfile) == "" {
		return baseTag(), baseContainerfile, "b" + strings.TrimPrefix(baseTag(), imageRepo+"/base:")[:8]
	}
	cf := def.Containerfile
	base := strings.TrimPrefix(baseTag(), imageRepo+"/base:")
	if fromBaseRE.MatchString(cf) {
		cf = fromBaseRE.ReplaceAllString(cf, "${1}"+baseTag()+"${2}")
		return imageRepo + "/env:" + hash12(base, cf, strings.Join(def.Devices, ",")), cf, "b" + base[:8]
	}
	h := hash12(cf)
	return imageRepo + "/env:" + h, cf, "e" + h[:8]
}

// usesContext reports whether a Containerfile reads the local build context: COPY/ADD without
// --from (which reads another stage or image instead), or RUN with a bind mount of the context.
func usesContext(cf string) bool {
	for _, line := range logicalLines(cf) {
		f := strings.Fields(line)
		if len(f) == 0 {
			continue
		}
		var flags []string // the instruction's leading --flags
		for _, w := range f[1:] {
			if !strings.HasPrefix(w, "--") {
				break
			}
			flags = append(flags, w)
		}
		switch strings.ToUpper(f[0]) {
		case "COPY", "ADD":
			fromStage := false
			for _, w := range flags {
				if strings.HasPrefix(w, "--from=") {
					fromStage = true
				}
			}
			if !fromStage {
				return true
			}
		case "RUN":
			for _, w := range flags {
				opts, ok := strings.CutPrefix(w, "--mount=")
				if !ok {
					continue
				}
				typ, from := "bind", false // bind is the default type
				for _, kv := range strings.Split(opts, ",") {
					k, v, _ := strings.Cut(kv, "=")
					switch strings.TrimSpace(k) {
					case "type":
						typ = v
					case "from":
						from = true
					}
				}
				if typ == "bind" && !from {
					return true
				}
			}
		}
	}
	return false
}

// logicalLines joins a Containerfile's continuation lines (ending in the escape character, `\`
// unless an escape directive says otherwise) and drops comments, as the builder reads it.
func logicalLines(cf string) []string {
	esc := "\\"
	lines := strings.Split(strings.ReplaceAll(cf, "\r\n", "\n"), "\n")
	for _, l := range lines { // parser directives come first, before anything else
		t := strings.TrimSpace(l)
		if !strings.HasPrefix(t, "#") {
			break
		}
		if k, v, ok := strings.Cut(strings.TrimSpace(strings.TrimPrefix(t, "#")), "="); ok && strings.EqualFold(strings.TrimSpace(k), "escape") {
			if v = strings.TrimSpace(v); v == "`" {
				esc = v
			}
		}
	}
	var out []string
	var cur strings.Builder
	for _, l := range lines {
		t := strings.TrimSpace(l)
		if strings.HasPrefix(t, "#") {
			continue // comments, also between continuation lines
		}
		if strings.HasSuffix(t, esc) {
			cur.WriteString(strings.TrimSuffix(t, esc))
			cur.WriteString(" ")
			continue
		}
		cur.WriteString(t)
		out = append(out, cur.String())
		cur.Reset()
	}
	if cur.Len() > 0 {
		out = append(out, cur.String())
	}
	return out
}

// imageReadyLocked reports whether an image marked ready still exists, re-checking at most once
// a minute (another runner of this user, or a manual prune, may have removed it).
func (s *Server) imageReadyLocked(tag string) bool {
	if !s.readyImages[tag] {
		return false
	}
	if time.Since(s.readyChecked[tag]) < time.Minute {
		return true
	}
	if !imageExists(tag) {
		delete(s.readyImages, tag)
		return false
	}
	s.readyChecked[tag] = time.Now()
	return true
}

func podman(ctx context.Context, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	out, err := exec.CommandContext(ctx, "podman", args...).CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

func imageExists(tag string) bool {
	_, err := podman(context.Background(), "image", "exists", tag)
	return err == nil
}

// ensureImageLocked returns true when the job's image is ready. Otherwise it starts (or joins) a
// build in the background and the job stays queued; a failed build fails the job.
func (s *Server) ensureImageLocked(j *job) bool {
	tag, cf, _ := envPlan(j.Spec.Environment)
	ctxRel := ""
	if j.Spec.Environment != nil && usesContext(cf) && j.slot != nil {
		ctxRel = contextRel(j.Spec.Environment.ContextDir)
		// Keyed by the exact context (directory and content): only identical inputs share an image.
		tag = imageRepo + "/env:" + hash12(tag, ctxRel, j.ContextDigest)
	}
	j.Image = tag
	if b := s.builds[tag]; b != nil {
		b.waiters[j.ID] = j
		b.hadWaiters = true
		return false
	}
	if s.imageReadyLocked(tag) {
		return true
	}
	if imageExists(tag) && s.bootstrapped(tag) {
		s.readyImages[tag] = true
		s.touchImage(tag)
		return true
	}
	if ok, _ := s.fitsLocked(api.Resources{Mem: s.cfg.buildMem}); !ok {
		j.Reason = "waiting: memory for the environment build"
		return false // builds take their memory from the same budget as jobs
	}
	b := &envBuild{tag: tag, waiters: map[string]*job{j.ID: j}, ctxRel: ctxRel, hadWaiters: true}
	if ctxRel != "" {
		b.slot = j.slot
		b.slot.leases++ // held only while the context snapshot is copied
	}
	s.builds[tag] = b
	name := "base"
	if j.Spec.Environment != nil {
		name = j.Spec.Environment.Name
	}
	s.note(j, fmt.Sprintf("building environment %s on %s (cached layers make later builds fast)", name, s.cfg.Name))
	go s.build(b, cf)
	return false
}

// build builds an image (and the base first, when needed) in its own unit with its own limits,
// so a job's small learned memory limit can't starve its environment build.
func (s *Server) build(b *envBuild, cf string) {
	// Each attempt has its own directory (Containerfile, context snapshot, log): a retry, or a
	// build unit that outlived a runner restart, never shares files with another attempt.
	dir := filepath.Join(s.cfg.StateDir, "envs", strings.ReplaceAll(strings.TrimPrefix(b.tag, imageRepo+"/"), ":", "-"), newID("a-"))
	_ = os.MkdirAll(dir, 0o755)
	s.mu.Lock()
	b.dir = dir
	s.mu.Unlock()
	defer os.RemoveAll(dir)
	b.logPath = filepath.Join(dir, "build.log")
	_ = os.WriteFile(b.logPath, nil, 0o644)
	stop, pumped := make(chan struct{}), make(chan struct{})
	go func() { s.pumpBuildLog(b, stop); close(pumped) }()
	var err error
	ctxDir := ""
	if b.slot != nil {
		// Podman reads a private, immutable copy of exactly the synced context files, never the
		// live workspace (which may hold ignored files, .git, or symlinks out of the tree).
		ctxDir = filepath.Join(dir, "context")
		err = snapshotContext(b.slot, b.ctxRel, ctxDir)
		s.mu.Lock()
		if b.slot.leases--; b.slot.leases == 0 && b.slot.releaseAfterBuild {
			s.releaseSlotLocked(b.slot, b.slot.dropAfterBuild)
		}
		s.mu.Unlock()
	}
	// Every phase runs in a unit of its own, registered on the build before it starts, so a build
	// every job has left can be stopped whichever phase it is in (see leaveBuildsLocked).
	phase := func(kind, tag string, run func(unit string) error) error {
		unit := "offload-" + kind + "-" + hash12(tag, time.Now().String()) + ".service"
		s.mu.Lock()
		b.unit = unit
		abandoned := b.abandoned
		s.mu.Unlock()
		if abandoned {
			return errors.New("abandoned: no job wants it any more")
		}
		return run(unit)
	}
	buildBase := func(unit string) error {
		return s.runBuildUnit(unit, baseTag(), baseContainerfile, "", dir, b.logPath)
	}
	if err == nil && strings.Contains(cf, baseTag()) && b.tag != baseTag() && !imageExists(baseTag()) {
		err = phase("build", baseTag(), buildBase)
	}
	if err == nil && !imageExists(b.tag) {
		err = phase("build", b.tag, func(unit string) error { return s.runBuildUnit(unit, b.tag, cf, ctxDir, dir, b.logPath) })
	}
	if err == nil && !s.bootstrapped(b.tag) {
		// Never a project image's own bootstrap script: it would write the shared toolchains.
		if !imageExists(baseTag()) {
			err = phase("build", baseTag(), buildBase)
		}
		if err == nil {
			// One bootstrap at a time (they write the same volumes). Waiting for the lock ends if
			// the build is abandoned meanwhile; phase re-checks right before the unit starts.
			for !bootstrapMu.TryLock() {
				s.mu.Lock()
				abandoned := b.abandoned
				s.mu.Unlock()
				if abandoned {
					err = errors.New("abandoned: no job wants it any more")
					break
				}
				time.Sleep(500 * time.Millisecond)
			}
			if err == nil {
				err = phase("bootstrap", baseTag(), func(unit string) error { return s.runBootstrap(unit, baseTag(), b.logPath) })
				bootstrapMu.Unlock()
			}
		}
	}
	close(stop)
	<-pumped                    // the pump owns b.read until it has stopped
	for s.copyBuildLog(b) > 0 { // drain everything, in bounded chunks
	}
	s.mu.Lock()
	b.done, b.err = true, err
	delete(s.builds, b.tag) // a failed build is retried by the next job that needs it
	if err == nil {
		s.readyImages[b.tag] = true
		s.touchImage(b.tag)
	} else if b.abandoned {
		err = nil // stopped on purpose; whoever asks next starts it afresh
	} else {
		s.buildErrs[b.tag] = err
		for _, j := range b.waiters {
			if j.State == api.StateQueued {
				s.finishLocked(j, api.StateFailed, nil, "environment build failed: "+err.Error())
			}
		}
	}
	s.mu.Unlock()
	if err != nil {
		log.Printf("build %s: %v", b.tag, err)
	}
	s.kick()
}

func (s *Server) runBuildUnit(unit, tag, cf, ctxDir, dir, logPath string) error {
	file := filepath.Join(dir, "Containerfile."+hash12(tag))
	if err := os.WriteFile(file, []byte(cf), 0o644); err != nil {
		return err
	}
	if ctxDir == "" {
		ctxDir = filepath.Join(dir, "empty")
		_ = os.MkdirAll(ctxDir, 0o755)
	}
	args := []string{
		"--user", "--quiet", "--collect", "--wait",
		"--unit=" + unit, "--slice=" + s.cfg.Slice,
		"-p", "Delegate=yes",
		"-p", fmt.Sprintf("MemoryMax=%d", s.cfg.buildMem),
		"-p", "MemorySwapMax=0", "-p", "CPUWeight=50", "-p", "RuntimeMaxSec=7200",
		"-p", fmt.Sprintf("OOMScoreAdjust=%d", s.cfg.Policy.OOMScoreAdj),
		"-p", "StandardOutput=append:" + logPath, "-p", "StandardError=append:" + logPath,
		"--", "podman", "build", "--layers", "--network=host", "--security-opt", "label=disable",
		"--pull=missing", "-t", tag, "-f", file, ctxDir,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Hour)
	defer cancel()
	out, err := exec.CommandContext(ctx, "systemd-run", args...).CombinedOutput()
	if err != nil {
		tail := lastLines(logPath, 5)
		if tail == "" {
			tail = strings.TrimSpace(string(out))
		}
		return fmt.Errorf("podman build %s: %v: %s", tag, err, tail)
	}
	return nil
}

// snapshotContext copies the synced files under ctxRel from the slot into dst, verifying each
// file's hash against the slot record. Both sides go through os.Root.
func snapshotContext(sl *slot, ctxRel, dst string) error {
	_ = os.RemoveAll(dst)
	if err := os.MkdirAll(dst, 0o755); err != nil {
		return err
	}
	src, err := os.OpenRoot(sl.Dir)
	if err != nil {
		return err
	}
	defer src.Close()
	out, err := os.OpenRoot(dst)
	if err != nil {
		return err
	}
	defer out.Close()
	rec, err := sl.loadErr()
	if err != nil {
		return fmt.Errorf("workspace record: %w", err) // never build from a silently empty context
	}
	for p, ent := range rec.Files {
		rel := p
		if ctxRel != "." {
			var ok bool
			if rel, ok = strings.CutPrefix(p, ctxRel+"/"); !ok {
				continue
			}
		}
		if err := out.MkdirAll(path.Dir(rel), 0o755); err != nil {
			return err
		}
		if ent.Link != "" {
			if err := out.Symlink(ent.Link, rel); err != nil {
				return err
			}
			continue
		}
		fi, err := src.Lstat(p)
		if err != nil || !fi.Mode().IsRegular() {
			return fmt.Errorf("context file %s changed on the runner", p)
		}
		f, err := src.Open(p)
		if err != nil {
			return err
		}
		err = writeConfined(out, rel, f, os.FileMode(ent.Mode).Perm(), ent.Hash)
		f.Close()
		if err != nil {
			return fmt.Errorf("context file %s: %w", p, err)
		}
	}
	return nil
}

func (s *Server) bootstrapMarker(tag string) string {
	return filepath.Join(s.cfg.StateDir, "cache", ".bootstrapped-"+hash12(tag))
}

// bootstrapped reports whether the image's bootstrap has run against the current shared volumes
// (the marker lives in the cache directory, so wiping the caches re-runs it).
func (s *Server) bootstrapped(string) bool {
	// Toolchains are installed only by the runner's own base image; every environment uses them.
	_, err := os.Stat(s.bootstrapMarker(baseTag()))
	return err == nil
}

// runBootstrap runs the image's /etc/offload/bootstrap.sh (if any) with the shared volumes
// mounted, in its own unit with the build memory limit, so toolchain installs never count
// against (or get killed by) a small job's limit.
func (s *Server) runBootstrap(unit, tag, logPath string) error {
	// The caller holds bootstrapMu: two builds finishing together must not both install into
	// the same shared volumes.
	if _, err := os.Stat(s.bootstrapMarker(tag)); err == nil {
		return nil
	}
	args := []string{
		"--user", "--quiet", "--collect", "--wait",
		"--unit=" + unit, "--slice=" + s.cfg.Slice,
		"-p", "Delegate=yes", "-p", fmt.Sprintf("MemoryMax=%d", s.cfg.buildMem), "-p", "MemorySwapMax=0", "-p", "RuntimeMaxSec=3600",
		"-p", "StandardOutput=append:" + logPath, "-p", "StandardError=append:" + logPath,
		"--", "podman", "run", "--rm", "--cgroups=split", "--network=host", "--security-opt", "label=disable",
		"--pull=never", "--entrypoint", "sh",
	}
	for _, m := range s.cacheMounts() {
		args = append(args, "-v", m)
	}
	args = append(args, tag, "-c", "if [ -x /etc/offload/bootstrap.sh ]; then /etc/offload/bootstrap.sh; fi")
	ctx, cancel := context.WithTimeout(context.Background(), time.Hour)
	defer cancel()
	if out, err := exec.CommandContext(ctx, "systemd-run", args...).CombinedOutput(); err != nil {
		return fmt.Errorf("bootstrap %s: %v: %s %s", tag, err, strings.TrimSpace(string(out)), lastLines(logPath, 3))
	}
	return os.WriteFile(s.bootstrapMarker(tag), nil, 0o644)
}

var bootstrapMu sync.Mutex

// Shared toolchain volumes (written only by the runner's bootstrap, from its own base image):
// rustup toolchains and the cargo and Go bin directories, all first on PATH.
var toolchainDirs = map[string]string{"rustup": "/opt/offload/rustup", "cargo-bin": "/opt/offload/cargo/bin", "go-bin": "/opt/offload/go/bin"}

// cacheLayout versions the cache directory layout. Older layouts mounted toolchains writable into
// jobs, so on a change everything is wiped once and the bootstrap reinstalls it from scratch.
const cacheLayout = "3"

func (s *Server) cacheDir(parts ...string) string {
	return filepath.Join(append([]string{s.cfg.StateDir, "cache"}, parts...)...)
}

// resetCachesIfOld wipes caches from an older layout (they may have been tampered with).
func (s *Server) resetCachesIfOld() error {
	marker := s.cacheDir(".layout")
	if b, err := os.ReadFile(marker); err == nil && strings.TrimSpace(string(b)) == cacheLayout {
		return nil
	}
	// Running workloads have the old layout mounted: wiping it under them would pull their
	// compiler or dependencies away mid-build. Wait for them (admission stays closed meanwhile).
	s.mu.Lock()
	busy := s.allocatedBusyLocked()
	s.mu.Unlock()
	if busy > 0 {
		return fmt.Errorf("cache layout changed: waiting for %d running job(s)/session(s) to finish before resetting caches", busy)
	}
	log.Printf("cache layout changed: wiping shared toolchains and caches (the bootstrap reinstalls them)")
	if err := removeAll(s.cacheDir()); err != nil {
		return fmt.Errorf("wiping old caches: %w", err)
	}
	if err := os.MkdirAll(s.cacheDir(), 0o755); err != nil {
		return err
	}
	return os.WriteFile(marker, []byte(cacheLayout+"\n"), 0o644)
}

// stopOrphanBuilds stops build and bootstrap units a previous runner process started in our
// slice: nothing waits for them any more, and a bootstrap still writing the toolchain volumes
// must be gone before anything else touches them.
func (s *Server) stopOrphanBuilds() error {
	out, err := systemctl(context.Background(), "list-units", "--all", "--plain", "--no-legend", "--type=service",
		"offload-build-*", "offload-bootstrap-*")
	if err != nil {
		return fmt.Errorf("listing build units: %v: %s", err, out)
	}
	var problems []string
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) == 0 {
			continue
		}
		unit := f[0]
		if active, known := unitActive(unit); known && !active {
			continue // already finished
		}
		slice, err := systemctl(context.Background(), "show", "-p", "Slice", "--value", unit)
		if err != nil || slice == "" {
			problems = append(problems, fmt.Sprintf("%s: can't tell whose it is: %v", unit, err))
			continue
		}
		if slice != s.cfg.Slice {
			continue // another runner's (a dev runner uses its own slice)
		}
		log.Printf("stopping %s: left over from a previous runner process", unit)
		if err := stopUnitConfirmed(unit); err != nil {
			problems = append(problems, err.Error())
		}
	}
	if len(problems) > 0 {
		return errors.New(strings.Join(problems, "; "))
	}
	return nil
}

// cacheMounts are the toolchain volumes for the runner's bootstrap step, which installs them:
// writable, and only ever used with the runner's own base image.
func (s *Server) cacheMounts() []string {
	var out []string
	for name, dst := range toolchainDirs {
		_ = os.MkdirAll(s.cacheDir(name), 0o755)
		out = append(out, s.cacheDir(name)+":"+dst)
	}
	return out
}

// workloadMounts are the volumes a job or session sees. Toolchains are read-only. Dependency and
// build caches (cargo registry and git checkouts, Go modules and build cache) are writable, so
// they are scoped: workloads of the same repository share them (as they already share target/),
// other repositories never do. CARGO_HOME itself is not shared, so a job's cargo config or
// credentials never reach another. npm's cache holds runnable tool trees (npx), so it is scoped
// too. scope "" (no workspace) gets no persistent caches at all.
func (s *Server) workloadMounts(scope string) []string {
	var out []string
	for name, dst := range toolchainDirs {
		_ = os.MkdirAll(s.cacheDir(name), 0o755)
		out = append(out, s.cacheDir(name)+":"+dst+":ro")
	}
	// Repository caches live in cache/scoped/<key> (aged out by GC), session caches in
	// cache/sessions/<profile> (scope "session:<profile>"): distinct trees, no name games.
	base := ""
	if name, ok := strings.CutPrefix(scope, "session:"); ok && validKey(name) {
		base = s.cacheDir("sessions", name)
	} else if scope != "" && validKey(scope) {
		base = s.cacheDir("scoped", scope)
	}
	if base != "" {
		for name, dst := range map[string]string{
			"cargo-registry": "/opt/offload/cargo/registry", "cargo-git": "/opt/offload/cargo/git",
			"go-mod": "/opt/offload/go/pkg/mod", "go-build": "/opt/offload/go/cache", "npm": "/opt/offload/npm",
		} {
			dir := filepath.Join(base, name)
			_ = os.MkdirAll(dir, 0o755)
			out = append(out, dir+":"+dst)
		}
	}
	return out
}

// lastLines returns the last n lines of a file, reading at most its final 16 KiB.
func lastLines(path string, n int) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	b := make([]byte, 16<<10)
	off := int64(0)
	if fi, err := f.Stat(); err == nil && fi.Size() > int64(len(b)) {
		off = fi.Size() - int64(len(b))
	}
	k, _ := f.ReadAt(b, off)
	b = b[:k]
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, " | ")
}

func (s *Server) pumpBuildLog(b *envBuild, stop chan struct{}) {
	for {
		select {
		case <-stop:
			return
		case <-time.After(500 * time.Millisecond):
		}
		s.copyBuildLog(b)
	}
}

// copyBuildLog appends new build output (at most 1 MiB per call, read outside s.mu) to the
// stderr of every job waiting on the build. Jobs that join mid-build see output from then on.
func (s *Server) copyBuildLog(b *envBuild) int {
	f, err := os.Open(b.logPath)
	if err != nil {
		return 0
	}
	chunk := make([]byte, 1<<20)
	n, _ := f.ReadAt(chunk, b.read)
	f.Close()
	if n == 0 {
		return 0
	}
	b.read += int64(n)
	s.mu.Lock()
	var dirs []string
	for _, j := range b.waiters {
		dirs = append(dirs, j.dir)
	}
	s.mu.Unlock()
	for _, d := range dirs {
		if f, err := os.OpenFile(filepath.Join(d, "stderr.log"), os.O_WRONLY|os.O_APPEND, 0o600); err == nil {
			f.Write(chunk[:n])
			f.Close()
		}
	}
	return n
}

// prebuildBase builds the base image in the background at startup so the first job doesn't wait.
func (s *Server) prebuildBase() {
	s.mu.Lock()
	blocked := s.retireErr != ""
	s.mu.Unlock()
	if blocked {
		return // started again once startup reconciliation succeeds
	}
	if !s.containersAvailable() || (imageExists(baseTag()) && s.bootstrapped(baseTag())) {
		s.mu.Lock()
		s.readyImages[baseTag()] = s.containersAvailable()
		s.mu.Unlock()
		return
	}
	s.mu.Lock()
	if s.builds[baseTag()] != nil {
		s.mu.Unlock()
		return
	}
	if ok, _ := s.fitsLocked(api.Resources{Mem: s.cfg.buildMem}); !ok {
		s.mu.Unlock()
		return // no room now; the first job that needs it builds it once there is
	}
	b := &envBuild{tag: baseTag(), waiters: map[string]*job{}, prebuild: true}
	s.builds[baseTag()] = b
	s.mu.Unlock()
	log.Printf("building base environment %s", baseTag())
	s.build(b, baseContainerfile)
}

// --- running a job in a container ---

// runSpec describes one podman run for a job or session.
type runSpec struct {
	Name, Image, Workdir, CPUs, Secrets string
	Mem                                 int64
	Same                                []string // host paths mounted at the same path
	ReadOnly                            []string // host paths mounted read-only at the same path
	Volumes                             []string // "host:container"
	Devices                             []string
	Env                                 map[string]string
	Interactive                         bool   // keep stdin (sessions)
	NestedSandbox                       bool   // flatpak/bubblewrap inside: unmask /proc, no seccomp filter
	ClientNet                           bool   // network through the client (own netns + TUN + user-space stack)
	CacheScope                          string // which scoped caches to mount (see workloadMounts)
	DNSSearch                           []string
	Argv                                []string
}

var basePathRE = regexp.MustCompile(`(?:^|\s)PATH=(\S+)`) // PATH=, not GOPATH=

// stockInstallEnv points `go install` and `cargo install` at writable places (the container's
// own, on PATH) for images that leave that to us: the stock base, or one built on it that sets
// none of GOPATH, GOBIN, CARGO_INSTALL_ROOT or PATH. The shared toolchain bins are read-only.
// An image that decides for itself is left alone.
func stockInstallEnv(def *api.EnvDef) map[string]string {
	set := map[string]bool{}
	if def != nil {
		// The final stage (and those it builds on) must come from offload/base: a builder stage
		// on it says nothing about an image whose final stage is Debian with its own rustc.
		cf := finalStage(def.Containerfile)
		if !fromBaseRE.MatchString(cf) && !strings.Contains(cf, imageRepo+"/base") {
			return nil
		}
		set = envKeys(cf)
	}
	m := basePathRE.FindStringSubmatch(baseContainerfile)
	if m == nil || !strings.Contains(m[1], "/usr/bin") {
		return nil // never hand a workload a PATH without the system directories
	}
	env := map[string]string{}
	if !set["GOPATH"] && !set["GOBIN"] && !set["PATH"] { // each default only where the image is silent
		env["GOPATH"], env["PATH"] = "/root/go", "/root/go/bin:"+m[1]
	}
	if !set["CARGO_INSTALL_ROOT"] {
		env["CARGO_INSTALL_ROOT"] = "/usr/local"
	}
	return env
}

var fromLineRE = regexp.MustCompile(`(?mi)^\s*FROM\s+(?:--\S+\s+)*(\S+)(?:\s+AS\s+(\S+))?`)

// finalStage is the text of the stage that makes the image, with the stages it builds on (FROM
// an earlier stage's name): an ENV in an unrelated builder stage doesn't reach the image.
func finalStage(cf string) string {
	locs := fromLineRE.FindAllStringSubmatchIndex(cf, -1)
	if len(locs) == 0 {
		return cf
	}
	type stage struct{ from, name, text string }
	stages := make([]stage, len(locs))
	for i, l := range locs {
		end := len(cf)
		if i+1 < len(locs) {
			end = locs[i+1][0]
		}
		st := stage{from: cf[l[2]:l[3]], text: cf[l[0]:end]}
		if l[4] >= 0 {
			st.name = strings.ToLower(cf[l[4]:l[5]])
		}
		stages[i] = st
	}
	out := ""
	cur := len(stages) - 1
	for seen := 0; cur >= 0 && seen < len(stages); seen++ {
		out = stages[cur].text + "\n" + out
		next := -1
		for i := cur - 1; i >= 0; i-- {
			if stages[i].name != "" && stages[i].name == strings.ToLower(stages[cur].from) {
				next = i
				break
			}
		}
		cur = next
	}
	return out
}

var envLineRE = regexp.MustCompile(`(?mi)^\s*ENV\s+(.+)$`)

// envKeys are the variables a Containerfile's ENV instructions set ("ENV A=1 B=2", "ENV A 1").
func envKeys(cf string) map[string]bool {
	keys := map[string]bool{}
	for _, m := range envLineRE.FindAllStringSubmatch(strings.Join(logicalLines(cf), "\n"), -1) {
		f := strings.Fields(m[1])
		if len(f) > 0 && !strings.Contains(f[0], "=") {
			keys[f[0]] = true // legacy "ENV KEY value"
			continue
		}
		for _, w := range f {
			if k, _, ok := strings.Cut(w, "="); ok {
				keys[k] = true
			}
		}
	}
	return keys
}

// expireBuildsLocked stops builds only session launches asked for once those stopped asking
// (the client polls every few seconds while it waits; Ctrl-C ends that).
func (s *Server) expireBuildsLocked() {
	for _, b := range s.builds {
		if b.prebuild || b.done || b.abandoned || len(b.waiters) > 0 || b.sessionAsked.IsZero() {
			continue
		}
		if time.Since(b.sessionAsked) > 2*time.Minute {
			b.abandoned = true
			if b.unit != "" {
				log.Printf("build %s: no launch is waiting for it any more; stopping it", b.tag)
				stopUnit(b.unit)
			}
		}
	}
}

// leaveBuildsLocked takes a finished job off the builds it waited for. A build every job left
// (and no session asked for lately) is stopped: it would hold build_mem for nothing.
func (s *Server) leaveBuildsLocked(j *job) {
	for _, b := range s.builds {
		if _, ok := b.waiters[j.ID]; !ok {
			continue
		}
		delete(b.waiters, j.ID)
		if len(b.waiters) == 0 && b.hadWaiters && !b.done && time.Since(b.sessionAsked) > time.Minute {
			b.abandoned = true
			if b.unit != "" {
				log.Printf("build %s: no job wants it any more; stopping it", b.tag)
				stopUnit(b.unit)
			}
		}
	}
}

// ownerLabel marks this runner's containers, so cleanup never touches another runner's (two
// runners of one user share podman's container namespace).
func (s *Server) ownerLabel() string { return "dev.offload.runner=" + hash12(s.cfg.StateDir) }

// podmanRun builds the podman command line. Paths are mounted where they are on the host; the
// mounted static offload binary is the entrypoint and loads secrets inside.
func (s *Server) podmanRun(r runSpec) []string {
	// --user 0:0: workloads run as the container's root (the runner's own user outside, in
	// rootless podman), which owns the mounted workspace and temp dirs; an image's USER line
	// (a devcontainer's "node") couldn't write them.
	a := []string{"podman", "run", "--rm", "--name", r.Name, "--label", s.ownerLabel(), "--user", "0:0",
		"--cgroups=split", "--security-opt", "label=disable",
		"--pids-limit=-1", "--init", "--pull=never",
		fmt.Sprintf("--memory=%d", r.Mem), "--memory-swap=" + fmt.Sprint(r.Mem),
		"-w", r.Workdir,
	}
	if r.Interactive {
		a = append(a, "-i")
	}
	if r.ClientNet {
		// Its own network namespace with only a TUN device; DNS and "laptop" point at the client.
		// (runner inner writes resolv.conf and hosts; podman refuses --dns with --network=none)
		a = append(a, "--network=none", "--device", "/dev/net/tun", "--cap-add", "NET_ADMIN")
		if r.Env != nil {
			if len(r.DNSSearch) > 0 {
				r.Env["OFFLOAD_DNS_SEARCH"] = strings.Join(r.DNSSearch, " ")
			}
		}
	} else {
		a = append(a, "--network=host")
	}
	if r.CPUs != "" {
		a = append(a, "--cpuset-cpus="+r.CPUs)
	}
	if r.NestedSandbox {
		// bubblewrap needs to mount /proc, create namespaces and bring up loopback in its own
		// network namespace. Still rootless: these capabilities only cover namespaces the
		// container owns, not the host's.
		a = append(a, "--security-opt", "unmask=ALL", "--security-opt", "seccomp=unconfined", "--cap-add", "SYS_ADMIN", "--cap-add", "NET_ADMIN")
	}
	// Only this workload's own paths: never the runner's main socket or the shared runtime dir.
	for _, p := range r.Same {
		if _, err := os.Stat(p); err == nil {
			a = append(a, "-v", p+":"+p)
		}
	}
	for _, p := range r.ReadOnly {
		if _, err := os.Stat(p); err == nil {
			a = append(a, "-v", p+":"+p+":ro")
		}
	}
	a = append(a, "-v", s.self+":"+s.self+":ro")
	for _, m := range append(s.workloadMounts(r.CacheScope), r.Volumes...) {
		a = append(a, "-v", m)
	}
	for _, d := range r.Devices {
		a = append(a, "--device", d)
	}
	if len(r.Devices) > 0 {
		a = append(a, "--group-add", "keep-groups")
	}
	keys := make([]string, 0, len(r.Env))
	for k := range r.Env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		a = append(a, "-e", k+"="+r.Env[k])
	}
	a = append(a, "--entrypoint", s.self, r.Image, "runner", "inner")
	if r.Secrets != "" {
		a = append(a, "--secrets", r.Secrets)
	}
	if r.Interactive {
		a = append(a, "--stdin")
	}
	if r.ClientNet {
		a = append(a, "--net-client")
	}
	a = append(a, "--")
	return append(a, r.Argv...)
}

// containerArgv wraps a job command in podman run.
func (s *Server) containerArgv(j *job, env map[string]string, workdir, secrets string) []string {
	r := runSpec{Name: "offload-" + j.ID, Image: j.Image, Workdir: workdir, CPUs: j.AllowedCPUs,
		Secrets: secrets, Mem: j.Resources.Mem, Env: env, Argv: j.Spec.Argv,
		// Never the job directory itself: it holds exit.json and the job record, which the
		// container must not be able to forge.
		Same: append([]string{filepath.Join(j.dir, "tmp"), filepath.Join(j.dir, "work")}, j.mounts...)}
	if j.slot != nil {
		r.Same = append(r.Same, j.slot.Dir)
		r.ReadOnly = append(r.ReadOnly, s.sharedGit(j.Spec.WorkspaceKey)) // shared by every slot of the repo
	}
	if j.Spec.Environment != nil {
		r.Devices = j.Spec.Environment.Devices
	}
	r.ClientNet, r.DNSSearch = j.Spec.Network == "client", j.Spec.DNSSearch
	r.CacheScope = j.Spec.WorkspaceKey
	return s.podmanRun(r)
}

// sessionImageLocked returns a session environment's image tag and whether it's ready to run,
// starting a build when needed (sessions don't queue; the client retries until it's ready).
func (s *Server) sessionImageLocked(def *api.EnvDef) (string, bool, error) {
	tag, cf, _ := envPlan(def)
	if s.imageReadyLocked(tag) {
		return tag, true, nil
	}
	if b := s.builds[tag]; b != nil {
		b.sessionAsked = time.Now()
		return tag, false, nil
	}
	if err := s.buildErrs[tag]; err != nil {
		delete(s.buildErrs, tag)
		return tag, false, err
	}
	if imageExists(tag) && s.bootstrapped(tag) {
		s.readyImages[tag] = true
		return tag, true, nil
	}
	if ok, _ := s.fitsLocked(api.Resources{Mem: s.cfg.buildMem}); !ok {
		return tag, false, nil // not yet: the client retries
	}
	b := &envBuild{tag: tag, waiters: map[string]*job{}, sessionAsked: time.Now()}
	s.builds[tag] = b
	go s.build(b, cf)
	return tag, false, nil
}

// containerEnv is the environment a containerized job gets: no host PATH/HOME (the image has its
// own), but parallelism, relays, git config, profile settings and the job's own variables.
func (s *Server) containerEnv(prof *Profile, ncpu int, j *job) map[string]string {
	env := parallelEnv(ncpu)
	for k, v := range stockInstallEnv(j.Spec.Environment) {
		env[k] = v
	}
	if prof != nil {
		for k, v := range prof.Env {
			if k != "PATH" && k != "HOME" {
				env[k] = v
			}
		}
	}
	if s.tz != "" {
		env["TZ"] = s.tz
	}
	if j.Spec.Environment != nil {
		for k, v := range j.Spec.Environment.Env {
			env[k] = v
		}
	}
	return env
}

// RunInner is "offload runner inner", the entrypoint inside a job's container: load secrets from
// the tmpfs mount (never passed to podman, which would store them on disk), then run the job.
func RunInner(secrets string, stdin, clientNet bool, argv []string) int {
	if secrets != "" {
		if err := loadSecrets(secrets); err != nil {
			fmt.Fprintf(os.Stderr, "offload: %v\n", err)
			return 1
		}
	}
	if clientNet {
		if err := startClientNet(); err != nil {
			fmt.Fprintf(os.Stderr, "offload: client network: %v\n", err)
			return 1
		}
	}
	var in io.Reader
	if stdin {
		in = os.Stdin
	}
	code, _, err := runChild(argv, in, os.Stdout)
	if err != nil {
		fmt.Fprintf(os.Stderr, "offload: %v\n", err)
	}
	return code
}

// removeAll deletes a tree, falling back to `podman unshare` for files created by non-root users
// inside containers (they belong to subordinate IDs the runner user can't delete directly).
func removeAll(path string) error {
	err := os.RemoveAll(path)
	if err == nil {
		return nil
	}
	if _, lerr := exec.LookPath("podman"); lerr == nil {
		if out, uerr := exec.Command("podman", "unshare", "rm", "-rf", path).CombinedOutput(); uerr != nil {
			return fmt.Errorf("%v (podman unshare: %s)", err, strings.TrimSpace(string(out)))
		}
		return nil
	}
	return err
}

// --- image bookkeeping and garbage collection ---

func (s *Server) imagesPath() string { return filepath.Join(s.cfg.StateDir, "images.json") }

func (s *Server) touchImage(tag string) {
	if s.imageUse == nil {
		s.imageUse = map[string]time.Time{}
	}
	s.imageUse[tag] = time.Now()
	_ = writeFileJSON(s.imagesPath(), s.imageUse)
}

func (s *Server) loadImageUse() {
	s.imageUse = map[string]time.Time{}
	if b, err := os.ReadFile(s.imagesPath()); err == nil {
		_ = json.Unmarshal(b, &s.imageUse)
	}
}

// gcContainersLocked removes images unused for the retention period (never the current base)
// and leftover containers of jobs that are no longer running.
func (s *Server) gcContainersLocked() {
	// Build attempt directories left by a runner that died mid-build (each holds a full context
	// snapshot): drop those no running build owns once they're old enough to be abandoned.
	active := map[string]bool{}
	for _, b := range s.builds {
		active[b.dir] = true
	}
	if dirs, err := filepath.Glob(filepath.Join(s.cfg.StateDir, "envs", "*", "a-*")); err == nil {
		for _, d := range dirs {
			if fi, err := os.Stat(d); err == nil && !active[d] && time.Since(fi.ModTime()) > 6*time.Hour {
				_ = os.RemoveAll(d)
			}
		}
	}
	if !s.containersAvailable() {
		return
	}
	live := map[string]bool{} // images of running jobs and live sessions are in use, whatever their age
	for _, j := range s.jobs {
		if !api.Terminal(j.State) && j.Image != "" {
			live[j.Image] = true
		}
	}
	for _, se := range s.sessions {
		if !api.Terminal(se.State) && se.Image != "" {
			live[se.Image] = true
		}
	}
	for tag, at := range s.imageUse {
		if tag == baseTag() || time.Since(at) < s.cfg.imageRetain || live[tag] {
			continue
		}
		if _, err := podman(context.Background(), "rmi", "--ignore", tag); err != nil {
			continue // not removed (in use, or podman trouble): keep its record, retry next time
		}
		log.Printf("gc: removed image %s (unused since %s)", tag, at.Format(time.DateTime))
		delete(s.imageUse, tag)
		delete(s.readyImages, tag)
	}
	_ = writeFileJSON(s.imagesPath(), s.imageUse)
	out, err := podman(context.Background(), "ps", "-a", "--filter", "name=^offload-j-", "--filter", "label="+s.ownerLabel(), "--format", "{{.Names}}")
	if err == nil && out != "" {
		for _, name := range strings.Split(out, "\n") {
			if j := s.jobs[strings.TrimPrefix(name, "offload-")]; j == nil || j.State != api.StateRunning {
				_, _ = podman(context.Background(), "rm", "-f", name)
			}
		}
	}
	// Only dangling layers of our own images (they all carry the base's label): never images
	// someone builds with podman on this account for other reasons.
	_, _ = podman(context.Background(), "image", "prune", "-f", "--filter", "label=dev.offload.base=1")
}
