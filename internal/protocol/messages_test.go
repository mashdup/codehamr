package protocol

import (
	"strings"
	"testing"

	chmctx "github.com/codehamr/codehamr/internal/ctx"
)

// newRunnerForMessages builds a Runner wired only enough to exercise
// buildMessages: liveCtxSize short-circuits activeContextSize so no config is
// needed.
func newRunnerForMessages(history []chmctx.Message, tree string) *Runner {
	return &Runner{
		system:      "EMBEDDED SYSTEM PROMPT",
		treeText:    tree,
		history:     history,
		liveCtxSize: 200000,
	}
}

func TestBuildMessagesAttachesTreeToUserTurnNotSystem(t *testing.T) {
	r := newRunnerForMessages(
		[]chmctx.Message{{Role: chmctx.RoleUser, Content: "fix the bug"}},
		"TREE_BLOCK",
	)
	out := r.buildMessages()

	sys := out[0]
	if sys.Role != chmctx.RoleSystem {
		t.Fatalf("first message must be system, got %q", sys.Role)
	}
	if strings.Contains(sys.Content, "TREE_BLOCK") {
		t.Fatalf("tree must NOT be glued to the system prefix:\n%s", sys.Content)
	}
	last := out[len(out)-1]
	if last.Role != chmctx.RoleUser {
		t.Fatalf("last message should be the user turn, got %q", last.Role)
	}
	if !strings.Contains(last.Content, "fix the bug") || !strings.Contains(last.Content, "TREE_BLOCK") {
		t.Fatalf("tree should ride on the user turn's content:\n%s", last.Content)
	}
}

func TestBuildMessagesSkipsTreeOnceShown(t *testing.T) {
	// After the session's first turn latched treeShown, a fresh user turn must
	// NOT carry the tree again — the model tracks its own edits from here.
	r := newRunnerForMessages(
		[]chmctx.Message{
			{Role: chmctx.RoleUser, Content: "first task"},
			{Role: chmctx.RoleAssistant, Content: "done"},
			{Role: chmctx.RoleUser, Content: "second task"},
		},
		"TREE_BLOCK",
	)
	r.treeShown = true
	out := r.buildMessages()
	for _, m := range out {
		if strings.Contains(m.Content, "TREE_BLOCK") {
			t.Fatalf("tree must not reappear after it was shown once:\n%+v", m)
		}
	}
}

func TestBuildMessagesSkipsTreeMidToolLoop(t *testing.T) {
	// Round > 0: the newest message is a tool result, so the model already saw
	// the tree when it planned this turn — don't re-transmit it.
	r := newRunnerForMessages([]chmctx.Message{
		{Role: chmctx.RoleUser, Content: "fix the bug"},
		{Role: chmctx.RoleAssistant, ToolCalls: []chmctx.ToolCall{{ID: "c1", Name: "read_file"}}},
		{Role: chmctx.RoleTool, ToolCallID: "c1", Content: "file contents"},
	}, "TREE_BLOCK")
	out := r.buildMessages()

	for _, m := range out {
		if strings.Contains(m.Content, "TREE_BLOCK") {
			t.Fatalf("tree must not be re-sent mid tool loop:\n%+v", m)
		}
	}
}

func TestBuildMessagesNoTreeIsClean(t *testing.T) {
	r := newRunnerForMessages(
		[]chmctx.Message{{Role: chmctx.RoleUser, Content: "hello"}},
		"",
	)
	out := r.buildMessages()
	if out[0].Content != "EMBEDDED SYSTEM PROMPT" {
		t.Fatalf("system prompt should be untouched when there is no tree:\n%s", out[0].Content)
	}
	if last := out[len(out)-1]; last.Content != "hello" {
		t.Fatalf("user turn should be untouched when there is no tree:\n%s", last.Content)
	}
}
