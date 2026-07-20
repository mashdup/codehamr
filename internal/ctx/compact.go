package ctx

import (
	"fmt"
	"strings"
)

// SummaryPrefix marks the single message that replaces the summarised older
// history. It's carried as a user-role message: user is the one role every
// OpenAI-compatible backend accepts anywhere in the sequence, so a summary can
// never reintroduce the second-system-message / orphan-tool wire shapes Pack's
// cleanup passes exist to prevent. The prefix lets the UI (and a human reading
// the debug log) tell the synthesised recap apart from a real user turn.
const SummaryPrefix = "[Earlier conversation summary - auto-compacted to fit the context window]\n\n"

// compactTriggerNum/compactTriggerDen set the fraction of the history Budget at
// which auto-compaction fires: at 80% the true prompt is close enough to the
// window that Pack would soon start silently evicting the oldest turns, so we
// summarise them into a recap before that lossy drop happens.
const (
	compactTriggerNum = 8
	compactTriggerDen = 10
)

// compactKeepNum/compactKeepDen set how much of the Budget the most recent
// turns keep verbatim after a compaction (30%). Small enough that summary +
// recent lands well under the trigger so one compaction buys many more turns
// before the next; large enough that the immediate working context (the current
// task and its last few tool exchanges) survives untouched.
const (
	compactKeepNum = 3
	compactKeepDen = 10
)

// HistoryTokens sums the packing cost of every message, the same per-message
// accounting Pack budgets against. It's the live measure of how full the
// conversation is, independent of any one request's packed window.
func HistoryTokens(history []Message) int {
	n := 0
	for _, m := range history {
		n += m.Tokens()
	}
	return n
}

// CompactionKeepRecent is the token budget of the most recent history kept
// verbatim through a compaction (see compactKeep*). Scales with the window so
// big-context profiles keep more live context than small ones.
func CompactionKeepRecent(ctxSize int) int {
	b := Budget(ctxSize)
	return b * compactKeepNum / compactKeepDen
}

// NeedsCompaction reports whether the conversation has grown past the trigger
// fraction of the history Budget (see compactTrigger*). False for a zero/void
// budget (a misconfigured tiny window): there's nothing a summary could safely
// shrink to, and Pack's over-budget guarantees already handle that degenerate
// case.
func NeedsCompaction(history []Message, ctxSize int) bool {
	b := Budget(ctxSize)
	if b <= 0 {
		return false
	}
	return HistoryTokens(history) > b*compactTriggerNum/compactTriggerDen
}

// SplitForCompaction returns the index that divides history into the older span
// to summarise (history[:i]) and the recent span kept verbatim (history[i:]).
// The recent span is the newest run of whole turns whose token cost stays within
// keepRecent; the boundary is then snapped FORWARD to the next user message so
// recent always starts at a clean turn boundary and can never begin mid
// tool-exchange (which would orphan a tool result off the assistant that issued
// it). Returns 0 when nothing can be peeled off (no older span to summarise),
// which callers treat as "skip compaction".
func SplitForCompaction(history []Message, keepRecent int) int {
	if len(history) == 0 {
		return 0
	}
	used := 0
	split := len(history)
	for i := len(history) - 1; i >= 0; i-- {
		cost := history[i].Tokens()
		// Keep at least one message in recent even if it alone exceeds
		// keepRecent (used == 0), mirroring Pack's always-keep-newest rule.
		if used > 0 && used+cost > keepRecent {
			break
		}
		used += cost
		split = i
	}
	// Snap forward to a user turn: recent must open on a user message so the
	// summarised older span carries away whole, well-formed turns. If no user
	// boundary follows (the recent run is one long tool exchange), split walks to
	// len(history) and the whole thing is summarised - safe, since the summary is
	// itself a user message and Pack re-anchors from there.
	for split < len(history) && history[split].Role != RoleUser {
		split++
	}
	return split
}

// RenderForSummary flattens the older span into a plain-text transcript for the
// summarisation request. Tool calls and their results are included in condensed
// form: what the model did and what it saw is exactly the context a good recap
// must preserve. Empty for an empty span.
func RenderForSummary(older []Message) string {
	var b strings.Builder
	for _, m := range older {
		switch m.Role {
		case RoleUser:
			fmt.Fprintf(&b, "USER: %s\n", strings.TrimSpace(m.Content))
		case RoleAssistant:
			if c := strings.TrimSpace(m.Content); c != "" {
				fmt.Fprintf(&b, "ASSISTANT: %s\n", c)
			}
			for _, tc := range m.ToolCalls {
				fmt.Fprintf(&b, "ASSISTANT called %s(%s)\n", tc.Name, condenseArgs(tc.Arguments))
			}
		case RoleTool:
			fmt.Fprintf(&b, "TOOL[%s] result: %s\n", m.ToolName, Truncate(strings.TrimSpace(m.Content)))
		case RoleSystem:
			fmt.Fprintf(&b, "NOTE: %s\n", strings.TrimSpace(m.Content))
		}
	}
	return b.String()
}

// condenseArgs renders a tool call's arguments as compact key=value pairs for
// the transcript. Order is non-deterministic (map range), which is fine: the
// summary is prose, not a reproducible artifact.
func condenseArgs(args map[string]any) string {
	if len(args) == 0 {
		return ""
	}
	parts := make([]string, 0, len(args))
	for k, v := range args {
		parts = append(parts, fmt.Sprintf("%s=%v", k, v))
	}
	return strings.Join(parts, ", ")
}

// ApplyCompaction replaces history[:split] with a single summary message,
// keeping history[split:] verbatim. The summary carries SummaryPrefix so it's
// identifiable, as a user-role message so it's always wire-legal. A blank
// summary or a non-positive split returns history unchanged: compaction is
// best-effort and must never drop context on a failed summarisation.
func ApplyCompaction(history []Message, split int, summary string) []Message {
	if split <= 0 || strings.TrimSpace(summary) == "" {
		return history
	}
	if split > len(history) {
		split = len(history)
	}
	out := make([]Message, 0, len(history)-split+1)
	out = append(out, Message{Role: RoleUser, Content: SummaryPrefix + strings.TrimSpace(summary)})
	out = append(out, history[split:]...)
	return out
}
