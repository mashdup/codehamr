package tools

import (
	"context"
	"fmt"
	"os"
	"strings"

	chmctx "github.com/codehamr/codehamr/internal/ctx"
)

// ReadFile returns path's contents, truncated to the shared tool-output budget
// (Truncate). The model gets exact bytes, not a shell-mangled approximation.
// Per the bash/write/edit convention, filesystem errors come back in the output
// string, never as a Go error: the model reacts to them like a non-zero exit.
func ReadFile(path string) string {
	if path == "" {
		return "(empty path)"
	}
	// Refuse non-regular files up front: open(2) on a FIFO blocks forever
	// waiting for a writer (leaking the tool goroutine past Ctrl+C, which
	// cancels the turn but can't unblock the read), and an endless device file
	// (/dev/zero) grows ReadFile's buffer without bound. Stat never blocks.
	if info, err := os.Stat(path); err == nil && !info.Mode().IsRegular() && !info.IsDir() {
		return fmt.Sprintf("(read error: %s is not a regular file)", path)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return fmt.Sprintf("(read error: %v)", err)
	}
	return chmctx.Truncate(string(raw))
}

// readTool is the registry entry for read_file: a side-effect-free tool (Safe,
// never needs approval) that does not mutate a tracked file.
type readTool struct{}

func (readTool) Name() string           { return ReadFileName }
func (readTool) Safe() bool             { return true }
func (readTool) Mutates() bool          { return false }
func (readTool) Schema() map[string]any { return readSchema() }

func (readTool) Run(_ context.Context, args map[string]any) string {
	path, _ := args["path"].(string)
	return ReadFile(path)
}

func (readTool) InlineStatus(args map[string]any) string {
	path, _ := args["path"].(string)
	return "▶ read_file: " + path
}

func (readTool) Failed(result string) bool {
	// read_file returns the file's RAW content on success, which can
	// legitimately start with "(" (Lisp, S-expressions, a leading paren expr).
	// Match only its two real failure outputs so a successful read isn't
	// counted as a failure and made to feed the repeated-failure nudge.
	t := strings.TrimSpace(result)
	return strings.HasPrefix(t, "(read error:") || t == "(empty path)"
}

func (readTool) TargetKey(args map[string]any) string {
	path, _ := args["path"].(string)
	return ReadFileName + "|" + path
}

// readSchema is the OpenAI tool definition for read_file. The description
// nudges the model toward read_file over `cat` so it stops piping source
// through the shell just to look at it.
func readSchema() map[string]any {
	return map[string]any{
		"type": "function",
		"function": map[string]any{
			"name":        ReadFileName,
			"description": "Read a file and return its contents. Prefer this over `cat`/`sed` in bash for inspecting a file - no shell quoting, exact bytes. Output over 6k tokens is truncated to first+last 2k; for a slice of a large file use bash with sed/grep/head/tail.",
			"parameters": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"path": map[string]any{
						"type":        "string",
						"description": "Absolute or relative file path.",
					},
				},
				"required": []string{"path"},
			},
		},
	}
}
