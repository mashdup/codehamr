package tools

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// WriteFile writes content to path, creating parent dirs. Errors return as
// part of the output string (bash convention), never as a Go error, so the
// model sees a write failure the way it sees a non-zero bash exit.
func WriteFile(path, content string) string {
	if path == "" {
		return "(empty path)"
	}
	// Refuse an existing non-regular target: open(2) with O_WRONLY on a FIFO
	// with no reader blocks forever, leaking the tool goroutine past Ctrl+C
	// (which cancels the turn but can't unblock the open). Stat never blocks;
	// directories fall through to os.WriteFile's immediate EISDIR.
	if info, err := os.Stat(path); err == nil && !info.Mode().IsRegular() && !info.IsDir() {
		return fmt.Sprintf("(write error: %s is not a regular file)", path)
	}
	if dir := filepath.Dir(path); dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Sprintf("(mkdir error: %v)", err)
		}
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		return fmt.Sprintf("(write error: %v)", err)
	}
	return fmt.Sprintf("wrote %d bytes to %s", len(content), path)
}

// writeTool is the registry entry for write_file: a side-effecting tool gated
// by approval that mutates the file at args["path"] (so the driver snapshots a
// diff around it).
type writeTool struct{}

func (writeTool) Name() string           { return WriteFileName }
func (writeTool) Safe() bool             { return false }
func (writeTool) Mutates() bool          { return true }
func (writeTool) Schema() map[string]any { return writeSchema() }

func (writeTool) Run(_ context.Context, args map[string]any) string {
	path, _ := args["path"].(string)
	// A missing/non-string content (valid JSON, so no _parse_error; schema
	// `required` is not enforced by open-source backends) must not decode to ""
	// and silently truncate an existing file to 0 bytes behind a success-shaped
	// result. An explicit `"content": ""` still writes.
	content, ok := args["content"].(string)
	if !ok {
		return `(missing content argument: the call carried no string "content", refusing to write - resend with the full content; an intentionally empty file needs an explicit "content": "")`
	}
	return WriteFile(path, content)
}

func (writeTool) InlineStatus(args map[string]any) string {
	path, _ := args["path"].(string)
	return "▶ write_file: " + path
}

func (writeTool) Failed(result string) bool {
	// write reports success as plain text ("wrote N bytes") and every error in
	// parens, so a leading "(" is the failure signal.
	return strings.HasPrefix(strings.TrimSpace(result), "(")
}

func (writeTool) TargetKey(args map[string]any) string {
	path, _ := args["path"].(string)
	return WriteFileName + "|" + path
}

// writeSchema is the OpenAI tool definition for write_file. The description
// steers the model away from bash heredocs (shell-quoting failure mode) for
// small-to-medium writes, and toward heredoc appends for large files (streamed
// tool-call args truncate server-side), mirroring the system prompt's rule so
// the two instruction channels can't contradict each other.
func writeSchema() map[string]any {
	return map[string]any{
		"type": "function",
		"function": map[string]any{
			"name":        WriteFileName,
			"description": "Write content bytes to a file at path. Creates parent directories. Overwrites existing files. Use this instead of bash heredocs for small-to-medium multi line content or content with single quotes, dollar signs, or backticks - no shell quoting issues. Content beyond a few hundred lines gets truncated by the server mid-stream: build large files with bash heredoc appends (cat > path <<'EOF' first, then cat >> path <<'EOF' per part) from the first call.",
			"parameters": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"path": map[string]any{
						"type":        "string",
						"description": "Absolute or relative file path. Relative paths resolve against the working directory.",
					},
					"content": map[string]any{
						"type":        "string",
						"description": "Exact bytes to write to the file.",
					},
				},
				"required": []string{"path", "content"},
			},
		},
	}
}
