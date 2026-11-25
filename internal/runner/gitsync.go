package runner

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/RestartFU/offload/internal/api"
)

// Git metadata for workspaces. Every workspace key has one bare object store (shared.git); each
// slot is a normal repository whose objects come from it through alternates. The client sends
// only commits the store is missing, as a git bundle, so the runner never needs credentials.

var shaRE = regexp.MustCompile(`^[0-9a-f]{40}([0-9a-f]{24})?$`)

func validGitRef(g *api.GitRef) bool {
	return shaRE.MatchString(g.Head) && (g.Branch == "" || validBranch(g.Branch))
}

// validBranch applies git's own ref-name rules (feature/café is fine, "a..b" isn't).
func validBranch(b string) bool {
	if len(b) > 255 || strings.HasPrefix(b, "-") || strings.ContainsAny(b, "\x00\n") {
		return false
	}
	if _, err := exec.LookPath("git"); err != nil {
		return true // no git on this host: the job runs without .git anyway (see gitPlan)
	}
	_, err := gitCmd(".", "check-ref-format", "refs/heads/"+b)
	return err == nil
}

func (s *Server) sharedGit(key string) string {
	return filepath.Join(s.cfg.StateDir, "work", key, "shared.git")
}

func gitCmd(dir string, args ...string) (string, error) {
	return gitCmdCtx(context.Background(), dir, args...)
}

// gitCmdCtx runs git in its own process group, killed as a whole (index-pack and other helpers
// included) when ctx ends or after five minutes.
func gitCmdCtx(ctx context.Context, dir string, args ...string) (string, error) {
	return gitCmdIn(ctx, dir, "", args...)
}

