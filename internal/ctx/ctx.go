// Package ctx owns conversation messages, tool-output truncation, and
// newest-first packing.
package ctx

import (
	"fmt"
	"slices"
	"unicode/utf8"
)

type Role string

const (
	RoleSystem    Role = "system"
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
	RoleTool      Role = "tool"
)

type ToolCall struct {
	ID        string         `json:"id,omitempty"`
	Name      string         `json:"name"`
	Arguments map[string]any `json:"arguments"`
}

type Message struct {
	Role       Role       `json:"role"`
	Content    string     `json:"content"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
	ToolName   string     `json:"name,omitempty"`
}

// Tokens approximates token count as char/4 — good enough for budgeting.
func Tokens(s string) int { return (len(s) + 3) / 4 }

func (m Message) Tokens() int {
	n := Tokens(m.Content)
	for _, tc := range m.ToolCalls {
		n += Tokens(tc.Name)
		for k, v := range tc.Arguments {
			n += Tokens(k) + Tokens(fmt.Sprint(v))
		}
	}
	return n + 8
}

const (
	ToolOutputCap = 6000
	ToolHeadTail  = 2000
	// FixedSystem reserves budget for the embedded prompt + working-dir anchor
	// (see tui.buildSystem). PROMPT_SYS.md + anchor is ~3380 tokens (the
	// sharpened headless-run recipe and oversized-write rule grew it); the
	// buffer to 3500 keeps prompt edits from silently over-budgeting small-ctx
	// profiles. A tui test pins this against the live prompt — bump here when it
	// fails, never relax the assertion.
	FixedSystem = 3500
	FixedTools  = 1500
)

// ResponseReserve is the slice Budget keeps free for the model's response.
// Scales as ctxSize/8 so reasoning models get room (262k→32k, 1M→125k),
// floored at 8k so small-ctx profiles don't collapse history to nothing.
func ResponseReserve(ctxSize int) int {
	if r := ctxSize / 8; r > 8000 {
		return r
	}
	return 8000
}

// Truncate collapses oversized tool outputs to first 2k + last 2k tokens;
// inputs at or under 6k pass through unchanged. Head/tail can't overlap:
// >6k tokens means >24k bytes, well over the 16k kept. Boundaries snap to a
// valid UTF-8 rune start so non-ASCII output never breaks mid-sequence.
func Truncate(out string) string {
	total := Tokens(out)
	if total <= ToolOutputCap {
		return out
	}
	limit := ToolHeadTail * 4
	head := runeBoundaryDown(out, limit)
	tail := runeBoundaryUp(out, len(out)-limit)
	marker := fmt.Sprintf("\n───── truncated: %d tokens total · showing first %d + last %d · re-run narrower (grep/sed/head/tail) ─────\n",
		total, ToolHeadTail, ToolHeadTail)
	return out[:head] + marker + out[tail:]
}

// runeBoundaryDown walks i left to a rune start so out[:i] never ends
// mid-sequence. Safe for i == len(out).
func runeBoundaryDown(out string, i int) int {
	if i >= len(out) {
		return len(out)
	}
	for i > 0 && !utf8.RuneStart(out[i]) {
		i--
	}
	return i
}

// runeBoundaryUp walks i right to a rune start so out[i:] never starts
// mid-sequence. Safe for i <= 0.
func runeBoundaryUp(out string, i int) int {
	if i <= 0 {
		return 0
	}
	for i < len(out) && !utf8.RuneStart(out[i]) {
		i++
	}
	return i
}

// PackResult records what Pack kept: the packed messages and their count.
type PackResult struct {
	Messages []Message
	Kept     int
}

// Pack keeps whole messages newest-first until the budget is full, then
// returns them chronologically. The newest message is always kept, even if it
// alone exceeds the budget. A second pass (dropOrphanTools) drops tool
// messages whose assistant.tool_calls ancestor got trimmed off the top.
func Pack(history []Message, budget int) PackResult {
	kept := make([]Message, 0, len(history))
	used := 0
	// walk newest to oldest
	for i := len(history) - 1; i >= 0; i-- {
		cost := history[i].Tokens()
		if len(kept) > 0 && used+cost > budget {
			break
		}
		kept = append(kept, history[i])
		used += cost
	}
	slices.Reverse(kept)
	kept = dropOrphanTools(kept)
	// dropOrphanTools can empty the kept set when the newest message is a tool
	// result whose owning assistant fell just past the budget cut: the always-
	// keep-newest guard keeps the lone tool, then the orphan drop removes it,
	// leaving nothing — so the next request would carry only the system prompt
	// and silently lose the whole conversation mid-turn (reachable on small-ctx
	// profiles after a big tool output). Recover the newest assistant+tool-results
	// group whole, over budget if need be — the same deliberately-over-budget
	// guarantee a newest user message already gets.
	if len(kept) == 0 {
		kept = newestToolGroup(history)
	}
	return PackResult{
		Messages: kept,
		Kept:     len(kept),
	}
}

// newestToolGroup returns the assistant that issued the newest tool result
// together with every tool result answering it, chronologically — the minimal
// well-formed unit that honours "always keep the newest" when the newest
// history message is a tool result. nil when the newest message isn't an
// identifiable tool result (the budget walk already keeps non-tool newests) or
// no owning assistant exists (an unpairable tool can't be kept anyway).
func newestToolGroup(history []Message) []Message {
	if len(history) == 0 {
		return nil
	}
	last := history[len(history)-1]
	if last.Role != RoleTool || last.ToolCallID == "" {
		return nil
	}
	owner := -1
search:
	for i := len(history) - 2; i >= 0; i-- {
		if history[i].Role != RoleAssistant {
			continue
		}
		for _, tc := range history[i].ToolCalls {
			if tc.ID == last.ToolCallID {
				owner = i
				break search
			}
		}
	}
	if owner < 0 {
		return nil
	}
	// Parallel tool calls put [assistant(c1,c2), tool(c1), tool(c2)] at the tail,
	// so collect every tool result whose id the owning assistant issued — not
	// just the immediately-preceding one.
	ids := map[string]bool{}
	for _, tc := range history[owner].ToolCalls {
		if tc.ID != "" {
			ids[tc.ID] = true
		}
	}
	group := []Message{history[owner]}
	for i := owner + 1; i < len(history); i++ {
		if history[i].Role == RoleTool && ids[history[i].ToolCallID] {
			group = append(group, history[i])
		}
	}
	return group
}

// dropOrphanTools removes tool messages whose tool_call_id has no matching
// assistant.tool_calls entry earlier in the slice — sending one alone 400s on
// every OpenAI-compatible backend ("tool message without preceding
// tool_calls").
//
// Empty IDs are orphans on both ends: otherwise one empty-id assistant call
// would let every empty-id tool message ride through seen[""], the exact 400
// we guard against. An unidentifiable tool message has no valid pairing.
func dropOrphanTools(kept []Message) []Message {
	seen := map[string]bool{}
	out := kept[:0]
	for _, m := range kept {
		if m.Role == RoleAssistant {
			for _, tc := range m.ToolCalls {
				if tc.ID != "" {
					seen[tc.ID] = true
				}
			}
		}
		if m.Role == RoleTool && (m.ToolCallID == "" || !seen[m.ToolCallID]) {
			continue
		}
		out = append(out, m)
	}
	return out
}

// Budget subtracts the fixed reservations from the total context size.
func Budget(ctxSize int) int {
	b := ctxSize - FixedSystem - FixedTools - ResponseReserve(ctxSize)
	if b < 0 {
		return 0
	}
	return b
}
