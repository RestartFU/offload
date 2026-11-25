package hook

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func decide(cmd string) bool {
	in, _ := json.Marshal(map[string]any{"tool_name": "Bash", "tool_input": map[string]string{"command": cmd}})
	var out bytes.Buffer
	Claude(bytes.NewReader(in), &out)
	return strings.Contains(out.String(), `"deny"`)
}

func TestGuard(t *testing.T) {
	deny := []string{"cargo build --release", "cd x && cargo test -p foo", "RUSTFLAGS=-g cargo clippy", "go test ./...", "make -j8", "time cargo b", "bun run build", "(cd sub; make)"}
	allow := []string{"offload run -- cargo build", "git commit -m 'make it build'", "ls target", "cargo fmt", "echo cargo build", "gofmt -l .", "cmake -S . -B build", "OFFLOAD_GUARD=off cargo build"}
	for _, c := range deny {
		if !decide(c) {
			t.Errorf("should deny %q", c)
		}
	}
	for _, c := range allow {
		if decide(c) {
			t.Errorf("should allow %q", c)
		}
	}
}
