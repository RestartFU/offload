package client

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"mime/multipart"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/BurntSushi/toml"

	"github.com/RestartFU/offload/internal/api"
	"github.com/RestartFU/offload/internal/units"
)

// DataSetConfig is a [data.NAME] table in client.toml: a local directory kept in sync on the
// runners that need it. Patterns use / separators; ** spans directories, and a pattern without a
// slash matches a file name at any depth.
//
//	[data.minecraft]
//	path = "~/.var/app/io.mrarm.mcpelauncher/data/mcpelauncher"
//	exclude = ["apks/**", "*.log"]
//	newest = ["versions/*"]   # of the entries matching, keep only the highest version
type DataSetConfig struct {
	Path    string   `toml:"path"`
	Include []string `toml:"include"` // empty = everything
	Exclude []string `toml:"exclude"`
	Newest  []string `toml:"newest"`
}

// SessionConfig is ~/.config/offload/sessions/NAME.toml: an MCP (or any stdio) server that runs
// on a runner, fully described on this machine so no runner needs setting up by hand.
type SessionConfig struct {
	Description   string            `toml:"description"`
	Environment   string            `toml:"environment"` // named environment (~/.config/offload/envs)
	Command       []string          `toml:"command"`
	Prepare       []string          `toml:"prepare"` // run by `offload mcp NAME --prepare` (first-time setup)
	Env           map[string]string `toml:"env"`
	Data          map[string]string `toml:"data"`    // data set name -> path in the container
	Volumes       map[string]string `toml:"volumes"` // runner-side persistent volume -> path
	Devices       []string          `toml:"devices"`
	NestedSandbox bool              `toml:"nested_sandbox"`
	Resources     struct {
		CPU    float64 `toml:"cpu"`
		CPUMax float64 `toml:"cpu_max"`
		Mem    string  `toml:"mem"`
	} `toml:"resources"`
}

// LoadSessionConfig reads a client-side session definition, or returns nil if there is none.
func LoadSessionConfig(name string) (*SessionConfig, error) {
	var sc SessionConfig
	path := filepath.Join(configDir(), "sessions", name+".toml")
	if _, err := toml.DecodeFile(path, &sc); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &sc, nil
}

func globRE(pattern string) *regexp.Regexp {
	var b strings.Builder
	b.WriteString("^")
	for i := 0; i < len(pattern); i++ {
		switch c := pattern[i]; c {
		case '*':
			if i+1 < len(pattern) && pattern[i+1] == '*' {
				b.WriteString(".*")
				i++
			} else {
				b.WriteString("[^/]*")
			}
		case '?':
			b.WriteString("[^/]")
		default:
			b.WriteString(regexp.QuoteMeta(string(c)))
		}
	}
	b.WriteString("$")
	return regexp.MustCompile(b.String())
}

// globMatch matches a slash path; patterns without a slash match the base name.
func globMatch(pattern, p string) bool {
	if !strings.Contains(pattern, "/") {
		return globRE(pattern).MatchString(baseName(p))
	}
	return globRE(pattern).MatchString(p)
}

func baseName(p string) string {
	if i := strings.LastIndex(p, "/"); i >= 0 {
		return p[i+1:]
	}
	return p
}

// versionLess orders "1.26.9.1" < "1.26.60.4" numerically, field by field.
func versionLess(a, b string) bool {
	fa, fb := strings.FieldsFunc(a, func(r rune) bool { return r == '.' || r == '-' || r == '_' }), strings.FieldsFunc(b, func(r rune) bool { return r == '.' || r == '-' || r == '_' })
	for i := 0; i < len(fa) && i < len(fb); i++ {
		na, ea := strconv.Atoi(fa[i])
		nb, eb := strconv.Atoi(fb[i])
		if ea == nil && eb == nil {
			if na != nb {
				return na < nb
			}
			continue
		}
		if fa[i] != fb[i] {
			return fa[i] < fb[i]
		}
	}
	return len(fa) < len(fb)
}

