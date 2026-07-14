package protocol

import (
	"bufio"
	"context"
	"encoding/json"
	"runtime"
	"strings"
	"sync"
	"testing"

	chmctx "github.com/codehamr/codehamr/internal/ctx"
)

// captureRunner builds a Runner whose emitted events land in the returned
// buffer, wired just enough to exercise the ask_user handshake.
func captureRunner() (*Runner, *syncBuffer) {
	buf := &syncBuffer{}
	return &Runner{
		out:       buf,
		approvals: map[string]chan approval{},
		asks:      map[string]chan askReply{},
	}, buf
}

// syncBuffer is a tiny concurrency-safe buffer: the turn goroutine emits while
// the test reads, so the race detector needs the writes guarded.
type syncBuffer struct {
	mu  sync.Mutex
	buf strings.Builder
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// events decodes every emitted NDJSON line into a map for assertions.
func (b *syncBuffer) events(t *testing.T) []map[string]any {
	t.Helper()
	var out []map[string]any
	sc := bufio.NewScanner(strings.NewReader(b.String()))
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal(line, &m); err != nil {
			t.Fatalf("bad emitted line %q: %v", line, err)
		}
		out = append(out, m)
	}
	return out
}

func askCall(prompt string, options ...any) *chmctx.ToolCall {
	return &chmctx.ToolCall{
		ID:   "call_ask",
		Name: askUserName,
		Arguments: map[string]any{
			"prompt":  prompt,
			"options": append([]any{}, options...),
		},
	}
}

// lastToolResult returns the content of the last tool message appended to
// history, the synthetic result runAskUserTool records.
func lastToolResult(t *testing.T, r *Runner) string {
	t.Helper()
	for i := len(r.history) - 1; i >= 0; i-- {
		if r.history[i].Role == chmctx.RoleTool {
			return r.history[i].Content
		}
	}
	t.Fatal("no tool result recorded in history")
	return ""
}

func TestAskUserReturnsSelectedOption(t *testing.T) {
	r, buf := captureRunner()
	call := askCall("Which framework?", "React", "Vue", "Svelte")

	done := make(chan bool, 1)
	go func() { done <- r.runAskUserTool(context.Background(), call) }()

	// The ask_user event must be emitted with the prompt and options so the GUI
	// can render the buttons.
	waitForAsk(t, r, call.ID)
	r.deliverAskReply(command{Type: "ask_user_response", CallID: call.ID, Selection: intp(1)})

	if !<-done {
		t.Fatal("runAskUserTool returned false (should keep dispatching)")
	}
	if got := lastToolResult(t, r); !strings.Contains(got, "Vue") {
		t.Fatalf("result should carry the chosen option, got %q", got)
	}

	// Assert the emitted ask_user event shape.
	var asked map[string]any
	for _, e := range buf.events(t) {
		if e["type"] == "ask_user" {
			asked = e
		}
	}
	if asked == nil {
		t.Fatal("no ask_user event emitted")
	}
	if asked["prompt"] != "Which framework?" {
		t.Fatalf("ask_user prompt wrong: %v", asked["prompt"])
	}
	opts, _ := asked["options"].([]any)
	if len(opts) != 3 {
		t.Fatalf("ask_user options wrong: %v", asked["options"])
	}
}

func TestAskUserAcceptsCustomAnswer(t *testing.T) {
	r, _ := captureRunner()
	call := askCall("Which framework?", "React", "Vue")

	done := make(chan bool, 1)
	go func() { done <- r.runAskUserTool(context.Background(), call) }()

	waitForAsk(t, r, call.ID)
	// selection -1 + custom text: the user typed their own answer.
	r.deliverAskReply(command{Type: "ask_user_response", CallID: call.ID, Selection: intp(-1), Custom: "SolidJS"})

	<-done
	if got := lastToolResult(t, r); !strings.Contains(got, "SolidJS") {
		t.Fatalf("custom answer should win over options, got %q", got)
	}
}

func TestAskUserRejectsMissingPrompt(t *testing.T) {
	r, _ := captureRunner()
	call := &chmctx.ToolCall{ID: "c", Name: askUserName, Arguments: map[string]any{
		"options": []any{"a"},
	}}
	if !r.runAskUserTool(context.Background(), call) {
		t.Fatal("should return true (records an error result, keeps dispatching)")
	}
	if got := lastToolResult(t, r); !strings.Contains(got, "prompt is required") {
		t.Fatalf("missing prompt should be reported, got %q", got)
	}
}

func TestAskUserRejectsNoOptions(t *testing.T) {
	r, _ := captureRunner()
	call := askCall("pick one") // no options
	if !r.runAskUserTool(context.Background(), call) {
		t.Fatal("should return true")
	}
	if got := lastToolResult(t, r); !strings.Contains(got, "at least one option") {
		t.Fatalf("empty options should be reported, got %q", got)
	}
}

func TestAskUserRejectsTooManyOptions(t *testing.T) {
	r, _ := captureRunner()
	call := askCall("pick", "1", "2", "3", "4", "5", "6") // 6 > max 5
	if !r.runAskUserTool(context.Background(), call) {
		t.Fatal("should return true")
	}
	if got := lastToolResult(t, r); !strings.Contains(got, "at most 5 options") {
		t.Fatalf("over-cap options should be reported, got %q", got)
	}
}

func TestAskUserCancelledByTurn(t *testing.T) {
	r, _ := captureRunner()
	call := askCall("Which?", "a", "b")

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan bool, 1)
	go func() { done <- r.runAskUserTool(ctx, call) }()

	waitForAsk(t, r, call.ID)
	cancel() // turn cancelled before the user answered

	if <-done {
		t.Fatal("a cancelled ask should return false (stop dispatching)")
	}
	// The pending ask must be unregistered so a late reply doesn't leak.
	r.approveMu.Lock()
	_, stillPending := r.asks[call.ID]
	r.approveMu.Unlock()
	if stillPending {
		t.Fatal("cancelled ask left registered")
	}
}

func TestAskUserSchemaAdvertisesCapAndTool(t *testing.T) {
	var found bool
	for _, tool := range buildTools() {
		if tool.Function.Name != askUserName {
			continue
		}
		found = true
		props, _ := tool.Function.Parameters["properties"].(map[string]any)
		opts, _ := props["options"].(map[string]any)
		if opts["maxItems"] != maxAskOptions {
			t.Fatalf("options.maxItems should be %d, got %v", maxAskOptions, opts["maxItems"])
		}
	}
	if !found {
		t.Fatal("askUser tool missing from buildTools()")
	}
}

func intp(i int) *int { return &i }

// waitForAsk spins until the ask handshake has registered its channel, so the
// test never delivers a reply before runAskUserTool is blocked on it.
func waitForAsk(t *testing.T, r *Runner, callID string) {
	t.Helper()
	for i := 0; i < 100000; i++ {
		r.approveMu.Lock()
		_, ok := r.asks[callID]
		r.approveMu.Unlock()
		if ok {
			return
		}
		runtime.Gosched()
	}
	t.Fatal("ask never registered")
}
