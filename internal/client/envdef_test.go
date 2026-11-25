package client

import (
	"bufio"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestStripJSONC(t *testing.T) {
	in := `{
  // the image
  "image": "mcr.microsoft.com/devcontainers/base:debian", /* inline */
  "containerEnv": {"A": "x//not a comment",},
  "runArgs": ["--device=/dev/dri",],
}`
	var v map[string]any
	if err := json.Unmarshal(stripJSONC([]byte(in)), &v); err != nil {
		t.Fatalf("%v\n%s", err, stripJSONC([]byte(in)))
	}
	if v["containerEnv"].(map[string]any)["A"] != "x//not a comment" {
		t.Fatalf("string mangled: %v", v)
	}
}

func TestEnvAddKeepsOneSortedPackageLayer(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	if err := EnvAdd("demo", []string{"protobuf-compiler", "jq"}, ""); err != nil {
		t.Fatal(err)
	}
	if err := EnvAdd("demo", []string{"alsa-lib-devel", "jq"}, "echo hi"); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(filepath.Join(dir, "offload", "envs", "demo", "Containerfile"))
	s := string(b)
	if strings.Count(s, packagesMarker) != 1 || !strings.Contains(s, "--no-install-recommends alsa-lib-devel jq protobuf-compiler &&") {
		t.Fatalf("package layer wrong:\n%s", s)
	}
	if !strings.HasSuffix(s, "RUN echo hi\n") || strings.Index(s, "FROM offload/base") > strings.Index(s, packagesMarker) {
		t.Fatalf("layout wrong:\n%s", s)
	}
}

func TestDataFiles(t *testing.T) {
	root := t.TempDir()
	for _, f := range []string{"versions/1.26.9.1/a", "versions/1.26.60.4/b", "versions/versions.ini", "versions/1.26.44.3/c", "apks/x.apk", "mods/m.so", "dobby.log", "minecraftpe/packcache/p", "minecraftpe/options.txt", "xal/1"} {
		os.MkdirAll(filepath.Join(root, filepath.Dir(f)), 0o755)
		os.WriteFile(filepath.Join(root, f), []byte(f), 0o644)
	}
	_, files, err := dataFiles(DataSetConfig{Path: root, Exclude: []string{"apks/**", "*.log", "minecraftpe/packcache/**"}, Newest: []string{"versions/*"}})
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Join(files, ",")
	if got != "minecraftpe/options.txt,mods/m.so,versions/1.26.60.4/b,versions/versions.ini,xal/1" {
		t.Fatalf("files = %s", got)
	}
	if !versionLess("1.26.9.1", "1.26.60.4") || versionLess("1.26.60.4", "1.26.44.3") {
		t.Fatal("versionLess")
	}
}

func TestCredentialRequest(t *testing.T) {
	r, err := parseCredentialRequest([]byte("protocol=https\nhost=github.com\ncapability[]=authtype\n\n"))
	if err != nil || r.Host != "github.com" || string(r.canonical()) != "protocol=https\nhost=github.com\n\n" {
		t.Fatalf("plain request: %+v %v", r, err)
	}
	for _, bad := range []string{
		"protocol=https\nhost=github.com\nhost=evil.example\n\n",
		"protocol=https\nhost=github.com\nurl=https://evil.example/\n\n",
		"host=github.com\n\n",
	} {
		if _, err := parseCredentialRequest([]byte(bad)); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
}

func TestWriteInRefusesSymlinkEscape(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "tree"), 0o755)
	os.MkdirAll(filepath.Join(dir, "outside"), 0o755)
	os.Symlink(filepath.Join(dir, "outside"), filepath.Join(dir, "tree", "link"))
	root, err := os.OpenRoot(filepath.Join(dir, "tree"))
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	if err := writeIn(root, "link/pwned", strings.NewReader("x"), 0o644, ""); err == nil {
		t.Fatal("wrote through a symlink to outside the tree")
	}
	if _, err := os.Stat(filepath.Join(dir, "outside", "pwned")); err == nil {
		t.Fatal("file appeared outside the tree")
	}
	for _, bad := range []string{"../x", "/etc/passwd", "a/../../b", ""} {
		if checkRel(bad) == nil {
			t.Errorf("checkRel accepted %q", bad)
		}
	}
}

