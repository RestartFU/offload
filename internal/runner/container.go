package runner

import (
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
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
	sent    map[string]int64 // bytes of the log already copied to each waiter
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

func usesContext(cf string) bool {
	re := regexp.MustCompile(`(?mi)^\s*(COPY|ADD)\s`)
	return re.MatchString(cf)
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
	if j.contextBuilt {
		return true
	}
	tag, cf, _ := envPlan(j.Spec.Environment)
	j.Image = tag
	ctxDir := ""
	if j.Spec.Environment != nil && usesContext(cf) && j.slot != nil {
		ctxDir = filepath.Join(j.slot.Dir, filepath.FromSlash(j.Spec.Environment.ContextDir))
	}
	if b := s.builds[tag]; b != nil {
		b.waiters[j.ID] = j
		return false
	}
	if ctxDir == "" && s.readyImages[tag] {
		return true
	}
	if ctxDir == "" && imageExists(tag) && s.bootstrapped(tag) {
		s.readyImages[tag] = true
		s.touchImage(tag)
		return true
	}
	b := &envBuild{tag: tag, waiters: map[string]*job{j.ID: j}, sent: map[string]int64{}}
	s.builds[tag] = b
	name := "base"
	if j.Spec.Environment != nil {
		name = j.Spec.Environment.Name
	}
	s.note(j, fmt.Sprintf("building environment %s on %s (cached layers make later builds fast)", name, s.cfg.Name))
	go s.build(b, cf, ctxDir)
	return false
}

// build builds an image (and the base first, when needed) in its own unit with its own limits,
// so a job's small learned memory limit can't starve its environment build.
func (s *Server) build(b *envBuild, cf, ctxDir string) {
	dir := filepath.Join(s.cfg.StateDir, "envs", strings.ReplaceAll(strings.TrimPrefix(b.tag, imageRepo+"/"), ":", "-"))
	_ = os.MkdirAll(dir, 0o755)
	b.logPath = filepath.Join(dir, "build.log")
	_ = os.WriteFile(b.logPath, nil, 0o644)
	stop := make(chan struct{})
	go s.pumpBuildLog(b, stop)
	var err error
	if strings.Contains(cf, baseTag()) && b.tag != baseTag() && !imageExists(baseTag()) {
		err = s.runBuild(baseTag(), baseContainerfile, "", dir, b.logPath)
	}
	if err == nil && (ctxDir != "" || !imageExists(b.tag)) {
		err = s.runBuild(b.tag, cf, ctxDir, dir, b.logPath)
	}
	if err == nil && !s.bootstrapped(b.tag) {
		err = s.runBootstrap(b.tag, b.logPath)
	}
	close(stop)
	s.mu.Lock()
	s.copyBuildLogLocked(b)
	b.done, b.err = true, err
	delete(s.builds, b.tag) // a failed build is retried by the next job that needs it
	if err == nil {
		s.readyImages[b.tag] = ctxDir == ""
		s.touchImage(b.tag)
		if ctxDir != "" {
			for _, j := range b.waiters {
				j.contextBuilt = true // built from this job's own context; start it
			}
		}
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

func (s *Server) runBuild(tag, cf, ctxDir, dir, logPath string) error {
	file := filepath.Join(dir, "Containerfile."+hash12(tag))
	if err := os.WriteFile(file, []byte(cf), 0o644); err != nil {
		return err
	}
	if ctxDir == "" {
		ctxDir = filepath.Join(dir, "empty")
		_ = os.MkdirAll(ctxDir, 0o755)
	}
	unit := "offload-build-" + hash12(tag, time.Now().String()) + ".service"
	args := []string{
		"--user", "--quiet", "--collect", "--wait",
		"--unit=" + unit, "--slice=" + s.cfg.Slice,
		"-p", "Delegate=yes",
		"-p", fmt.Sprintf("MemoryMax=%d", s.cfg.buildMem),
		"-p", "MemorySwapMax=0", "-p", "CPUWeight=50",
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

func (s *Server) bootstrapMarker(tag string) string {
	return filepath.Join(s.cfg.StateDir, "cache", ".bootstrapped-"+hash12(tag))
}

// bootstrapped reports whether the image's bootstrap has run against the current shared volumes
// (the marker lives in the cache directory, so wiping the caches re-runs it).
func (s *Server) bootstrapped(tag string) bool {
	_, err := os.Stat(s.bootstrapMarker(tag))
	return err == nil
}

// runBootstrap runs the image's /etc/offload/bootstrap.sh (if any) with the shared volumes
// mounted, in its own unit with the build memory limit, so toolchain installs never count
// against (or get killed by) a small job's limit.
func (s *Server) runBootstrap(tag, logPath string) error {
	args := []string{
		"--user", "--quiet", "--collect", "--wait",
		"--unit=offload-bootstrap-" + hash12(tag, time.Now().String()) + ".service", "--slice=" + s.cfg.Slice,
		"-p", "Delegate=yes", "-p", fmt.Sprintf("MemoryMax=%d", s.cfg.buildMem), "-p", "MemorySwapMax=0",
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

// cacheMounts are the shared toolchain/cache volumes every environment gets.
func (s *Server) cacheMounts() []string {
	var out []string
	for _, c := range []string{"rustup", "cargo", "go", "npm", "pip"} {
		dir := filepath.Join(s.cfg.StateDir, "cache", c)
		_ = os.MkdirAll(dir, 0o755)
		out = append(out, dir+":/opt/offload/"+c)
	}
	return out
}

func lastLines(path string, n int) string {
	b, _ := os.ReadFile(path)
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
		s.mu.Lock()
		s.copyBuildLogLocked(b)
		s.mu.Unlock()
	}
}

// copyBuildLogLocked appends new build output to every waiting job's stderr.
func (s *Server) copyBuildLogLocked(b *envBuild) {
	data, err := os.ReadFile(b.logPath)
	if err != nil {
		return
	}
	for id, j := range b.waiters {
		from := b.sent[id]
		if int64(len(data)) <= from {
			continue
		}
		f, err := os.OpenFile(filepath.Join(j.dir, "stderr.log"), os.O_WRONLY|os.O_APPEND, 0o600)
		if err != nil {
			continue
		}
		f.Write(data[from:])
		f.Close()
		b.sent[id] = int64(len(data))
	}
}

// prebuildBase builds the base image in the background at startup so the first job doesn't wait.
func (s *Server) prebuildBase() {
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
	b := &envBuild{tag: baseTag(), waiters: map[string]*job{}, sent: map[string]int64{}}
	s.builds[baseTag()] = b
	s.mu.Unlock()
	log.Printf("building base environment %s", baseTag())
	s.build(b, baseContainerfile, "")
}

// --- running a job in a container ---

// runSpec describes one podman run for a job or session.
type runSpec struct {
	Name, Image, Workdir, CPUs, Secrets string
	Mem                                 int64
	Same                                []string // host paths mounted at the same path
	Volumes                             []string // "host:container"
	Devices                             []string
	Env                                 map[string]string
	Interactive                         bool // keep stdin (sessions)
	NestedSandbox                       bool // flatpak/bubblewrap inside: unmask /proc, no seccomp filter
	ClientNet                           bool // network through the client (own netns + TUN + user-space stack)
	DNSSearch                           []string
	Argv                                []string
}

// podmanRun builds the podman command line. Paths are mounted where they are on the host; the
// mounted static offload binary is the entrypoint and loads secrets inside.
func (s *Server) podmanRun(r runSpec) []string {
	a := []string{"podman", "run", "--rm", "--name", r.Name,
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
			r.Env["OFFLOAD_RUNNER_SOCK"] = unixSocketPath()
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
	same := append(r.Same, runtimeDir()) // relay sockets, runner socket, secrets
	for _, p := range same {
		if _, err := os.Stat(p); err == nil {
			a = append(a, "-v", p+":"+p)
		}
	}
	a = append(a, "-v", s.self+":"+s.self+":ro")
	for _, m := range append(s.cacheMounts(), r.Volumes...) {
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
		Secrets: secrets, Mem: j.Resources.Mem, Env: env, Argv: j.Spec.Argv, Same: []string{j.dir}}
	if j.slot != nil {
		r.Same = append(r.Same, j.slot.Dir, s.sharedGit(j.Spec.WorkspaceKey))
	}
	if j.Spec.Environment != nil {
		r.Devices = j.Spec.Environment.Devices
	}
	r.ClientNet, r.DNSSearch = j.Spec.Network == "client", j.Spec.DNSSearch
	return s.podmanRun(r)
}

// sessionImageLocked returns a session environment's image tag and whether it's ready to run,
// starting a build when needed (sessions don't queue; the client retries until it's ready).
func (s *Server) sessionImageLocked(def *api.EnvDef) (string, bool, error) {
	tag, cf, _ := envPlan(def)
	if s.readyImages[tag] {
		return tag, true, nil
	}
	if s.builds[tag] != nil {
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
	b := &envBuild{tag: tag, waiters: map[string]*job{}, sent: map[string]int64{}}
	s.builds[tag] = b
	go s.build(b, cf, "")
	return tag, false, nil
}

// containerEnv is the environment a containerized job gets: no host PATH/HOME (the image has its
// own), but parallelism, relays, git config, profile settings and the job's own variables.
func (s *Server) containerEnv(prof *Profile, ncpu int, j *job) map[string]string {
	env := parallelEnv(ncpu)
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
		loadSecrets(secrets)
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
	if !s.containersAvailable() {
		return
	}
	for tag, at := range s.imageUse {
		if tag == baseTag() || time.Since(at) < s.cfg.imageRetain {
			continue
		}
		if _, err := podman(context.Background(), "rmi", tag); err == nil {
			log.Printf("gc: removed image %s (unused since %s)", tag, at.Format(time.DateTime))
		}
		delete(s.imageUse, tag)
		delete(s.readyImages, tag)
	}
	_ = writeFileJSON(s.imagesPath(), s.imageUse)
	out, err := podman(context.Background(), "ps", "-a", "--filter", "name=^offload-j-", "--format", "{{.Names}}")
	if err == nil && out != "" {
		for _, name := range strings.Split(out, "\n") {
			if j := s.jobs[strings.TrimPrefix(name, "offload-")]; j == nil || j.State != api.StateRunning {
				_, _ = podman(context.Background(), "rm", "-f", name)
			}
		}
	}
	_, _ = podman(context.Background(), "image", "prune", "-f")
}
