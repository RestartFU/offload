package client

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"sort"
	"strings"
	"sync"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/RestartFU/offload/internal/api"
	"github.com/RestartFU/offload/internal/units"
)

// GitHubOptions are the flags of `offload github add`.
type GitHubOptions struct {
	Owner     string
	Token     string // a token or a cmd:/env:/file: source; empty uses the gh login
	Labels    []string
	Max       int
	On        []string
	Resources api.Resources
	Shared    *bool // --shared-cache, when given
}

// githubToken resolves the token to hand runners, and where it came from ("gh" or "token").
// A gh login is checked for the scope GitHub wants for managing the owner's runners.
func githubToken(ctx context.Context, owner, src string) (token, from string, err error) {
	if src != "" {
		token, err = resolveValue(src)
		return strings.TrimSpace(token), "token", err
	}
	if _, err := exec.LookPath("gh"); err != nil {
		return "", "", errors.New("offload uses your GitHub CLI login, and gh isn't installed: install it and run `gh auth login`, or pass --token")
	}
	out, err := exec.CommandContext(ctx, "gh", "auth", "token", "--hostname", "github.com").Output()
	token = strings.TrimSpace(string(out))
	if err != nil || token == "" {
		return "", "", errors.New("you're not logged in to the GitHub CLI: run `gh auth login`, or pass --token")
	}
	need := "admin:org"
	if strings.Contains(owner, "/") {
		need = "repo"
	}
	scopes, login, err := githubScopes(ctx, token)
	if err != nil {
		return "", "", fmt.Errorf("checking your gh login with GitHub: %w", err)
	}
	if scopes != nil && !scopes[need] {
		return "", "", fmt.Errorf("your gh login (%s) can't manage the runners of %s: GitHub wants the %s scope for that. Grant it with:\n\n    gh auth refresh -h github.com -s %s\n\nthen run this again", login, owner, need, need)
	}
	return token, "gh", nil
}

// githubScopes returns a classic or OAuth token's scopes (nil for a fine-grained token, which
// has none) and its login.
func githubScopes(ctx context.Context, token string) (map[string]bool, string, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", "https://api.github.com/user", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, "", fmt.Errorf("GitHub answered %s", resp.Status)
	}
	var u struct {
		Login string `json:"login"`
	}
	_ = json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&u)
	h, ok := resp.Header["X-Oauth-Scopes"]
	if !ok {
		return nil, u.Login, nil
	}
	scopes := map[string]bool{}
	for _, s := range strings.Split(strings.Join(h, ","), ",") {
		scopes[strings.TrimSpace(s)] = true
	}
	if scopes["admin:org"] {
		scopes["write:org"], scopes["read:org"] = true, true
	}
	return scopes, u.Login, nil
}

// githubHosts are the runners to use: --on, or every one that can run GitHub jobs (containers;
// never this machine's own runner, which sleeps with it).
func githubHosts(hosts []*Host, on []string, forAdd bool) ([]*Host, error) {
	if len(on) > 0 {
		var out []*Host
		for _, name := range on {
			h, err := FindHost(hosts, name)
			if err != nil {
				return nil, err
			}
			if err := checkProto(h); err != nil {
				return nil, err
			}
			if h.derived && h.Token == "" { // discovered isn't paired: it must never see the token
				return nil, fmt.Errorf("%s isn't paired with this machine: offload host pair %s --code CODE", h.Name, h.Name)
			}
			out = append(out, h)
		}
		return out, nil
	}
	var out []*Host
	seen := map[string]bool{}
	for _, h := range hosts {
		if h.Status == nil || seen[h.Status.ID] || (forAdd && (h.sock != "" || h.Status.Runtime != "container")) {
			continue
		}
		if checkProto(h) != nil || (h.derived && h.Token == "") {
			continue
		}
		seen[h.Status.ID] = true
		out = append(out, h)
	}
	if len(out) == 0 {
		return nil, errors.New("no runner can take GitHub jobs: they need podman, and this machine's own runner never does (see offload hosts)")
	}
	return out, nil
}

// errNoGitHub is a runner from before GitHub support: it has no pools.
var errNoGitHub = errors.New("too old for GitHub jobs")

func githubPools(ctx context.Context, h *Host) ([]api.GitHubPool, error) {
	var pools []api.GitHubPool
	err := h.do(ctx, "GET", "/v1/github", nil, &pools)
	var ae *APIError
	if errors.As(err, &ae) && ae.Code == 404 {
		return nil, fmt.Errorf("%w: run `offload host upgrade %s`", errNoGitHub, h.Name)
	}
	return pools, err
}

func githubFind(pools []api.GitHubPool, key string) *api.GitHubPool {
	for i := range pools {
		if api.GitHubKey(pools[i].Owner) == key {
			return &pools[i]
		}
	}
	return nil
}

