package client

import (
	"encoding/json"
	"os"
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
