package tools

import (
	"context"
	"fmt"
	"os"
	"strings"
)

// EditFile replaces old_string with new_string in path. old_string must match
// EXACTLY ONCE; otherwise the file is untouched and an error string is returned
// so the model sees the failure and reacts, same convention as bash/WriteFile.
//
// Empty old_string is rejected (no anchor, every position matches);
// old_string == new_string is rejected as a no-op turn-waster.
func EditFile(path, oldString, newString string) string {
	if path == "" {
		return "(empty path)"
	}
	if oldString == "" {
		return "(empty old_string)"
	}
	if oldString == newString {
		return "(no change: old_string equals new_string)"
	}
	// Same guard as ReadFile: open(2) on a FIFO blocks forever and Ctrl+C
	// can't unblock it, leaking the tool goroutine. Stat never blocks.
	if info, err := os.Stat(path); err == nil && !info.Mode().IsRegular() && !info.IsDir() {
		return fmt.Sprintf("(read error: %s is not a regular file)", path)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return fmt.Sprintf("(read error: %v)", err)
	}
	content := string(raw)
	n := strings.Count(content, oldString)
	if n == 0 {
		// A near-miss that differs only in whitespace (wrong indentation, tabs vs
		// spaces) is the most common edit_file failure for an LLM. Name it so the
		// model fixes the bytes instead of burning retries toward the failure nudge.
		// Detection only; never auto-apply a fuzzy match, or the exact-match-once
		// safety the caller relies on is gone.
		if differsOnlyInWhitespace(content, oldString) {
			return fmt.Sprintf("(not found: no exact match in %s - a block there differs only in whitespace (indentation/tabs/newlines); copy the exact bytes, including indentation)", path)
		}
		return fmt.Sprintf("(not found: old_string does not appear in %s)", path)
	}
	if n > 1 {
		return fmt.Sprintf("(ambiguous: old_string appears %d times - provide more context to make it unique)", n)
	}
	// strings.Count only counts non-overlapping occurrences, so a self-
	// overlapping old_string ("==" in "a === b") passes n == 1 yet matches at
	// two positions with different results. Catch the overlapping second match
	// so the exactly-once guarantee holds.
	if idx := strings.Index(content, oldString); strings.Contains(content[idx+1:], oldString) {
		return "(ambiguous: old_string overlaps itself - provide more context to make it unique)"
	}
	updated := strings.Replace(content, oldString, newString, 1)
	if err := os.WriteFile(path, []byte(updated), 0o644); err != nil {
		return fmt.Sprintf("(write error: %v)", err)
	}
	return fmt.Sprintf("edited %s: -%d +%d bytes", path, len(oldString), len(newString))
}

// differsOnlyInWhitespace reports whether oldString matches content at exactly
// one spot once every whitespace run is collapsed: i.e. the sole mismatch is
// indentation/tabs/newlines. Bounded with spaces so a match can't straddle a
// token boundary and mislabel an unrelated near-miss.
func differsOnlyInWhitespace(content, oldString string) bool {
	norm := func(s string) string { return " " + strings.Join(strings.Fields(s), " ") + " " }
	return strings.Count(norm(content), norm(oldString)) == 1
}

// editTool is the registry entry for edit_file: a side-effecting tool gated by
// approval that mutates the file at args["path"] (so the driver snapshots a
// diff around it).
type editTool struct{}

func (editTool) Name() string           { return EditFileName }
func (editTool) Safe() bool             { return false }
func (editTool) Mutates() bool          { return true }
func (editTool) Schema() map[string]any { return editSchema() }

func (editTool) Run(_ context.Context, args map[string]any) string {
	path, _ := args["path"].(string)
	oldString, _ := args["old_string"].(string)
	// Same guard as write_file's content: a dropped new_string must not decode
	// to "" and silently delete the matched text. An explicit `"new_string":
	// ""` still deletes.
	newString, ok := args["new_string"].(string)
	if !ok {
		return `(missing new_string argument: the call carried no string "new_string", refusing to edit - resend it; deleting the match needs an explicit "new_string": "")`
	}
	return EditFile(path, oldString, newString)
}

func (editTool) InlineStatus(args map[string]any) string {
	path, _ := args["path"].(string)
	return "▶ edit_file: " + path
}

func (editTool) Failed(result string) bool {
	// edit reports success as plain text ("edited …") and every error in
	// parens, so a leading "(" is the failure signal.
	return strings.HasPrefix(strings.TrimSpace(result), "(")
}

func (editTool) TargetKey(args map[string]any) string {
	path, _ := args["path"].(string)
	return EditFileName + "|" + path
}

// editSchema is the OpenAI tool definition for edit_file. The description
// steers the model toward edit_file over write_file for small changes so it
// stops rewriting whole documents to fix a typo.
func editSchema() map[string]any {
	return map[string]any{
		"type": "function",
		"function": map[string]any{
			"name":        EditFileName,
			"description": "Surgically replace a single occurrence of old_string with new_string in an existing file. old_string must appear EXACTLY ONCE in the file - include enough surrounding context to make it unique. Prefer this over write_file for any change to an existing file shorter than a full rewrite: small typo fixes, single-line edits, swapping a function body. Errors (not found, ambiguous, file missing) come back as part of the result string, same as bash. A large new_string hits the same streamed-args truncation ceiling as write_file - chunk big insertions with bash heredoc appends instead.",
			"parameters": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"path": map[string]any{
						"type":        "string",
						"description": "Absolute or relative file path. Relative paths resolve against the working directory.",
					},
					"old_string": map[string]any{
						"type":        "string",
						"description": "Exact substring to find. Must be non-empty and appear exactly once.",
					},
					"new_string": map[string]any{
						"type":        "string",
						"description": "Replacement string. Empty deletes the match.",
					},
				},
				"required": []string{"path", "old_string", "new_string"},
			},
		},
	}
}
