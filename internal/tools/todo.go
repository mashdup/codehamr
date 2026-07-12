package tools

import (
	"context"
	"fmt"
	"strings"
	"sync"
)

// TodoWriteName is the wire name for the task-list tool.
const TodoWriteName = "todo_write"

// maxTodos caps a list so a runaway call can't grow it without bound; well
// above any real task breakdown.
const maxTodos = 60

// todoState valid values. A todo is exactly one of these at a time.
const (
	todoPending    = "pending"
	todoInProgress = "in_progress"
	todoCompleted  = "completed"
)

// todoStore holds the single active task list for the session. The whole call
// replaces the list (the model always sends the full state), so a plain
// mutex-guarded slice is enough - no per-item addressing to race on.
type todoStore struct {
	mu    sync.Mutex
	items []todoItem
}

type todoItem struct {
	content string
	status  string
}

// todos is the process-wide task list. One list per running session matches
// how the TUI runs a single conversation; a fresh process starts empty.
var todos = &todoStore{}

// replace swaps in a new list wholesale under the lock and returns a copy for
// rendering, so the caller formats without holding the mutex.
func (s *todoStore) replace(items []todoItem) []todoItem {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.items = items
	out := make([]todoItem, len(items))
	copy(out, items)
	return out
}

// statusMark renders a todo status as a checkbox glyph for the result text.
func statusMark(status string) string {
	switch status {
	case todoCompleted:
		return "[x]"
	case todoInProgress:
		return "[~]"
	default:
		return "[ ]"
	}
}

// renderTodos formats the list the way the model reads it back: one checkbox
// line per item, plus a one-line progress tally so it can see at a glance how
// much is left.
func renderTodos(items []todoItem) string {
	if len(items) == 0 {
		return "(todo list cleared)"
	}
	var b strings.Builder
	done, inProg := 0, 0
	for _, it := range items {
		switch it.status {
		case todoCompleted:
			done++
		case todoInProgress:
			inProg++
		}
	}
	fmt.Fprintf(&b, "Todos (%d done, %d in progress, %d total):\n", done, inProg, len(items))
	for _, it := range items {
		fmt.Fprintf(&b, "%s %s\n", statusMark(it.status), it.content)
	}
	return strings.TrimRight(b.String(), "\n")
}

// TodoWrite replaces the session task list with items and returns the rendered
// list. Validation (empty content, bad status, at-most-one in_progress) is
// enforced so the model keeps a coherent list; a failure leaves the previous
// list untouched. Paren-wrapped errors, bash convention.
func TodoWrite(items []todoItem) string {
	if len(items) > maxTodos {
		return fmt.Sprintf("(too many todos: %d exceeds the %d cap)", len(items), maxTodos)
	}
	inProgress := 0
	for i, it := range items {
		if strings.TrimSpace(it.content) == "" {
			return fmt.Sprintf("(todo %d: empty content)", i+1)
		}
		switch it.status {
		case todoPending, todoInProgress, todoCompleted:
		default:
			return fmt.Sprintf("(todo %d: invalid status %q - use pending, in_progress, or completed)", i+1, it.status)
		}
		if it.status == todoInProgress {
			inProgress++
		}
	}
	if inProgress > 1 {
		return fmt.Sprintf("(%d todos are in_progress - keep exactly one in_progress at a time)", inProgress)
	}
	return renderTodos(todos.replace(items))
}

// decodeTodos pulls the todos array out of the raw args, tolerating the
// JSON-decoded []any/map[string]any shapes. A missing status defaults to
// pending (a freshly-added task); a missing todos key returns ok=false so Run
// can name the malformed call.
func decodeTodos(args map[string]any) ([]todoItem, bool) {
	rawList, ok := args["todos"].([]any)
	if !ok {
		return nil, false
	}
	out := make([]todoItem, 0, len(rawList))
	for _, item := range rawList {
		m, ok := item.(map[string]any)
		if !ok {
			return nil, false
		}
		content, _ := m["content"].(string)
		status, ok := m["status"].(string)
		if !ok || status == "" {
			status = todoPending
		}
		out = append(out, todoItem{content: content, status: status})
	}
	return out, true
}

// todoWriteTool is the registry entry for todo_write: a Safe, non-mutating
// tool (it touches no file and has no external side effect, so it never needs
// approval).
type todoWriteTool struct{}

func (todoWriteTool) Name() string           { return TodoWriteName }
func (todoWriteTool) Safe() bool             { return true }
func (todoWriteTool) Mutates() bool          { return false }
func (todoWriteTool) Schema() map[string]any { return todoWriteSchema() }

func (todoWriteTool) Run(_ context.Context, args map[string]any) string {
	items, ok := decodeTodos(args)
	if !ok {
		return `(malformed todos: todo_write needs a "todos" array of {"content":...,"status":...} objects)`
	}
	return TodoWrite(items)
}

func (todoWriteTool) InlineStatus(args map[string]any) string {
	n := 0
	if list, ok := args["todos"].([]any); ok {
		n = len(list)
	}
	return fmt.Sprintf("▶ todo_write: %d items", n)
}

func (todoWriteTool) Failed(result string) bool {
	// Success starts with "Todos (" or "(todo list cleared)"; every validation
	// error is paren-wrapped. The cleared note also starts with "(todo ", so
	// exclude it explicitly and match the real error prefixes.
	t := strings.TrimSpace(result)
	if t == "(todo list cleared)" {
		return false
	}
	return strings.HasPrefix(t, "(too many todos:") ||
		strings.HasPrefix(t, "(todo ") ||
		strings.HasPrefix(t, "(malformed todos:") ||
		(strings.HasPrefix(t, "(") && strings.Contains(t, "in_progress at a time)"))
}

func (todoWriteTool) TargetKey(map[string]any) string {
	// One shared list, so the target is the tool itself; a repeated failing
	// write keys on the same identity and trips the loop backstop.
	return TodoWriteName
}

func todoWriteSchema() map[string]any {
	return map[string]any{
		"type": "function",
		"function": map[string]any{
			"name":        TodoWriteName,
			"description": "Record and update a task list for the current multi-step job. Send the FULL list every call (it replaces the previous one). Use it to plan work with several distinct steps and to mark progress: keep exactly one task in_progress, flip it to completed the moment it's done. Skip it for a trivial one-step change.",
			"parameters": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"todos": map[string]any{
						"type":        "array",
						"description": "The complete task list, replacing any previous list.",
						"items": map[string]any{
							"type": "object",
							"properties": map[string]any{
								"content": map[string]any{
									"type":        "string",
									"description": "What the task is.",
								},
								"status": map[string]any{
									"type":        "string",
									"enum":        []string{todoPending, todoInProgress, todoCompleted},
									"description": "One of pending, in_progress, completed. Keep at most one in_progress.",
								},
							},
							"required": []string{"content", "status"},
						},
					},
				},
				"required": []string{"todos"},
			},
		},
	}
}
