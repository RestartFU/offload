package runner

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/RestartFU/offload/internal/api"
)

// Git metadata for workspaces. Every workspace key has one bare object store (shared.git); each
// slot is a normal repository whose objects come from it through alternates. The client sends
// only commits the store is missing, as a git bundle, so the runner never needs credentials.

var shaRE = regexp.MustCompile(`^[0-9a-f]{40}([0-9a-f]{24})?$`)
var branchRE = regexp.MustCompile(`^[A-Za-z0-9._/@+-]{1,200}$`)

func validGitRef(g *api.GitRef) bool {
	return shaRE.MatchString(g.Head) && (g.Branch == "" || (branchRE.MatchString(g.Branch) && !strings.Contains(g.Branch, "..")))
}

func (s *Server) sharedGit(key string) string {
	return filepath.Join(s.cfg.StateDir, "work", key, "shared.git")
}

func gitCmd(dir string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "GIT_CONFIG_NOSYSTEM=1")
	out, err := cmd.CombinedOutput()
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
func (s *Server) gitPlan(key, head string) (needed bool, have []string, err error) {
	shared, err := s.ensureShared(key)
	if err != nil {
		return false, nil, err
	}
	if _, err := gitCmd(shared, "cat-file", "-e", head+"^{commit}"); err == nil {
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
func (s *Server) gitReceive(key, head, bundle string) error {
	shared, err := s.ensureShared(key)
	if err != nil {
		return err
	}
	if _, err := gitCmd(shared, "bundle", "verify", "-q", bundle); err != nil {
		return err
	}
	if _, err := gitCmd(shared, "fetch", "-q", "--no-tags", bundle, "+HEAD:refs/offload/"+head); err != nil {
		return err
	}
	if _, err := gitCmd(shared, "cat-file", "-e", head+"^{commit}"); err != nil {
		return fmt.Errorf("bundle did not contain %s", head)
	}
	s.pruneGitRefs(shared)
	return nil
}

// pruneGitRefs keeps the 64 newest pinned commits (they are what we advertise as "have").
func (s *Server) pruneGitRefs(shared string) {
	out, err := gitCmd(shared, "for-each-ref", "--sort=-committerdate", "--format=%(refname)", "refs/offload/")
	if err != nil {
		return
	}
	refs := strings.Split(out, "\n")
	for i := 64; i < len(refs); i++ {
		_, _ = gitCmd(shared, "update-ref", "-d", refs[i])
	}
}

// gitCheckout makes the slot a repository at head without touching its files (they were just
// synced from the client): HEAD and branch point at head, and the index matches head, so
// `git status` / `git diff` show the client's uncommitted changes as working-tree changes.
func (s *Server) gitCheckout(sl *slot, key string, g *api.GitRef) error {
	shared, err := s.ensureShared(key)
	if err != nil {
		return err
	}
	if _, err := gitCmd(shared, "cat-file", "-e", g.Head+"^{commit}"); err != nil {
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
	if _, err := gitCmd(sl.Dir, "init", "-q"); err != nil {
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
		if _, err := gitCmd(sl.Dir, "update-ref", "refs/heads/"+g.Branch, g.Head); err != nil {
			return err
		}
		if _, err := gitCmd(sl.Dir, "symbolic-ref", "HEAD", "refs/heads/"+g.Branch); err != nil {
			return err
		}
	} else if _, err := gitCmd(sl.Dir, "update-ref", "--no-deref", "HEAD", g.Head); err != nil {
		return err
	}
	if _, err := gitCmd(sl.Dir, "read-tree", "HEAD"); err != nil {
		return err
	}
	_, _ = gitCmd(sl.Dir, "update-index", "-q", "--refresh")
	return nil
}
