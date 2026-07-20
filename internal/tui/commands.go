package tui

import (
	"context"
	"errors"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/codehamr/codehamr/internal/cloud"
	chmctx "github.com/codehamr/codehamr/internal/ctx"
	"github.com/codehamr/codehamr/internal/llm"
	"github.com/codehamr/codehamr/internal/tools"
)

// streamEventMsg and streamClosedMsg tag their originating channel so the model
// can drop events from a stream the current turn no longer owns. After Ctrl+C →
// fresh submit, the prior turn's readEvent Cmd is still scheduled; without the
// tag its tokens leak into the new turn, or its close runs endTurn against it.
type streamEventMsg struct {
	ch <-chan llm.Event
	e  llm.Event
}

type streamClosedMsg struct {
	ch <-chan llm.Event
}

// toolResultMsg carries one finished tool call back to Update, tagged with the
// turnCtx it was dispatched against. Update drops it when that ctx no longer
// matches m.turnCtx: the originating turn was Ctrl+C'd and superseded.
// Otherwise the orphan result appends to the new turn's history with no
// preceding assistant.tool_calls and abandons that turn's live stream.
type toolResultMsg struct {
	Msg     chmctx.Message
	turnCtx context.Context
}

// readEvent drains one event from the LLM stream as a tea.Msg, re-scheduled
// until the channel closes. Tags ch so Update can spot stale prior-turn events.
func readEvent(ch <-chan llm.Event) tea.Cmd {
	return func() tea.Msg {
		e, ok := <-ch
		if !ok {
			return streamClosedMsg{ch: ch}
		}
		return streamEventMsg{ch: ch, e: e}
	}
}

// runToolCall executes one tool call off the UI goroutine. parent is the
// per-turn root: Ctrl+C aborts the tool mid-run, and the toolResultMsg carries
// that ctx so Update can drop it if the turn has moved on.
//
// No outer timeout: bash owns its model-set per-call timeout (capped at 3600s
// by the schema), write_file/edit_file are filesystem-fast. An outer cap would
// silently override the model's bash timeout: a 30-min build dying at 3 min.
func runToolCall(parent context.Context, call chmctx.ToolCall) tea.Cmd {
	return func() tea.Msg {
		return toolResultMsg{Msg: tools.Execute(parent, call), turnCtx: parent}
	}
}

// compactionMsg carries the result of an auto-compaction summarisation back to
// Update, tagged with the turnCtx it was issued against so a Ctrl+C'd turn's
// late summary is dropped (same staleness guard as toolResultMsg). summary is
// empty on failure/cancel; the handler then leaves history untouched and starts
// the turn on the raw (uncompacted) window rather than losing context.
type compactionMsg struct {
	summary string
	err     error
	split   int
	turnCtx context.Context
}

// summarizeCmd runs the compaction summarisation off the UI goroutine. It builds
// the summariser request from a fixed instruction plus the rendered older span,
// calls the blocking llm.Summarize, and reports back via compactionMsg tagged
// with parent so a superseded turn's result is ignored.
func summarizeCmd(parent context.Context, cli *llm.Client, older []chmctx.Message, split int) tea.Cmd {
	return func() tea.Msg {
		msgs := []chmctx.Message{
			{Role: chmctx.RoleSystem, Content: compactionInstruction},
			{Role: chmctx.RoleUser, Content: chmctx.RenderForSummary(older)},
		}
		summary, err := cli.Summarize(parent, msgs)
		return compactionMsg{summary: summary, err: err, split: split, turnCtx: parent}
	}
}

// compactionInstruction steers the summariser: a dense, factual recap the main
// model can keep working from, not a chat pleasantry. It mirrors the recap
// contract in the compaction plan (topics, decisions, established context,
// pending work) and demands concrete identifiers survive since the summary
// replaces the only record of them.
const compactionInstruction = "You are compacting an in-progress coding session so it fits the context window. " +
	"Summarise the transcript below into a dense, factual recap the assistant can keep working from. " +
	"Preserve: the user's goals and requests; decisions and approaches taken; files, functions, and " +
	"commands touched (name them exactly); key findings from tool output; and any unfinished work or " +
	"open questions. Omit pleasantries and redundant detail. Write it as notes to your future self, not a " +
	"message to the user. Do not invent anything not present in the transcript."

// errorMessage maps a stream error into a one-line TUI hint, same format across
// all profiles.
func (m Model) errorMessage(e llm.Event) string {
	if e.Err == nil {
		return ""
	}
	switch {
	case errors.Is(e.Err, cloud.ErrBudgetExhausted):
		return "⚠ hamrpass depleted · top up at codehamr.com"
	case errors.Is(e.Err, cloud.ErrUnauthorized):
		return "⚠ key rejected · check models." + m.cfg.Active + ".key in .codehamr/config.yaml"
	case isUnreachable(e.Err):
		return "⚠ unreachable: " + m.cfg.ActiveURL() + " · /models to switch profile"
	default:
		return "⚠ " + e.Err.Error()
	}
}

func isUnreachable(err error) bool {
	_, ok := errors.AsType[cloud.ErrUnreachable](err)
	return ok
}
