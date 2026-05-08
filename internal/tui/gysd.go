package tui

import (
	"context"
	"fmt"
	"math"
	"strings"
	"time"
	"unicode/utf8"

	tea "github.com/charmbracelet/bubbletea"

	chmctx "github.com/codehamr/codehamr/internal/ctx"
	"github.com/codehamr/codehamr/internal/gysd"
)

// verifyResultMsg carries a completed verify subprocess back to Update so
// gysd.Session.RecordVerify runs on the main goroutine — Session never
// mutates from a tea.Cmd. Includes the original tool-call ID and the
// command we sent to /bin/sh so the result message and ANSI/exit logging
// stay traceable to a single dispatch.
//
// turnCtx is the per-turn context this verify was launched against; Update
// compares it against m.turnCtx and drops a result whose owning turn is no
// longer live. Without this guard a verify subprocess from turn N that
// completes after the user has Ctrl+C'd and submitted turn N+1 would mutate
// gysd.Session of turn N+1 (false RedStreak bumps, evidence pool poisoned by
// stale green output, even an accepted `done` evidenced by a prior turn's
// verify). The phase.active() check in applyVerifyResult is not enough on its
// own — a fresh turn's phase is also active.
type verifyResultMsg struct {
	callID   string
	callName string
	command  string
	outcome  gysd.RunOutcome
	turnCtx  context.Context
}

// handleGYSDTool routes verify/done/ask to the gysd Session. verify spawns
// a subprocess (async — runs in tea.Cmd, lands as verifyResultMsg);
// done/ask are pure state mutations and apply synchronously.
func (m Model) handleGYSDTool(call chmctx.ToolCall) (tea.Model, tea.Cmd) {
	turnCtx := m.turnCtx
	switch call.Name {
	case gysd.ToolVerify:
		cmdStr, _ := call.Arguments["command"].(string)
		timeoutSec := argInt(call.Arguments, "timeout_seconds")
		run, timeout, r := m.gysd.PreVerify(cmdStr, timeoutSec)
		if !run {
			return m.applyGYSDResult(r, call.ID, call.Name, turnCtx)
		}
		m.phase = phaseRunning
		return m, dispatchVerify(turnCtx, call, cmdStr, timeout)

	case gysd.ToolDone:
		summary, _ := call.Arguments["summary"].(string)
		evidence, _ := call.Arguments["evidence"].(string)
		r := m.gysd.HandleDone(summary, evidence)
		return m.applyGYSDResult(r, call.ID, call.Name, turnCtx)

	case gysd.ToolAsk:
		question, _ := call.Arguments["question"].(string)
		r := m.gysd.HandleAsk(question)
		return m.applyGYSDResult(r, call.ID, call.Name, turnCtx)
	}
	// Unreachable — IsLoopTool gates this in dispatchNextTool. Defensive
	// fallthrough so a future tool-name mismatch surfaces visibly.
	m.appendLine(styleError.Render("⚠ unknown gysd tool: " + call.Name))
	m.endTurn()
	return m, nil
}

// applyGYSDResult turns a gysd.Result into a state mutation + tea.Cmd
// pair. Three outcomes: end the loop (accepted done), yield to user
// (rejected for S1-S5 / ask), or feed a tool-result back to the model.
// turnCtx travels into the synthetic tool-result so a stale GYSD result
// (e.g., a verifyResultMsg path that already passed its own staleness gate
// and is feeding back into chat) cannot land in a turn that has since been
// cancelled.
func (m Model) applyGYSDResult(r gysd.Result, callID, callName string, turnCtx context.Context) (tea.Model, tea.Cmd) {
	switch {
	case r.EndLoop:
		m.flushStreaming()
		if s := strings.TrimSpace(r.FinalSummary); s != "" {
			m.appendLine(styleOK.Render("✓ " + s))
		}
		m.finalizeTurn()
		m.endTurn()
		return m, nil

	case r.Yield:
		m.flushStreaming()
		if s := strings.TrimSpace(r.UserBlock); s != "" {
			m.appendLine(styleWarn.Render(s))
		}
		m.finalizeTurn()
		m.endTurn()
		return m, nil

	default:
		// Synthetic tool-result — flows through the same toolResultMsg
		// path as a real tool, so the chat loop continues uniformly.
		m.phase = phaseThinking
		return m, syntheticToolResult(r.ToolPayload, callID, callName, turnCtx)
	}
}

// dispatchVerify spawns the verify subprocess in a tea.Cmd. PreVerify has
// already validated and clamped; this just runs. The owning turnCtx travels
// on the result so applyVerifyResult can drop the message when the turn it
// came from is no longer live (Ctrl+C → resubmit during a long verify).
func dispatchVerify(parent context.Context, call chmctx.ToolCall, command string, timeout time.Duration) tea.Cmd {
	return func() tea.Msg {
		outcome := gysd.RunCommand(parent, command, timeout)
		return verifyResultMsg{
			callID:   call.ID,
			callName: call.Name,
			command:  command,
			outcome:  outcome,
			turnCtx:  parent,
		}
	}
}