// dataFiles lists a data set's files after include/exclude/newest rules.
func dataFiles(cfg DataSetConfig) (string, []string, error) {
	root := expand(cfg.Path)
	// newest: for each pattern, every matching entry except the highest version is excluded.
	var drop []string
	for _, pat := range cfg.Newest {
		all, _ := filepath.Glob(filepath.Join(root, filepath.FromSlash(pat)))
		var matches []string
		for _, m := range all {
			if fi, err := os.Stat(m); err == nil && fi.IsDir() { // versions/*, not versions/versions.ini
				matches = append(matches, m)
			}
		}
		sort.Slice(matches, func(i, j int) bool { return versionLess(filepath.Base(matches[i]), filepath.Base(matches[j])) })
		for i := 0; i < len(matches)-1; i++ {
			rel, _ := filepath.Rel(root, matches[i])
			drop = append(drop, filepath.ToSlash(rel))
		}
	}
	excluded := func(rel string) bool {
		for _, d := range drop {
			if rel == d || strings.HasPrefix(rel, d+"/") {
				return true
			}
		}
		for _, x := range cfg.Exclude {
			if globMatch(x, rel) || (strings.HasSuffix(x, "/**") && rel == strings.TrimSuffix(x, "/**")) {
				return true
			}
		}
		return false
	}
	var files []string
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		rel, _ := filepath.Rel(root, p)
		rel = filepath.ToSlash(rel)
		if rel == "." {
			return nil
		}
		if excluded(rel) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.IsDir() || d.Type()&(fs.ModeSocket|fs.ModeNamedPipe|fs.ModeDevice) != 0 {
			return nil
		}
		if len(cfg.Include) > 0 {
			ok := false
			for _, in := range cfg.Include {
				if globMatch(in, rel) {
					ok = true
					break
				}
			}
			if !ok {
				return nil
			}
		}
		files = append(files, rel)
		return nil
	})
	return root, files, err
}

// SyncData brings a data set up to date on a runner, sending only changed files.
func SyncData(ctx context.Context, h *Host, name string, cfg DataSetConfig, quiet bool) error {
	start := time.Now()
	root, files, err := dataFiles(cfg)
	if err != nil {
		return fmt.Errorf("data set %s: %w", name, err)
	}
	manifest, err := hashFiles(root, files)
	if err != nil {
		return fmt.Errorf("data set %s: %w", name, err)
	}
	var plan struct {
		Needed []string `json:"needed"`
	}
	if err := h.do(ctx, "POST", "/v1/data/"+name, api.DataPlan{Manifest: manifest}, &plan); err != nil {
		return fmt.Errorf("data set %s: %w", name, err)
	}
	if len(plan.Needed) == 0 {
		return nil
	}
	var size int64
	byPath := map[string]bool{}
	for _, p := range plan.Needed {
		byPath[p] = true
	}
	for _, e := range manifest {
		if byPath[e.Path] {
			if fi, err := os.Stat(filepath.Join(root, filepath.FromSlash(e.Path))); err == nil {
				size += fi.Size()
			}
		}
	}
	logf(quiet, "syncing data set %s to %s: %d changed files, %s", name, h.Name, len(plan.Needed), units.FormatSize(size))
	pr, pw := io.Pipe()
	mw := multipart.NewWriter(pw)
	go func() {
		part, err := mw.CreateFormField("manifest")
		if err == nil {
			err = encodeJSON(part, api.DataPlan{Manifest: manifest})
		}
		if err == nil {
			part, err = mw.CreateFormFile("files", "files.tar.gz")
		}
		if err == nil {
			err = writeTar(part, root, manifest, plan.Needed)
		}
		if err == nil {
			err = mw.Close()
		}
		pw.CloseWithError(err)
	}()
	req, err := newRequest(ctx, h, "PUT", "/v1/data/"+name, pr)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	resp, err := h.http.Do(req)
	if err != nil {
		return fmt.Errorf("data set %s: %w", name, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("data set %s: %s: %s", name, resp.Status, strings.TrimSpace(string(b)))
	}
	logf(quiet, "data set %s synced in %s", name, time.Since(start).Round(100*time.Millisecond))
	return nil
}
