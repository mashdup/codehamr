package protocol

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/codehamr/codehamr/internal/config"
	chmctx "github.com/codehamr/codehamr/internal/ctx"
	"github.com/codehamr/codehamr/internal/llm"
)

// sseServer returns a test server that streams the given content as one SSE
// chunk plus [DONE], the minimal backend the summariser needs.
func sseServer(t *testing.T, content string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{\"content\":%q}}],\"usage\":{\"completion_tokens\":3}}\n\n", content)
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	t.Cleanup(srv.Close)
	return srv
}

// compactRunner builds a Runner wired to a summariser server, with the given
// live context window so NeedsCompaction is deterministic.
func compactRunner(t *testing.T, summary string, ctxWindow int) (*Runner, *syncBuffer) {
	t.Helper()
	srv := sseServer(t, summary)
	buf := &syncBuffer{}
	return &Runner{
		cfg:         &config.Config{Dir: t.TempDir()},
		client:      llm.New(srv.URL, "m", "", ""),
		out:         buf,
		liveCtxSize: ctxWindow,
		approvals:   map[string]chan approval{},
		asks:        map[string]chan askReply{},
	}, buf
}

// bigUserMsg builds a user message of ~n/4 tokens so tests can push
// HistoryTokens past a chosen budget.
func bigUserMsg(n int) chmctx.Message {
	return chmctx.Message{Role: chmctx.RoleUser, Content: strings.Repeat("x", n)}
}

// TestMaybeAutoCompactShrinksLongHistory: a history over the trigger is
// summarised in place, the older span collapses to one summary message, the
// recent span stays verbatim, and a `compacted` event is emitted.
func TestMaybeAutoCompactShrinksLongHistory(t *testing.T) {
	const ctxWindow = 65_536
	r, buf := compactRunner(t, "the summary", ctxWindow)

	big := strings.Repeat("x", 4000)
	for i := 0; i < 40; i++ {
		r.history = append(r.history,
			chmctx.Message{Role: chmctx.RoleUser, Content: big},
			chmctx.Message{Role: chmctx.RoleAssistant, Content: big})
	}
	// The just-submitted prompt, which must survive verbatim.
	r.history = append(r.history, chmctx.Message{Role: chmctx.RoleUser, Content: "current task"})

	if !chmctx.NeedsCompaction(r.history, ctxWindow) {
		t.Fatalf("precondition: history (%d tok) should exceed trigger", chmctx.HistoryTokens(r.history))
	}
	before := len(r.history)

	r.maybeAutoCompact(context.Background())

	if len(r.history) >= before {
		t.Fatalf("history did not shrink: before=%d after=%d", before, len(r.history))
	}
	if !strings.HasPrefix(r.history[0].Content, chmctx.SummaryPrefix) {
		t.Fatalf("first message must be the summary, got %q", r.history[0].Content)
	}
	if last := r.history[len(r.history)-1]; last.Content != "current task" {
		t.Fatalf("current task must survive verbatim, got %q", last.Content)
	}
	// The compacted event must be emitted with auto=true.
	var sawCompacted bool
	for _, e := range buf.events(t) {
		if e["type"] == "compacted" {
			sawCompacted = true
			if e["auto"] != true {
				t.Fatalf("auto-compaction must set auto=true, got %v", e["auto"])
			}
		}
	}
	if !sawCompacted {
		t.Fatal("no compacted event emitted")
	}
}

// TestMaybeAutoCompactNoOpUnderTrigger: a small history is left untouched and no
// event is emitted.
func TestMaybeAutoCompactNoOpUnderTrigger(t *testing.T) {
	r, buf := compactRunner(t, "unused", 65_536)
	r.history = []chmctx.Message{{Role: chmctx.RoleUser, Content: "hi"}}
	r.maybeAutoCompact(context.Background())
	if len(r.history) != 1 || r.history[0].Content != "hi" {
		t.Fatalf("small history must be untouched, got %+v", r.history)
	}
	if len(buf.events(t)) != 0 {
		t.Fatal("no event should be emitted when compaction isn't needed")
	}
}

// TestMaybeAutoCompactKeepsHistoryOnFailure: a backend failure leaves history
// intact so no context is lost.
func TestMaybeAutoCompactKeepsHistoryOnFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(500)
		fmt.Fprint(w, `{"error":{"message":"boom"}}`)
	}))
	t.Cleanup(srv.Close)

	const ctxWindow = 65_536
	r := &Runner{client: llm.New(srv.URL, "m", "", ""), out: &syncBuffer{}, liveCtxSize: ctxWindow}
	big := strings.Repeat("x", 4000)
	for i := 0; i < 40; i++ {
		r.history = append(r.history,
			chmctx.Message{Role: chmctx.RoleUser, Content: big},
			chmctx.Message{Role: chmctx.RoleAssistant, Content: big})
	}
	before := len(r.history)
	r.maybeAutoCompact(context.Background())
	if len(r.history) != before {
		t.Fatalf("failed compaction must not touch history: before=%d after=%d", before, len(r.history))
	}
}

// TestRunCompactReplacesWholeHistory: the manual compact command summarises
// everything into a single summary message and emits `compacted`.
func TestRunCompactReplacesWholeHistory(t *testing.T) {
	r, buf := compactRunner(t, "full recap", 65_536)
	r.history = []chmctx.Message{
		{Role: chmctx.RoleUser, Content: "task one"},
		{Role: chmctx.RoleAssistant, Content: "did one"},
		{Role: chmctx.RoleUser, Content: "task two"},
	}
	r.runCompact()

	if len(r.history) != 1 {
		t.Fatalf("manual compact must collapse to one message, got %d", len(r.history))
	}
	if !strings.HasPrefix(r.history[0].Content, compactedPrefix) {
		t.Fatalf("summary must carry compactedPrefix, got %q", r.history[0].Content)
	}
	if !strings.Contains(r.history[0].Content, "full recap") {
		t.Fatal("summary content missing")
	}
	var sawCompacted bool
	for _, e := range buf.events(t) {
		if e["type"] == "compacted" {
			sawCompacted = true
			if _, ok := e["auto"]; ok {
				t.Fatalf("manual compact must not set auto, got %v", e["auto"])
			}
		}
	}
	if !sawCompacted {
		t.Fatal("no compacted event from runCompact")
	}
}

// TestRunCompactEmptyHistory: compacting nothing emits a compacted event with
// HistoryLen 0 and does not call the model.
func TestRunCompactEmptyHistory(t *testing.T) {
	r, buf := compactRunner(t, "unused", 65_536)
	r.runCompact()
	if len(r.history) != 0 {
		t.Fatalf("empty compact must leave history empty, got %d", len(r.history))
	}
	events := buf.events(t)
	if len(events) != 1 || events[0]["type"] != "compacted" {
		t.Fatalf("expected one compacted event, got %+v", events)
	}
}
