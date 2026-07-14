package protocol

import (
	"context"
	"strings"
	"testing"

	chmctx "github.com/codehamr/codehamr/internal/ctx"
)

// TestBashStreamsOutputDeltas drives a bash tool call through runTool in
// auto-approve mode and asserts the live output arrives as tool_output_delta
// events before the final tool_result — the whole point of the streaming path.
func TestBashStreamsOutputDeltas(t *testing.T) {
	r, buf := captureRunner()
	r.mode = ModeAuto
	r.sessionAllowed = map[string]bool{}

	call := &chmctx.ToolCall{
		ID:   "call_bash",
		Name: "bash",
		Arguments: map[string]any{
			// Two prints with a tiny gap so the coalescing streamer flushes at
			// least one delta mid-run rather than only the tail.
			"cmd": "printf 'hello\\n'; sleep 0.2; printf 'world\\n'",
		},
	}

	if !r.runTool(context.Background(), call) {
		t.Fatal("runTool returned false (turn cancelled?)")
	}

	events := buf.events(t)
	var deltas []string
	var result string
	var sawResult bool
	for _, e := range events {
		switch e["type"] {
		case "tool_output_delta":
			if e["callId"] != "call_bash" {
				t.Fatalf("delta wrong callId: %v", e["callId"])
			}
			deltas = append(deltas, e["text"].(string))
		case "tool_result":
			sawResult = true
			result, _ = e["output"].(string)
		}
	}

	if len(deltas) == 0 {
		t.Fatal("no tool_output_delta emitted; output did not stream")
	}
	if !sawResult {
		t.Fatal("no tool_result emitted")
	}
	joined := strings.Join(deltas, "")
	if !strings.Contains(joined, "hello") || !strings.Contains(joined, "world") {
		t.Fatalf("streamed deltas missing output; got %q", joined)
	}
	if !strings.Contains(result, "hello") || !strings.Contains(result, "world") {
		t.Fatalf("final result missing output; got %q", result)
	}
}