// gitCmdIn is gitCmdCtx with stdin.
func gitCmdIn(ctx context.Context, dir, stdin string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...)
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = 5 * time.Second
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "GIT_CONFIG_NOSYSTEM=1")
	out, err := cmd.CombinedOutput()
	if cmd.Process != nil {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) // leftovers of the group
	}
	if err != nil {
		return "", fmt.Errorf("git %s: %v: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return strings.TrimSpace(string(out)), nil
}

// ensureShared creates the per-key object store if needed.
func (s *Server) ensureShared(key string) (string, error) {
	shared := s.sharedGit(key)
	if _, err := os.Stat(filepath.Join(shared, "objects")); err == nil {
		return shared, nil
	}
	if err := os.MkdirAll(shared, 0o755); err != nil {
		return "", err
	}
	if _, err := gitCmd(shared, "init", "-q", "--bare"); err != nil {
		return "", err
	}
	_, _ = gitCmd(shared, "config", "gc.auto", "0") // refs are pruned by us; avoid surprise repacks mid-job
	return shared, nil
}

// gitPlan reports whether the store lacks head, and which commits it has (newest first) so
// the client can build a minimal bundle.
func (s *Server) gitPlan(key, head string, tags map[string]string) (needed bool, have []string, err error) {
	shared, err := s.ensureShared(key)
	if err != nil {
		return false, nil, err
	}
	if _, err := gitCmd(shared, "cat-file", "-e", head+"^{commit}"); err == nil && len(missingObjects(shared, tags)) == 0 {
		return false, nil, nil
	}
	out, err := gitCmd(shared, "for-each-ref", "--sort=-committerdate", "--count=64", "--format=%(objectname)", "refs/offload/")
	if err != nil {
		return true, nil, nil
	}
	if out != "" {
		have = strings.Split(out, "\n")
	}
	return true, have, nil
}

// gitReceive fetches an uploaded bundle into the store and pins head with a ref.
// gitLocks serializes bundle imports per repository: clearing the store's tag refs and fetching
// must not interleave with another job's.
var gitLocks sync.Map

func (s *Server) gitReceive(ctx context.Context, key, head, bundle string) error {
	l, _ := gitLocks.LoadOrStore(key, make(chan struct{}, 1))
	sem := l.(chan struct{})
	select { // a cancelled job stops waiting (and frees its slot) at once
	case sem <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	}
	defer func() { <-sem }()
	shared, err := s.ensureShared(key)
	if err != nil {
		return err
	}
	if _, err := gitCmdCtx(ctx, shared, "bundle", "verify", "-q", bundle); err != nil {
		return err
	}
	// The store's tag refs only carry tag objects in; the slot gets the client's names. Clear
	// them first, so a renamed tag (release -> release/v2) can't block the fetch. The objects
	// stay (nothing prunes them), and missingObjects looks at objects, not refs.
	if refs, err := gitCmdCtx(ctx, shared, "for-each-ref", "--format=delete %(refname)", "refs/tags"); err == nil && refs != "" {
		_, _ = gitCmdIn(ctx, shared, refs+"\n", "update-ref", "--stdin")
	}
	// HEAD is in the bundle unless the store already had it (then only tags are new).
	refspecs := []string{"+refs/tags/*:refs/tags/*"}
	if heads, err := gitCmdCtx(ctx, shared, "bundle", "list-heads", bundle); err == nil {
		for _, line := range strings.Split(heads, "\n") {
			if strings.HasSuffix(line, " HEAD") {
				refspecs = append(refspecs, "+HEAD:refs/offload/"+head)
			}
		}
	}
	if _, err := gitCmdCtx(ctx, shared, append([]string{"fetch", "-q", "--no-tags", bundle}, refspecs...)...); err != nil {
		return err
	}
	if _, err := gitCmdCtx(ctx, shared, "update-ref", "refs/offload/"+head, head); err != nil {
		return fmt.Errorf("bundle did not contain %s", head)
	}
	if _, err := gitCmdCtx(ctx, shared, "cat-file", "-e", head+"^{commit}"); err != nil {
		return fmt.Errorf("bundle did not contain %s", head)
	}
	s.pruneGitRefs(ctx, shared)
	return nil
}

// pruneGitRefs keeps the 64 newest pinned commits (they are what we advertise as "have").
func (s *Server) pruneGitRefs(ctx context.Context, shared string) {
	out, err := gitCmdCtx(ctx, shared, "for-each-ref", "--sort=-committerdate", "--format=%(refname)", "refs/offload/")
	if err != nil {
		return
	}
	refs := strings.Split(out, "\n")
	for i := 64; i < len(refs); i++ {
		_, _ = gitCmdCtx(ctx, shared, "update-ref", "-d", refs[i])
	}
}

// gitCheckout makes the slot a repository at head without touching its files (they were just
// synced from the client): HEAD and branch point at head, and the index matches head, so
// `git status` / `git diff` show the client's uncommitted changes as working-tree changes.
func (s *Server) gitCheckout(ctx context.Context, sl *slot, key string, g *api.GitRef) error {
	shared, err := s.ensureShared(key)
	if err != nil {
		return err
	}
	if _, err := gitCmdCtx(ctx, shared, "cat-file", "-e", g.Head+"^{commit}"); err != nil {
		return fmt.Errorf("commit %s was not uploaded", g.Head[:12])
	}
	// A fresh .git for every job: a previous job may have replaced parts of it with symlinks, and
	// the runner is about to run git there. Objects live in the (read-only mounted) shared store,
	// so this is cheap.
	gitDir := filepath.Join(sl.Dir, ".git")
	root, err := os.OpenRoot(sl.Dir)
	if err != nil {
		return err
	}
	err = root.RemoveAll(".git")
	root.Close()
	if err != nil {
		return err
	}
	if _, err := gitCmdCtx(ctx, sl.Dir, "init", "-q"); err != nil {
		return err
	}
	alt := filepath.Join(gitDir, "objects", "info", "alternates")
	if err := os.MkdirAll(filepath.Dir(alt), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(alt, []byte(filepath.Join(shared, "objects")+"\n"), 0o644); err != nil {
		return err
	}
	if g.Branch != "" {
		if _, err := gitCmdCtx(ctx, sl.Dir, "update-ref", "refs/heads/"+g.Branch, g.Head); err != nil {
			return err
		}
		if _, err := gitCmdCtx(ctx, sl.Dir, "symbolic-ref", "HEAD", "refs/heads/"+g.Branch); err != nil {
			return err
		}
	} else if _, err := gitCmdCtx(ctx, sl.Dir, "update-ref", "--no-deref", "HEAD", g.Head); err != nil {
		return err
	}
	if _, err := gitCmdCtx(ctx, sl.Dir, "read-tree", "HEAD"); err != nil {
		return err
	}
	// The client's tags (those reachable from HEAD), for `git describe`; ones whose objects
	// never arrived are left out rather than failing the checkout.
	if len(g.Tags) > 0 {
		missing := map[string]bool{}
		for _, id := range missingObjects(shared, g.Tags) {
			missing[id] = true
		}
		var cmds strings.Builder
		for name, id := range g.Tags {
			if !missing[id] && validTagName(name) {
				fmt.Fprintf(&cmds, "create refs/tags/%s %s\n", name, id)
			}
		}
		if cmds.Len() > 0 {
			_, _ = gitCmdIn(ctx, sl.Dir, cmds.String(), "update-ref", "--stdin")
		}
	}
	_, _ = gitCmdCtx(ctx, sl.Dir, "update-index", "-q", "--refresh")
	return nil
}

// missingObjects returns the tag object ids not in the store.
func missingObjects(shared string, tags map[string]string) []string {
	if len(tags) == 0 {
		return nil
	}
	var in strings.Builder
	for _, id := range tags {
		in.WriteString(id + "\n")
	}
	out, err := gitCmdIn(context.Background(), shared, in.String(), "cat-file", "--batch-check=%(objectname)")
	if err != nil {
		return []string{"?"}
	}
	var missing []string
	for _, line := range strings.Split(out, "\n") {
		if id, ok := strings.CutSuffix(strings.TrimSpace(line), " missing"); ok {
			missing = append(missing, id)
		}
	}
	return missing
}

func validTagName(n string) bool {
	return n != "" && !strings.ContainsAny(n, " \t\n\x00~^:?*[\\") && !strings.Contains(n, "..") && !strings.HasPrefix(n, "/") && !strings.HasPrefix(n, "-")
}

// pruneUntracked removes files a previous job created in the slot that git doesn't ignore and
// the client didn't send (generated sources, say): they would otherwise survive into builds of
// other branches. Ignored files (target/, node_modules/ …) stay; that's what keeps slots warm.
func (s *Server) pruneUntracked(ctx context.Context, sl *slot, manifest []api.FileEntry, ignoreRules string) {
	// A scratch repository with an empty index: everything in the slot is "untracked", minus
	// what the tree's .gitignore files ignore, whether or not this job gets a .git.
	scratch, err := os.MkdirTemp("", "offload-prune-")
	if err != nil {
		return
	}
	defer os.RemoveAll(scratch)
	if _, err := gitCmdCtx(ctx, scratch, "init", "-q", "--bare"); err != nil {
		return
	}
	// The client's rules from outside the tree (.git/info/exclude, its global excludes file):
	// what it ignores by those stays too.
	if ignoreRules != "" {
		_ = os.MkdirAll(filepath.Join(scratch, "info"), 0o755)
		_ = os.WriteFile(filepath.Join(scratch, "info", "exclude"), []byte(ignoreRules), 0o644)
	}
	out, err := gitCmdCtx(ctx, sl.Dir, "--git-dir="+scratch, "--work-tree=.", "ls-files", "-z", "--others", "--exclude-standard", "--exclude=/.git")
	if err != nil || out == "" {
		return
	}
	sent := make(map[string]bool, len(manifest))
	for _, e := range manifest {
		sent[e.Path] = true
	}

	root, err := os.OpenRoot(sl.Dir)
	if err != nil {
		return
	}
	defer root.Close()
	for _, p := range strings.Split(out, "\x00") {
		if p == "" || sent[p] || strings.HasPrefix(p, ".git/") {
			continue
		}
		if _, err := cleanRel(p); err == nil && parentsState(root, p) == parentsReal {
			if root.Remove(p) == nil {
				// Directories this leaves empty go too: an empty vendor/ still switches Go to
				// vendor mode. Removal stops at the first directory that still has anything.
				for d := path.Dir(p); d != "." && d != "/"; d = path.Dir(d) {
					if root.Remove(d) != nil {
						break
					}
				}
			}
		}
	}
}