func TestArtifactsRefuseSymlinkParentsInsideTree(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "src"), 0o755)
	os.WriteFile(filepath.Join(dir, "src", "config"), []byte("keep"), 0o644)
	os.Symlink("src", filepath.Join(dir, "out")) // out -> src, inside the tree
	root, _ := os.OpenRoot(dir)
	defer root.Close()
	if realParents(root, "out/config") {
		t.Fatal("symlinked parent accepted")
	}
	if !realParents(root, "src/config") {
		t.Fatal("real parent rejected")
	}
}

func TestBlockedTarget(t *testing.T) {
	cfg := &Config{Port: 7420, Hosts: []HostConfig{{URL: "http://box.example:9000"}}}
	for target, blocked := range map[string]bool{
		"100.106.54.69:7420":          true, // a runner's API on the tailnet
		"[fd7a:115c:a1e0::1]:7420":    true,
		"box.example:9000":            true, // a configured runner
		"100.106.54.69:07420":         true, // the same port, spelled differently
		"[::ffff:100.106.54.69]:7420": true,
		"box.example:09000":           true,
		"100.106.54.69:22":            false, // other tailnet services are the point of the feature
		"127.0.0.1:18080":             false,
		"140.82.114.3:443":            false,
	} {
		if got := blockedTarget(target, cfg) != ""; got != blocked {
			t.Errorf("blockedTarget(%s) = %v, want %v", target, got, blocked)
		}
	}
}

func TestReadLineMax(t *testing.T) {
	r := bufio.NewReaderSize(strings.NewReader("short\n"+strings.Repeat("x", 100)+"\nend"), 16)
	if l, err := readLineMax(r, 64); err != nil || string(l) != "short\n" {
		t.Fatalf("got %q %v", l, err)
	}
	if _, err := readLineMax(r, 64); !errors.Is(err, errTooLong) {
		t.Fatalf("long line: %v", err)
	}
	r = bufio.NewReaderSize(strings.NewReader(strings.Repeat("y", 40)+"\n"), 16)
	if l, err := readLineMax(r, 64); err != nil || len(l) != 41 {
		t.Fatalf("line across buffer refills: %d %v", len(l), err)
	}
}

func TestLocalOrigin(t *testing.T) {
	for u, want := range map[string]bool{
		"../upstream.git":                      true,
		"/srv/git/repo.git":                    true,
		"file:///srv/git/repo.git":             true,
		"git@github.com:RestartFU/offload.git": false,
		"https://github.com/RestartFU/offload": false,
		"ssh://git@host/x.git":                 false,
		"./a:b":                                true,
	} {
		if got := localOrigin(u); got != want {
			t.Errorf("localOrigin(%q) = %v, want %v", u, got, want)
		}
	}
}

func TestDataRootSymlinkIsFollowed(t *testing.T) {
	dir := t.TempDir()
	real := filepath.Join(dir, "real")
	os.MkdirAll(filepath.Join(real, "sub"), 0o755)
	os.WriteFile(filepath.Join(real, "sub", "f"), []byte("x"), 0o644)
	os.Symlink(real, filepath.Join(dir, "link"))
	_, files, err := dataFiles(DataSetConfig{Path: filepath.Join(dir, "link")})
	if err != nil || len(files) != 1 || files[0] != "sub/f" {
		t.Fatalf("files = %v, %v", files, err)
	}
	if _, _, err := dataFiles(DataSetConfig{Path: filepath.Join(dir, "missing")}); err == nil {
		t.Fatal("missing root accepted")
	}
}

func TestGitFilesTrackedFileBecameDirectory(t *testing.T) {
	dir := t.TempDir()
	run := func(args ...string) {
		cmd := exec.Command("git", append([]string{"-C", dir, "-c", "user.name=t", "-c", "user.email=t@t"}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	run("init", "-q")
	os.WriteFile(filepath.Join(dir, "a"), []byte("file"), 0o644)
	run("add", "a")
	run("commit", "-qm", "a")
	os.Remove(filepath.Join(dir, "a"))
	os.MkdirAll(filepath.Join(dir, "a"), 0o755)
	os.WriteFile(filepath.Join(dir, "a", "b"), []byte("now a dir"), 0o644)
	files, err := gitFiles(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 || files[0] != "a/b" {
		t.Fatalf("files = %v, want [a/b]", files)
	}
}

func TestGlobMatchUnicode(t *testing.T) {
	if !globMatch("captures/été/**", "captures/été/a.png") {
		t.Fatal("accented directory not matched")
	}
	if !globMatch("caf?.txt", "café.txt") {
		t.Fatal("? must match one character, not one byte")
	}
}
