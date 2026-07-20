package ctx

import (
	"strings"
	"testing"
)

// bigMsg builds a user message whose content is n runes, ~n/4 tokens, so tests
// can push HistoryTokens past a chosen budget deterministically.
func bigMsg(role Role, n int) Message {
	return Message{Role: role, Content: strings.Repeat("x", n)}
}

func TestHistoryTokensSumsMessages(t *testing.T) {
	h := []Message{
		{Role: RoleUser, Content: "hello"},
		{Role: RoleAssistant, Content: "world"},
	}
	want := h[0].Tokens() + h[1].Tokens()
	if got := HistoryTokens(h); got != want {
		t.Fatalf("HistoryTokens = %d, want %d", got, want)
	}
	if got := HistoryTokens(nil); got != 0 {
		t.Fatalf("HistoryTokens(nil) = %d, want 0", got)
	}
}

// TestNeedsCompactionTrigger: below 80% of Budget stays false, above trips.
func TestNeedsCompactionTrigger(t *testing.T) {
	const ctxSize = 65_536
	b := Budget(ctxSize)
	trigger := b * compactTriggerNum / compactTriggerDen

	// Just under the trigger: one message ~ (trigger-100) tokens.
	under := []Message{bigMsg(RoleUser, (trigger-100)*4)}
	if NeedsCompaction(under, ctxSize) {
		t.Fatalf("history at %d tok should not need compaction (trigger %d)", HistoryTokens(under), trigger)
	}
	// Comfortably over the trigger.
	over := []Message{bigMsg(RoleUser, (trigger+500)*4)}
	if !NeedsCompaction(over, ctxSize) {
		t.Fatalf("history at %d tok should need compaction (trigger %d)", HistoryTokens(over), trigger)
	}
}

// TestNeedsCompactionZeroBudget: a window too small to yield any history budget
// never triggers (nothing to shrink to).
func TestNeedsCompactionZeroBudget(t *testing.T) {
	if Budget(1000) != 0 {
		t.Fatalf("precondition: Budget(1000) should be 0, got %d", Budget(1000))
	}
	if NeedsCompaction([]Message{bigMsg(RoleUser, 40000)}, 1000) {
		t.Fatal("zero-budget window must never report NeedsCompaction")
	}
}

// TestSplitForCompactionKeepsRecentAtUserBoundary: the recent span begins on a
// user turn and stays within keepRecent; the older span is everything before it.
func TestSplitForCompaction(t *testing.T) {
	h := []Message{
		{Role: RoleUser, Content: "task one"},          // 0
		{Role: RoleAssistant, Content: "did one"},      // 1
		{Role: RoleUser, Content: "task two"},          // 2
		{Role: RoleAssistant, ToolCalls: []ToolCall{{ID: "c1", Name: "bash"}}}, // 3
		{Role: RoleTool, ToolCallID: "c1", ToolName: "bash", Content: "out"},    // 4
		{Role: RoleUser, Content: "task three"},        // 5
		{Role: RoleAssistant, Content: "did three"},    // 6
	}
	// keepRecent big enough for turns 5-6 but not 2-6: split lands on index 5.
	keep := h[5].Tokens() + h[6].Tokens() + 2
	split := SplitForCompaction(h, keep)
	if split != 5 {
		t.Fatalf("split = %d, want 5 (recent should start at the last user turn)", split)
	}
	if h[split].Role != RoleUser {
		t.Fatalf("recent span must start on a user turn, got %s", h[split].Role)
	}
}

// TestSplitForCompactionSnapsForwardOffToolResult: a keepRecent that would cut
// mid tool-exchange snaps forward to the next user turn so no orphan tool result
// starts the recent span.
func TestSplitForCompactionSnapsForwardOffToolResult(t *testing.T) {
	h := []Message{
		{Role: RoleUser, Content: "task"},                                       // 0
		{Role: RoleAssistant, ToolCalls: []ToolCall{{ID: "c1", Name: "bash"}}},  // 1
		{Role: RoleTool, ToolCallID: "c1", ToolName: "bash", Content: "output"}, // 2
		{Role: RoleAssistant, Content: "done"},                                  // 3
	}
	// keepRecent lands the raw cut on the tool result (index 2); with no later
	// user turn it must snap forward to len(h) so the whole thing summarises.
	keep := h[2].Tokens() + h[3].Tokens()
	split := SplitForCompaction(h, keep)
	if split != len(h) {
		t.Fatalf("split = %d, want %d (must snap past a dangling tool exchange)", split, len(h))
	}
}

