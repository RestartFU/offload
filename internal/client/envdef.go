package client

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/RestartFU/offload/internal/api"
)

// Environments. A job runs in, in order of preference:
//
//  1. --environment NAME: ~/.config/offload/envs/NAME/Containerfile (tools not tied to a repo);
//  2. the repository's .offload/Containerfile (project tools, committed with the code);
//  3. the repository's .devcontainer/devcontainer.json (image or build.dockerfile, containerEnv);
//  4. nothing: the runner's built-in offload/base.
//
// Containerfiles normally start with `FROM offload/base`, so they only add what the base lacks.

const repoContainerfile = ".offload/Containerfile"

// packagesMarker tags the single, sorted package layer that `offload env add` maintains.
const packagesMarker = "# offload:packages"

func namedEnvDir(name string) string { return filepath.Join(configDir(), "envs", name) }

// LoadEnvDef finds the environment for a job run from root (the workspace root, may be "").
func LoadEnvDef(root, named string) (*api.EnvDef, error) {
	if named != "" {
		dir := namedEnvDir(named)
		b, err := os.ReadFile(filepath.Join(dir, "Containerfile"))
		if err != nil {
			return nil, fmt.Errorf("environment %q: %w", named, err)
		}
		def := &api.EnvDef{Name: named, Containerfile: string(b)}
		def.Devices = devicesFrom(string(b))
		return def, nil
	}
	if root == "" {
		return nil, nil
	}
	if b, err := os.ReadFile(filepath.Join(root, repoContainerfile)); err == nil {
		return &api.EnvDef{Name: repoContainerfile, Containerfile: string(b), ContextDir: ".", Devices: devicesFrom(string(b))}, nil
	}
	return loadDevcontainer(root)
}

// devicesFrom reads `# offload:device /dev/dri` lines (devices the environment needs).
func devicesFrom(cf string) []string {
	var out []string
	for _, line := range strings.Split(cf, "\n") {
		if d, ok := strings.CutPrefix(strings.TrimSpace(line), "# offload:device "); ok {
			out = append(out, strings.TrimSpace(d))
		}
	}
	return out
}

// loadDevcontainer supports the parts of devcontainer.json that define the environment:
// image, build.dockerfile/context, containerEnv and --device runArgs. Features are not supported.
func loadDevcontainer(root string) (*api.EnvDef, error) {
	for _, rel := range []string{".devcontainer/devcontainer.json", ".devcontainer.json"} {
		b, err := os.ReadFile(filepath.Join(root, rel))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		var dc struct {
			Image string `json:"image"`
			Build struct {
				Dockerfile string `json:"dockerfile"`
				Context    string `json:"context"`
			} `json:"build"`
			DockerFile   string            `json:"dockerFile"`
			ContainerEnv map[string]string `json:"containerEnv"`
			RunArgs      []string          `json:"runArgs"`
		}
		if err := json.Unmarshal(stripJSONC(b), &dc); err != nil {
			return nil, fmt.Errorf("%s: %w", rel, err)
		}
		def := &api.EnvDef{Name: rel, Env: dc.ContainerEnv}
		for i, a := range dc.RunArgs {
			if a == "--device" && i+1 < len(dc.RunArgs) {
				def.Devices = append(def.Devices, dc.RunArgs[i+1])
			} else if d, ok := strings.CutPrefix(a, "--device="); ok {
				def.Devices = append(def.Devices, d)
			}
		}
		dockerfile := dc.Build.Dockerfile
		if dockerfile == "" {
			dockerfile = dc.DockerFile
		}
		base := filepath.Dir(rel)
		switch {
		case dockerfile != "":
			cf, err := os.ReadFile(filepath.Join(root, base, dockerfile))
			if err != nil {
				return nil, fmt.Errorf("%s: %w", rel, err)
			}
			ctx := dc.Build.Context
			if ctx == "" {
				ctx = "."
			}
			def.Containerfile = string(cf)
			def.ContextDir = filepath.ToSlash(filepath.Clean(filepath.Join(base, ctx)))
		case dc.Image != "":
			def.Containerfile = "FROM " + dc.Image + "\n"
		default:
			continue
		}
		return def, nil
	}
	return nil, nil
}

