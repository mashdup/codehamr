package tools

import (
	"strings"
	"testing"
)

func TestTodoWriteRendersList(t *testing.T) {
	todos.replace(nil) // reset shared state
	out := TodoWrite([]todoItem{
		{content: "design", status: todoCompleted},
		{content: "build", status: todoInProgress},
		{content: "test", status: todoPending},
	})
	if !strings.Contains(out, "1 done, 1 in progress, 3 total") {
		t.Fatalf("tally wrong:\n%s", out)
	}
	if !strings.Contains(out, "[x] design") || !strings.Contains(out, "[~] build") || !strings.Contains(out, "[ ] test") {
		t.Fatalf("checkbox render wrong:\n%s", out)
	}
}

func TestTodoWriteReplacesPrevious(t *testing.T) {
	todos.replace(nil)
	TodoWrite([]todoItem{{content: "old", status: todoPending}})
	out := TodoWrite([]todoItem{{content: "new", status: todoPending}})
	if strings.Contains(out, "old") {
		t.Fatalf("list should be replaced wholesale:\n%s", out)
	}
	if !strings.Contains(out, "new") {
		t.Fatalf("new item missing:\n%s", out)
	}
}

func TestTodoWriteRejectsTwoInProgress(t *testing.T) {
	todos.replace(nil)
	out := TodoWrite([]todoItem{
		{content: "a", status: todoInProgress},
		{content: "b", status: todoInProgress},
	})
	if !strings.Contains(out, "in_progress at a time") {
		t.Fatalf("expected single-in-progress rule, got: %s", out)
	}
}

func TestTodoWriteRejectsBadStatusAndEmpty(t *testing.T) {
	todos.replace(nil)
	if out := TodoWrite([]todoItem{{content: "x", status: "bogus"}}); !strings.Contains(out, "invalid status") {
		t.Fatalf("bad status not caught: %s", out)
	}
	if out := TodoWrite([]todoItem{{content: "  ", status: todoPending}}); !strings.Contains(out, "empty content") {
		t.Fatalf("empty content not caught: %s", out)
	}
}

func TestTodoWriteClear(t *testing.T) {
	todos.replace([]todoItem{{content: "x", status: todoPending}})
	out := TodoWrite(nil)
	if !strings.Contains(out, "cleared") {
		t.Fatalf("expected cleared message, got: %s", out)
	}
}

func TestTodoDecodeDefaultsStatus(t *testing.T) {
	todos.replace(nil)
	args := map[string]any{
		"todos": []any{
			map[string]any{"content": "task with no status"},
		},
	}
	out := todoWriteTool{}.Run(nil, args)
	if !strings.Contains(out, "[ ] task with no status") {
		t.Fatalf("missing status should default to pending:\n%s", out)
	}
}

func TestTodoRunMalformed(t *testing.T) {
	out := todoWriteTool{}.Run(nil, map[string]any{})
	if !strings.Contains(out, "malformed todos") {
		t.Fatalf("expected malformed message, got: %s", out)
	}
}

func TestTodoFailedShape(t *testing.T) {
	tl := todoWriteTool{}
	if tl.Failed("(todo list cleared)") {
		t.Fatal("cleared is a success, must not be Failed")
	}
	if tl.Failed("Todos (0 done, 1 in progress, 2 total):\n[~] a\n[ ] b") {
		t.Fatal("rendered list must not be Failed")
	}
	if !tl.Failed("(todo 1: empty content)") {
		t.Fatal("validation error should be Failed")
	}
	if !tl.Failed("(2 todos are in_progress - keep exactly one in_progress at a time)") {
		t.Fatal("two-in-progress error should be Failed")
	}
}

func TestTodoPolicyFlags(t *testing.T) {
	tl := todoWriteTool{}
	if !tl.Safe() {
		t.Fatal("todo_write must be Safe (no side effects)")
	}
	if tl.Mutates() {
		t.Fatal("todo_write must not report Mutates (touches no file)")
	}
}