func TestSplitForCompactionEmpty(t *testing.T) {
	if got := SplitForCompaction(nil, 1000); got != 0 {
		t.Fatalf("SplitForCompaction(nil) = %d, want 0", got)
	}
}

// TestApplyCompactionReplacesOlderSpan: older span becomes one summary user
// message carrying SummaryPrefix; the recent span is kept verbatim.
func TestApplyCompaction(t *testing.T) {
	h := []Message{
		{Role: RoleUser, Content: "old task"},
		{Role: RoleAssistant, Content: "old work"},
		{Role: RoleUser, Content: "recent task"},
		{Role: RoleAssistant, Content: "recent work"},
	}
	out := ApplyCompaction(h, 2, "did the old work")
	if len(out) != 3 {
		t.Fatalf("len(out) = %d, want 3 (summary + 2 recent)", len(out))
	}
	if out[0].Role != RoleUser || !strings.HasPrefix(out[0].Content, SummaryPrefix) {
		t.Fatalf("summary message malformed: role=%s content=%q", out[0].Role, out[0].Content)
	}
	if !strings.Contains(out[0].Content, "did the old work") {
		t.Fatal("summary content missing")
	}
	if out[1].Content != "recent task" || out[2].Content != "recent work" {
		t.Fatal("recent span not preserved verbatim")
	}
}

// TestApplyCompactionNoOpOnEmptySummary: a failed summarisation must never drop
// history.
func TestApplyCompactionNoOpOnEmptySummary(t *testing.T) {
	h := []Message{{Role: RoleUser, Content: "task"}, {Role: RoleAssistant, Content: "work"}}
	if out := ApplyCompaction(h, 1, "   "); len(out) != len(h) {
		t.Fatalf("blank summary must be a no-op, got len %d", len(out))
	}
	if out := ApplyCompaction(h, 0, "real summary"); len(out) != len(h) {
		t.Fatalf("split 0 must be a no-op, got len %d", len(out))
	}
}

// TestRenderForSummaryIncludesToolActivity: the transcript must carry the user
// goal, the tool call, and its result, the context a recap depends on.
func TestRenderForSummary(t *testing.T) {
	h := []Message{
		{Role: RoleUser, Content: "fix the bug"},
		{Role: RoleAssistant, Content: "looking", ToolCalls: []ToolCall{
			{Name: "bash", Arguments: map[string]any{"cmd": "go test"}},
		}},
		{Role: RoleTool, ToolName: "bash", Content: "FAIL"},
	}
	out := RenderForSummary(h)
	for _, want := range []string{"fix the bug", "bash", "cmd=go test", "FAIL"} {
		if !strings.Contains(out, want) {
			t.Fatalf("transcript missing %q:\n%s", want, out)
		}
	}
}

// TestCompactedHistoryFitsBudget: end to end, compacting an over-trigger history
// with a realistic summary brings HistoryTokens back under the trigger, so one
// compaction buys many more turns.
func TestCompactedHistoryFitsBudget(t *testing.T) {
	const ctxSize = 65_536
	// Build history well over the trigger from many turns.
	var h []Message
	for i := 0; i < 40; i++ {
		h = append(h, bigMsg(RoleUser, 4000), bigMsg(RoleAssistant, 4000))
	}
	if !NeedsCompaction(h, ctxSize) {
		t.Fatalf("precondition: history (%d tok) should exceed trigger", HistoryTokens(h))
	}
	split := SplitForCompaction(h, CompactionKeepRecent(ctxSize))
	if split <= 0 {
		t.Fatal("expected a positive split")
	}
	out := ApplyCompaction(h, split, strings.Repeat("summary ", 100))
	if NeedsCompaction(out, ctxSize) {
		t.Fatalf("post-compaction history (%d tok) still over trigger", HistoryTokens(out))
	}
}