// syntheticToolResult builds the closure that fakes a tool-result message
// arriving from a real tool dispatch. The chat loop in Update treats the
// resulting msg identically to a runToolCall response, including the
// turnCtx staleness check.
func syntheticToolResult(payload, callID, callName string, turnCtx context.Context) tea.Cmd {
	return func() tea.Msg {
		return toolResultMsg{
			Msg: chmctx.Message{
				Role:       chmctx.RoleTool,
				Content:    payload,
				ToolCallID: callID,
				ToolName:   callName,
			},
			turnCtx: turnCtx,
		}
	}
}

// applyVerifyResult is called from Update on verifyResultMsg. Renders the
// inline outcome marker for the user (live UX), then calls RecordVerify
// and turns the resulting Result into the appropriate state transition.
// Stale results from a cancelled turn are dropped before any mutation: the
// turnCtx tag must match the live turn (cancelled turn → idle phase →
// turnCtx nil; resubmit installs a fresh ctx that won't match the old one),
// AND phase must still be active (covers the brief window between cancel
// and resubmit where m.turnCtx happens to also be nil).
func (m Model) applyVerifyResult(msg verifyResultMsg) (tea.Model, tea.Cmd) {
	if msg.turnCtx != m.turnCtx || !m.phase.active() {
		return m, nil
	}
	m.appendLine(styleDim.Render(verifyOutcomeLine(msg.outcome)))

	r := m.gysd.RecordVerify(
		msg.command,
		msg.outcome.Output,
		msg.outcome.ExitCode,
		msg.outcome.Canceled,
	)
	return m.applyGYSDResult(r, msg.callID, msg.callName, msg.turnCtx)
}

// verifyOutcomeLine renders one indented status line per verify outcome
// so the user can see grün/rot at a glance without waiting for the next
// model response. The first non-blank line of output usually carries the
// pass/fail summary in pytest, cargo, go test, grep, etc. — preferring
// it over a blind tail keeps creative-open output legible too.
//
// ANSI is stripped from the snippet before the 160-byte cut: pytest, cargo
// et al. emit colour codes on every line, and a mid-CSI truncation lands
// on the terminal via tea.Println where it can flip the terminal into
// sticky-colour mode (or worse) until the next stray escape clears it.
// gysd.RecordVerify already scrubs the stored copy; the live UI line was
// the only un-scrubbed path.
func verifyOutcomeLine(o gysd.RunOutcome) string {
	icon := "✓"
	switch {
	case o.Canceled:
		icon = "⊘"
	case o.TimedOut, o.ExitCode != 0:
		icon = "✗"
	}
	snippet := gysd.StripANSI(firstNonBlankLine(o.Output))
	if snippet == "" {
		return fmt.Sprintf("  %s (no output, exit %d)", icon, o.ExitCode)
	}
	if len(snippet) > 160 {
		// Snap the byte cut down to the previous rune boundary; otherwise
		// a non-ASCII first line (umlauts, box-drawing, emoji) would be
		// sliced mid-sequence and tea.Println would dump invalid UTF-8 to
		// the user's terminal.
		cut := 157
		for cut > 0 && !utf8.RuneStart(snippet[cut]) {
			cut--
		}
		snippet = snippet[:cut] + "..."
	}
	return fmt.Sprintf("  %s %s", icon, snippet)
}

// firstNonBlankLine returns the first line of s with non-whitespace
// content. Used by verifyOutcomeLine; pulled out so the truncation logic
// stays linear instead of nested.
func firstNonBlankLine(s string) string {
	for line := range strings.SplitSeq(s, "\n") {
		if t := strings.TrimSpace(line); t != "" {
			return t
		}
	}
	return ""
}

// argInt extracts an integer argument from a tool-call arguments map.
// JSON unmarshalling produces float64 for numbers; returning 0 on missing/
// wrong-type lets callers treat 0 as "use default". Negative values from
// the model are also returned as 0 — gysd.PreVerify clamps anyway.
//
// NaN and ±Inf are also returned as 0: NaN comparisons evaluate to false,
// so the `n < 0` gate would let it through, and `int(NaN)` is
// implementation-defined in Go (yields MinInt64 on amd64). Both would
// then propagate into time.Duration arithmetic and produce nonsense. The
// JSON spec disallows non-finite numbers anyway, so a sane backend never
// sends them — defensive only.
//
// Huge positive floats (1e20 etc.) are clamped to math.MaxInt before the
// int conversion. Without this, `int(1e20)` overflows to MinInt64 on
// amd64; PreVerify's `if timeoutSec > 0` then silently falls back to the
// 60s default instead of clamping the request to MaxTimeout. A model that
// asks for the maximum timeout must land at MaxTimeout, not at
// `default-because-overflow`.
//
// The clamp uses `>=` rather than `>`: float64 has only 53 mantissa bits,
// so `float64(math.MaxInt64)` rounds *up* to 2^63 (one above the int64
// max). With strict `>`, a model that emits exactly the rounded value
// would slip past the gate and `int(2^63)` would overflow to MinInt64 on
// amd64 — the same silent-overflow regression in a one-value gap. `>=`
// catches the singular boundary so every non-finite-int input lands at
// MaxInt before the conversion.
func argInt(args map[string]any, name string) int {
	v, ok := args[name]
	if !ok {
		return 0
	}
	switch n := v.(type) {
	case float64:
		if n < 0 || math.IsNaN(n) || math.IsInf(n, 0) {
			return 0
		}
		if n >= math.MaxInt {
			return math.MaxInt
		}
		return int(n)
	case int:
		if n < 0 {
			return 0
		}
		return n
	}
	return 0
}
