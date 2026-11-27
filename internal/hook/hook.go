// Package hook implements the Claude Code PreToolUse guard that sends heavy commands to runners.
package hook

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"
)

// heavy matches commands that should run on a runner rather than the local machine.
// It only looks at command position (start, or after ; & | ( or $( ), allowing VAR=value
// prefixes and time/nice, so words inside quoted strings don't trigger it.
var heavy = regexp.MustCompile(`(?:^|[;&|(]|\$\()\s*(?:\w+=\S*\s+)*(?:time\s+|nice\s+(?:-n\s*-?\d+\s+)?)?(` + strings.Join([]string{
	`cargo\s+(build|b|test|t|check|c|clippy|bench|run|r|nextest|doc|install)\b`,
	`go\s+(build|test|vet|install|generate)\b`,
	`make\b`, `ninja\b`, `cmake\s+--build\b`, `meson\s+compile\b`,
	`(npm|pnpm|yarn|bun)\s+(run\s+)?(build|test)\b`,
	`gradlew?\s`, `mvn\s`, `bazel\s+(build|test|run)\b`, `zig\s+build\b`,
	`rustc\s`, `tsc\b`, `pytest\b`,
}, "|") + `)`)

type input struct {
	ToolName  string `json:"tool_name"`
	ToolInput struct {
		Command string `json:"command"`
	} `json:"tool_input"`
}

// Claude reads a PreToolUse event on stdin and denies heavy local Bash commands with a hint to
// use `offload run`. Set OFFLOAD_GUARD=off to disable it for a session.
func Claude(in io.Reader, out io.Writer) int {
	if strings.EqualFold(os.Getenv("OFFLOAD_GUARD"), "off") {
		return 0
	}
	var ev input
	if err := json.NewDecoder(in).Decode(&ev); err != nil || ev.ToolName != "Bash" {
		return 0
	}
	cmd := ev.ToolInput.Command
	if cmd == "" || strings.Contains(cmd, "offload ") || !heavy.MatchString(cmd) {
		return 0
	}
	reason := fmt.Sprintf("Heavy commands run on offload runners, not this machine. Run it as:\n"+
		"  offload run -- %s\n"+
		"Memory limits are learned from earlier runs, so leave out --mem unless a job ran out of memory. "+
		"--get <path> to bring build outputs back. Run it in the background and use `offload attach <id>` if it takes a while. "+
		"If it truly must run locally, prefix the command with OFFLOAD_GUARD=off.", cmd)
	if strings.HasPrefix(strings.TrimSpace(cmd), "OFFLOAD_GUARD=off") {
		return 0
	}
	resp := map[string]any{"hookSpecificOutput": map[string]any{
		"hookEventName":            "PreToolUse",
		"permissionDecision":       "deny",
		"permissionDecisionReason": reason,
	}}
	_ = json.NewEncoder(out).Encode(resp)
	return 0
}