func githubRoom(p api.GitHubPool) string {
	busy := p.Running + p.Starting
	free := max(p.Capacity-busy, 0)
	switch {
	case busy == 0 && free == 0:
		return "full right now: GitHub waits for room"
	case busy == 0:
		return fmt.Sprintf("room for %d job%s now", free, plural(free))
	case free == 0:
		return fmt.Sprintf("%d running, no room for more", busy)
	}
	return fmt.Sprintf("%d running, room for %d more", busy, free)
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

// GitHubAdd starts taking an organization's or repository's GitHub jobs on runners, and waits
// until each is listening (or says why it can't).
func GitHubAdd(o GitHubOptions) error {
	owner, _, err := api.GitHubOwner(o.Owner)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ctx, cancelWait := context.WithTimeout(ctx, 20*time.Minute)
	defer cancelWait()
	token, from, err := githubToken(ctx, owner, o.Token)
	if err != nil {
		return err
	}
	all, err := discover(ctx)
	if err != nil {
		return err
	}
	hosts, err := githubHosts(all, o.On, true)
	if err != nil {
		return err
	}
	how := "your gh login"
	if from == "token" {
		how = "the token you gave"
	}
	fmt.Printf("Taking GitHub jobs for %s on %s, with %s.\n\n", owner, joinNames(hosts), how)
	width := 0
	for _, h := range hosts {
		width = max(width, len(h.Name))
	}
	line := func(h *Host, state, detail string) {
		fmt.Printf("  %-*s  %-10s %s\n", width, h.Name, state, detail)
	}
	key := api.GitHubKey(owner)
	var mu sync.Mutex
	var wg sync.WaitGroup
	ok := 0
	for _, h := range hosts {
		wg.Add(1)
		go func() {
			defer wg.Done()
			req := api.GitHubAdd{URL: owner, Token: token, TokenFrom: from, Labels: o.Labels, Max: o.Max, Resources: o.Resources, SharedCache: o.Shared}
			var p api.GitHubPool
			if err := h.do(ctx, "POST", "/v1/github", req, &p); err != nil {
				var ae *APIError
				msg := err.Error()
				if errors.As(err, &ae) {
					msg = ae.Msg
					if ae.Code == 404 {
						msg = "too old for GitHub jobs: run `offload host upgrade " + h.Name + "`"
					}
				}
				mu.Lock()
				line(h, "failed", msg)
				mu.Unlock()
				return
			}
			last := ""
			for {
				pools, err := githubPools(ctx, h)
				if ctx.Err() != nil {
					return
				}
				if err == nil {
					if p := githubFind(pools, key); p != nil {
						now := p.State + "|" + p.Detail
						if now != last {
							last = now
							mu.Lock()
							switch p.State {
							case api.GitHubListening:
								ok++
								line(h, "ready", githubRoom(*p))
							default:
								line(h, p.State, p.Detail)
							}
							mu.Unlock()
						}
						// Ready, or not getting there without help: report it and stop waiting.
						if p.State == api.GitHubListening || p.State == api.GitHubFailed || strings.Contains(p.Detail, "paused") {
							return
						}
					}
				}
				select {
				case <-ctx.Done():
					return
				case <-time.After(2 * time.Second):
				}
			}
		}()
	}
	wg.Wait()
	if errors.Is(ctx.Err(), context.DeadlineExceeded) && ok == 0 {
		return errors.New("no runner confirmed it is taking GitHub jobs within 20 minutes: `offload github ls` shows where each one stands")
	}
	if ctx.Err() != nil {
		fmt.Println("\nStill setting up in the background: `offload github ls` shows how far each runner got.")
		return nil
	}
	if ok == 0 {
		return errors.New("no runner is taking GitHub jobs yet (they keep retrying; offload github ls shows why)")
	}
	fmt.Printf("\nWorkflows in %s run here with:\n\n    runs-on: %s\n\n", owner, runsOn(o.Labels))
	if !strings.Contains(owner, "/") && o.Shared == nil && !githubSharedNow(ctx, hosts, owner) {
		fmt.Printf("Each job starts with clean caches, because %s's repositories would otherwise share checkouts,\n"+
			"dependency caches and docker images. If they all trust each other, keep them warm with:\n\n"+
			"    offload github add %s --shared-cache\n\n", owner, owner)
	}
	return nil
}

func runsOn(labels []string) string {
	if len(labels) == 0 {
		return "offload"
	}
	if len(labels) == 1 {
		return labels[0]
	}
	return "[" + strings.Join(labels, ", ") + "]"
}

func joinNames(hosts []*Host) string {
	names := make([]string, len(hosts))
	for i, h := range hosts {
		names[i] = h.Name
	}
	return strings.Join(names, ", ")
}

// GitHubList prints the organizations and repositories runners take GitHub jobs for.
func GitHubList() error {
	ctx := context.Background()
	all, err := discover(ctx)
	if err != nil {
		return err
	}
	hosts, err := githubHosts(all, nil, false)
	if err != nil {
		return err
	}
	type row struct {
		h *Host
		p api.GitHubPool
	}
	byOwner := map[string][]row{}
	var owners []string
	for _, h := range hosts {
		pools, err := githubPools(ctx, h)
		if errors.Is(err, errNoGitHub) {
			continue
		}
		if err != nil {
			fmt.Fprintf(os.Stderr, "%s: %v\n", h.Name, err)
			continue
		}
		for _, p := range pools {
			k := api.GitHubKey(p.Owner)
			if byOwner[k] == nil {
				owners = append(owners, k)
			}
			byOwner[k] = append(byOwner[k], row{h, p})
		}
	}
	if len(owners) == 0 {
		fmt.Println("No runner takes GitHub jobs yet. Start with:\n\n    offload github add ORG")
		return nil
	}
	sort.Strings(owners)
	tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	for i, k := range owners {
		rows := byOwner[k]
		first := rows[0].p
		if i > 0 {
			fmt.Fprintln(tw)
		}
		token := "a token"
		if first.TokenFrom == "gh" {
			token = "your gh login"
		}
		caches := "clean caches every job"
		if first.Shared {
			caches = "shared warm caches"
		}
		fmt.Fprintf(tw, "%s\truns-on: %s · with %s · %s\n", first.Owner, runsOn(first.Labels), token, caches)
		fmt.Fprintln(tw, "  RUNNER\tSTATE\tNOW\tDONE\tLAST JOB\tEACH JOB")
		for _, r := range rows {
			p := r.p
			now := githubRoom(p)
			if p.State != api.GitHubListening && p.Detail != "" {
				now = p.Detail
			}
			last := "–"
			if p.LastRun != nil {
				last = time.Since(*p.LastRun).Round(time.Minute).String() + " ago"
				if time.Since(*p.LastRun) < time.Minute {
					last = "just now"
				}
			}
			fmt.Fprintf(tw, "  %s\t%s\t%s\t%d\t%s\t%.0f–%.0f cores, %s\n", r.h.Name, p.State, now, p.Runs, last,
				p.Resources.CPU, p.Resources.CPUMax, units.FormatSize(p.Resources.Mem))
		}
	}
	return tw.Flush()
}

// GitHubRemove stops taking an owner's GitHub jobs on runners and deletes their scale sets.
func GitHubRemove(raw string, on []string, forget bool) error {
	owner, _, err := api.GitHubOwner(raw)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	all, err := discover(ctx)
	if err != nil {
		return err
	}
	hosts, err := githubHosts(all, on, false)
	if err != nil {
		return err
	}
	key := api.GitHubKey(owner)
	var targets []*Host
	var unchecked []string
	for _, h := range hosts {
		pools, err := githubPools(ctx, h)
		switch {
		case errors.Is(err, errNoGitHub): // it can't have any
		case err != nil:
			unchecked = append(unchecked, fmt.Sprintf("%s (%v)", h.Name, err))
		case githubFind(pools, key) != nil:
			targets = append(targets, h)
		}
	}
	if len(targets) == 0 && len(unchecked) == 0 {
		return fmt.Errorf("no runner takes GitHub jobs for %s (see offload github ls)", owner)
	}
	defer func() {
		if len(unchecked) > 0 {
			fmt.Printf("\nCouldn't check %s: run this again once they answer, or they keep taking %s's jobs.\n", strings.Join(unchecked, ", "), owner)
		}
	}()
	fmt.Printf("Removing GitHub jobs for %s from %s. Running jobs are stopped.\n\n", owner, joinNames(targets))
	var wg sync.WaitGroup
	var mu sync.Mutex
	var failed error
	for _, h := range targets {
		wg.Add(1)
		go func() {
			defer wg.Done()
			report := func(format string, a ...any) {
				mu.Lock()
				fmt.Printf("  %s: "+format+"\n", append([]any{h.Name}, a...)...)
				mu.Unlock()
			}
			path := "/v1/github/" + key
			if forget {
				path += "?forget=1"
			}
			if err := h.do(ctx, "DELETE", path, nil, nil); err != nil {
				report("%v", err)
				mu.Lock()
				failed = err
				mu.Unlock()
				return
			}
			last := ""
			for deadline := time.Now().Add(2 * time.Minute); time.Now().Before(deadline); {
				pools, err := githubPools(ctx, h)
				if err == nil && githubFind(pools, key) == nil {
					report("removed")
					return
				}
				if p := githubFind(pools, key); err == nil && p.Detail != last && strings.HasPrefix(p.Detail, "couldn't") {
					last = p.Detail
					report("%s", p.Detail)
				}
				select {
				case <-ctx.Done():
					return
				case <-time.After(time.Second):
				}
			}
			report("still removing; `offload github ls` shows when it's done")
			mu.Lock()
			if failed == nil && last != "" {
				failed = errors.New("GitHub hasn't confirmed the removal yet")
			}
			mu.Unlock()
		}()
	}
	wg.Wait()
	if failed == nil && len(unchecked) > 0 {
		failed = errors.New("some runners couldn't be checked")
	}
	return failed
}

// githubSharedNow reports whether a runner already keeps owner with shared caches (the choice is
// fixed when an organization is added, so re-adding it for a new token keeps it).
func githubSharedNow(ctx context.Context, hosts []*Host, owner string) bool {
	for _, h := range hosts {
		if pools, err := githubPools(ctx, h); err == nil {
			if p := githubFind(pools, api.GitHubKey(owner)); p != nil && p.Shared {
				return true
			}
		}
	}
	return false
}
