package tools

import (
	"context"
	"fmt"
	"os"
	"strings"
)

// MultiEditName is the wire name for the multi-hunk file editor.
const MultiEditName = "multi_edit"

// maxMultiEdits caps hunks per call: past this the model should rewrite the
// file, and an unbounded slice from a malformed call can't spin.
const maxMultiEdits = 50

// multiEdit is one old→new replacement inside a multi_edit call.
type multiEdit struct {
	oldString string
	newString string
}

// MultiEdit applies a sequence of exact-once replacements to path atomically:
// every hunk is validated and applied in order against the in-memory content,
// and the file is written ONCE at the end only if all hunks succeed. Any
// failure (bad anchor, ambiguous match, no-op) leaves the file untouched and
// returns an error naming the offending hunk, so a partial edit can never land
// on disk. Same paren-wrapped-error convention as edit_file.
//
// Hunks apply against the running buffer, so a later hunk can legitimately
// match text an earlier hunk produced; each anchor must still be unique at the
// moment it applies.
func MultiEdit(path string, edits []multiEdit) string {
	if path == "" {
		return "(empty path)"
	}
	if len(edits) == 0 {
		return "(no edits: multi_edit needs at least one {old_string,new_string} hunk)"
	}
	if len(edits) > maxMultiEdits {
		return fmt.Sprintf("(too many edits: %d hunks exceeds the %d cap - rewrite the file with write_file instead)", len(edits), maxMultiEdits)
	}
	if info, err := os.Stat(path); err == nil && !info.Mode().IsRegular() && !info.IsDir() {
		return fmt.Sprintf("(read error: %s is not a regular file)", path)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return fmt.Sprintf("(read error: %v)", err)
	}
	content := string(raw)
	totalOld, totalNew := 0, 0
	for i, e := range edits {
		// 1-based hunk index in every message: it matches how the model
		// numbered them in the call and points straight at the bad one.
		switch {
		case e.oldString == "":
			return fmt.Sprintf("(hunk %d: empty old_string)", i+1)
		case e.oldString == e.newString:
			return fmt.Sprintf("(hunk %d: no change - old_string equals new_string)", i+1)
		}
		n := strings.Count(content, e.oldString)
		if n == 0 {
			if differsOnlyInWhitespace(content, e.oldString) {
				return fmt.Sprintf("(hunk %d not found: a block differs only in whitespace (indentation/tabs/newlines); copy the exact bytes, including indentation)", i+1)
			}
			return fmt.Sprintf("(hunk %d not found: old_string does not appear in %s at this point - hunks apply in order, so check whether an earlier hunk changed this text)", i+1, path)
		}
		if n > 1 {
			return fmt.Sprintf("(hunk %d ambiguous: old_string appears %d times - add surrounding context to make it unique)", i+1, n)
		}
		if idx := strings.Index(content, e.oldString); strings.Contains(content[idx+1:], e.oldString) {
			return fmt.Sprintf("(hunk %d ambiguous: old_string overlaps itself - add surrounding context to make it unique)", i+1)
		}
		content = strings.Replace(content, e.oldString, e.newString, 1)
		totalOld += len(e.oldString)
		totalNew += len(e.newString)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		return fmt.Sprintf("(write error: %v)", err)
	}
	return fmt.Sprintf("edited %s: %d hunks, -%d +%d bytes", path, len(edits), totalOld, totalNew)
}

// decodeEdits pulls the edits array out of the raw tool args, tolerating the
// JSON-decoded []any/map[string]any shapes. A missing new_string on a hunk is
// rejected (like edit_file) rather than silently deleting the match; a missing
// edits key returns ok=false so Run can name that specific malformed call.
func decodeEdits(args map[string]any) ([]multiEdit, bool) {
	rawList, ok := args["edits"].([]any)
	if !ok {
		return nil, false
	}
	out := make([]multiEdit, 0, len(rawList))
	for _, item := range rawList {
		m, ok := item.(map[string]any)
		if !ok {
			return nil, false
		}
		oldStr, _ := m["old_string"].(string)
		newStr, hasNew := m["new_string"].(string)
		if !hasNew {
			return nil, false
		}
		out = append(out, multiEdit{oldString: oldStr, newString: newStr})
	}
	return out, true
}

// multiEditTool is the registry entry for multi_edit: a side-effecting,
// approval-gated tool that mutates the file at args["path"] (so the driver
// snapshots one diff around the whole batch).
type multiEditTool struct{}

func (multiEditTool) Name() string           { return MultiEditName }
func (multiEditTool) Safe() bool             { return false }
func (multiEditTool) Mutates() bool          { return true }
func (multiEditTool) Schema() map[string]any { return multiEditSchema() }

func (multiEditTool) Run(_ context.Context, args map[string]any) string {
	path, _ := args["path"].(string)
	edits, ok := decodeEdits(args)
	if !ok {
		return `(malformed edits: multi_edit needs an "edits" array of {"old_string":...,"new_string":...} objects, each with a string new_string (use "" to delete))`
	}
	return MultiEdit(path, edits)
}

func (multiEditTool) InlineStatus(args map[string]any) string {
	path, _ := args["path"].(string)
	n := 0
	if list, ok := args["edits"].([]any); ok {
		n = len(list)
	}
	return fmt.Sprintf("▶ multi_edit: %s (%d hunks)", path, n)
}

func (multiEditTool) Failed(result string) bool {
	// Success is "edited …"; every error is paren-wrapped, so a leading "(" is
	// the failure signal (same shape as edit_file).
	return strings.HasPrefix(strings.TrimSpace(result), "(")
}

func (multiEditTool) TargetKey(args map[string]any) string {
	path, _ := args["path"].(string)
	return MultiEditName + "|" + path
}

func multiEditSchema() map[string]any {
	return map[string]any{
		"type": "function",
		"function": map[string]any{
			"name":        MultiEditName,
			"description": "Apply several exact-once replacements to ONE file in a single atomic call. Prefer this over multiple edit_file calls when changing 3+ spots in the same file - all hunks succeed or none are written, so the file is never left half-edited. Each hunk's old_string must appear exactly once at the moment it applies; hunks apply top to bottom against the running content.",
			"parameters": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"path": map[string]any{
						"type":        "string",
						"description": "File to edit. Relative paths resolve against the working directory.",
					},
					"edits": map[string]any{
						"type":        "array",
						"description": "Ordered list of replacements applied to the file.",
						"items": map[string]any{
							"type": "object",
							"properties": map[string]any{
								"old_string": map[string]any{
									"type":        "string",
									"description": "Exact substring to find. Must be unique at the point this hunk applies.",
								},
								"new_string": map[string]any{
									"type":        "string",
									"description": "Replacement. Empty string deletes the match.",
								},
							},
							"required": []string{"old_string", "new_string"},
						},
					},
				},
				"required": []string{"path", "edits"},
			},
		},
	}
}