// stripJSONC removes // and /* */ comments and trailing commas (devcontainer.json is JSONC).
func stripJSONC(b []byte) []byte {
	var out []byte
	inStr, esc := false, false
	for i := 0; i < len(b); i++ {
		c := b[i]
		if inStr {
			out = append(out, c)
			switch {
			case esc:
				esc = false
			case c == '\\':
				esc = true
			case c == '"':
				inStr = false
			}
			continue
		}
		switch {
		case c == '"':
			inStr = true
			out = append(out, c)
		case c == '/' && i+1 < len(b) && b[i+1] == '/':
			for i < len(b) && b[i] != '\n' {
				i++
			}
			out = append(out, '\n')
		case c == '/' && i+1 < len(b) && b[i+1] == '*':
			i += 2
			for i+1 < len(b) && !(b[i] == '*' && b[i+1] == '/') {
				i++
			}
			i++
		case c == ']' || c == '}':
			// drop a trailing comma before the closing bracket
			j := len(out) - 1
			for j >= 0 && (out[j] == ' ' || out[j] == '\n' || out[j] == '\t' || out[j] == '\r') {
				j--
			}
			if j >= 0 && out[j] == ',' {
				out = append(out[:j], out[j+1:]...)
			}
			out = append(out, c)
		default:
			out = append(out, c)
		}
	}
	return out
}

// EnvAdd adds system packages (one sorted layer) or a raw RUN step to an environment file:
// the named environment, or the repository's .offload/Containerfile (created on first use).
func EnvAdd(named string, pkgs []string, run string) error {
	path, err := envFilePath(named)
	if err != nil {
		return err
	}
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		b = []byte("# Job environment for offload (https://github.com/RestartFU/offload).\n# Built on the runner's base image; add tools with `offload env add PKG...`.\nFROM offload/base\n")
		err = os.MkdirAll(filepath.Dir(path), 0o755)
	}
	if err != nil {
		return err
	}
	lines := strings.Split(strings.TrimRight(string(b), "\n"), "\n")
	if len(pkgs) > 0 {
		idx := -1
		for i, l := range lines {
			if strings.Contains(l, packagesMarker) {
				idx = i
			}
		}
		have := map[string]bool{}
		if idx >= 0 {
			for _, f := range strings.Fields(lines[idx]) {
				if f == "#" || strings.HasPrefix(f, "offload:") || strings.HasPrefix(f, "-") || strings.Contains(f, "/") ||
					f == "RUN" || f == "apt-get" || f == "update" || f == "install" || f == "&&" || f == "rm" {
					continue
				}
				have[f] = true
			}
		}
		for _, p := range pkgs {
			have[p] = true
		}
		var all []string
		for p := range have {
			all = append(all, p)
		}
		sort.Strings(all)
		line := "RUN apt-get update && apt-get install -y --no-install-recommends " + strings.Join(all, " ") + " && rm -rf /var/lib/apt/lists/* " + packagesMarker
		if idx >= 0 {
			lines[idx] = line
		} else {
			// Right after FROM, so the package layer comes before any custom steps.
			at := 0
			for i, l := range lines {
				if strings.HasPrefix(strings.ToUpper(strings.TrimSpace(l)), "FROM ") {
					at = i + 1
				}
			}
			lines = append(lines[:at], append([]string{line}, lines[at:]...)...)
		}
	}
	if run != "" {
		lines = append(lines, "RUN "+run)
	}
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		return err
	}
	fmt.Printf("updated %s\n", path)
	return nil
}

func envFilePath(named string) (string, error) {
	if named != "" {
		return filepath.Join(namedEnvDir(named), "Containerfile"), nil
	}
	cwd, _ := os.Getwd()
	top, err := git(cwd, "rev-parse", "--show-toplevel")
	if err != nil {
		return "", errors.New("not in a git repository; use --environment NAME for a named environment")
	}
	return filepath.Join(strings.TrimSpace(top), repoContainerfile), nil
}

// EnvShow prints the environment a job started here would use.
func EnvShow(named string) error {
	cwd, _ := os.Getwd()
	root := ""
	if top, err := git(cwd, "rev-parse", "--show-toplevel"); err == nil {
		root = strings.TrimSpace(top)
	}
	def, err := LoadEnvDef(root, named)
	if err != nil {
		return err
	}
	if def == nil {
		fmt.Println("environment: offload/base (the runner's built-in default)")
		return nil
	}
	fmt.Printf("environment: %s\n\n%s", def.Name, def.Containerfile)
	return nil
}

// EnvBuild builds the current environment on the best runner (or every runner) by running a
// no-op job in it; the build log streams back like any job output.
func EnvBuild(named, on string, all bool) (int, error) {
	targets := []string{on}
	if all {
		cfg, err := LoadConfig()
		if err != nil {
			return 1, err
		}
		targets = nil
		for _, h := range Discover(contextBG(), cfg) {
			if h.Status.Runtime == "container" {
				targets = append(targets, h.Name)
			}
		}
	}
	worst := 0
	for _, t := range targets {
		code, err := Run(RunOptions{On: t, Environment: named, Argv: []string{"true"}, NoGit: true, NoWriteBack: true, NoCreds: true})
		if err != nil {
			return 1, err
		}
		worst = max(worst, code)
	}
	return worst, nil
}
func contextBG() context.Context { return context.Background() }
