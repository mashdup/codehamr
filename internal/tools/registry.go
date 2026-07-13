package tools

import (
	"context"
	"fmt"
	"strings"

	chmctx "github.com/codehamr/codehamr/internal/ctx"
)

// Tool is the single contract every local executor implements. It colocates a
// tool's four concerns that used to be smeared across parallel switch
// statements in three packages: its wire schema, how it runs, how it renders in
// the TUI, and the per-tool policy the driver needs (approval, diff snapshot,
// failure shape, loop-detection identity). Adding a tool is now one new file
// implementing this interface plus one Register line in registry.go's init —
// the tui/protocol drivers never change.
type Tool interface {
	// Name is the wire-format tool name the model calls (e.g. "bash").
	Name() string
	// Schema is the OpenAI tool definition (the map[string]any shape the chat
	// payload carries).
	Schema() map[string]any
	// Run decodes its own arguments and executes, returning the raw result
	// string (bash convention: errors come back in the string, never as a Go
	// error). The router handles the truncated-args sentinel before Run is
	// reached, so args here is always real JSON.
	Run(ctx context.Context, args map[string]any) string
	// InlineStatus is the one-liner the TUI prints when the call starts.
	InlineStatus(args map[string]any) string
	// Safe reports a tool with no side effects (read_file): it never needs
	// approval. Everything else is gated by the driver's allow/deny UI.
	Safe() bool
	// Mutates reports a tool that edits a file at args["path"], so the driver
	// snapshots before/after and emits a diff.
	Mutates() bool
	// Failed reports whether this tool's result string is an error the model
	// should react to (feeds the repeated-failure nudge). Each tool owns its
	// own success/failure shape: write/edit wrap errors in parens while
	// read_file returns raw file bytes that can legitimately start with "(".
	Failed(result string) bool
	// TargetKey is the stable identity a repeated-failure loop is detected on:
	// tool name + its target (path for file tools, the command's first line for
	// bash), deliberately not the full args (a cosmetic retry change defeats
	// that).
	TargetKey(args map[string]any) string
}

// registry holds every tool in a fixed order (the order the model sees them in
// the payload, pinned by test) plus a name index for O(1) dispatch. Populated
// once from init; never mutated after, so no locking.
var (
	registry       []Tool
	registryByName = map[string]Tool{}
)

// Register adds a tool to the registry. Called only from init; a duplicate name
// is a programming error (two tools claiming the same wire name would make
// dispatch ambiguous), so it panics rather than silently shadowing.
func Register(t Tool) {
	if _, dup := registryByName[t.Name()]; dup {
		panic("tools: duplicate tool registration: " + t.Name())
	}
	registry = append(registry, t)
	registryByName[t.Name()] = t
}

// init wires the built-in tools in payload order: bash, read_file, write_file,
// edit_file, multi_edit, glob, grep, web_fetch, todo_write, remember. This is the single
// registration site — the "one line per tool" the interface buys. A new tool
// adds its file plus one Register call here.
func init() {
	Register(bashTool{})
	Register(readTool{})
	Register(writeTool{})
	Register(editTool{})
	Register(multiEditTool{})
	Register(globTool{})
	Register(grepTool{})
	Register(webFetchTool{})
	Register(todoWriteTool{})
	Register(rememberTool{})
}

// Lookup returns the registered tool for a wire name.
func Lookup(name string) (Tool, bool) {
	t, ok := registryByName[name]
	return t, ok
}

// Schemas returns every tool's OpenAI definition in registry order, for the
// chat payload's tool list.
func Schemas() []map[string]any {
	out := make([]map[string]any, len(registry))
	for i, t := range registry {
		out[i] = t.Schema()
	}
	return out
}

// Names returns every registered tool's wire name in order.
func Names() []string {
	out := make([]string, len(registry))
	for i, t := range registry {
		out[i] = t.Name()
	}
	return out
}

// Safe reports whether the named tool has no side effects (read_file). Unknown
// names are treated as unsafe so a hallucinated tool can't slip past approval.
func Safe(name string) bool {
	if t, ok := registryByName[name]; ok {
		return t.Safe()
	}
	return false
}

// Mutates reports whether the named tool edits a file (write_file/edit_file),
// so the driver snapshots for a diff.
func Mutates(name string) bool {
	if t, ok := registryByName[name]; ok {
		return t.Mutates()
	}
	return false
}

// Execute runs a tool call and returns the (possibly truncated) result ready
// to be appended to the conversation as a `tool` message.
func Execute(parent context.Context, call chmctx.ToolCall) chmctx.Message {
	raw := runRaw(parent, call)
	return chmctx.Message{
		Role:       chmctx.RoleTool,
		Content:    chmctx.Truncate(raw),
		ToolCallID: call.ID,
		ToolName:   call.Name,
	}
}

func runRaw(parent context.Context, call chmctx.ToolCall) string {
	// A truncated/oversized tool call leaves llm.resolve()'s _parse_error
	// sentinel where real args should be. Without this guard the call falls
	// through to an empty path/cmd and returns a misleading "(empty path)",
	// hiding that the server cut the arguments at its output-token limit, the
	// failure that makes a model re-emit the same too-large write for minutes.
	// Name the real cause and the recovery so it self-corrects in one step.
	if msg, ok := call.Arguments["_parse_error"].(string); ok {
		return fmt.Sprintf("(tool arguments were not valid JSON: %s, most likely the "+
			"content was too large and the server truncated the call at its output-token "+
			"limit. Do NOT retry the same whole-file write. Build the file in chunks with "+
			"bash heredoc append: `cat > path <<'EOF'` … `EOF` for the first part, then "+
			"repeated `cat >> path <<'EOF'` … `EOF` for each next part, then verify with "+
			"`wc -c path`.)", msg)
	}
	t, ok := registryByName[call.Name]
	if !ok {
		return fmt.Sprintf("(unknown tool: %s)", call.Name)
	}
	return t.Run(parent, call.Arguments)
}

// InlineStatus is the one-liner the TUI prints per tool call.
func InlineStatus(call chmctx.ToolCall) string {
	if t, ok := registryByName[call.Name]; ok {
		return t.InlineStatus(call.Arguments)
	}
	// Unknown tool (hallucinated name): fall back to the first non-empty string
	// arg so the status line still says something useful.
	for _, v := range call.Arguments {
		if s, ok := v.(string); ok && s != "" {
			return fmt.Sprintf("▶ %s: %s", call.Name, firstLine(s))
		}
	}
	return "▶ " + call.Name
}

// TargetKey is the stable identity used to detect a repeated-failure loop; see
// Tool.TargetKey. An unknown tool keys on its name alone.
func TargetKey(call chmctx.ToolCall) string {
	if t, ok := registryByName[call.Name]; ok {
		return t.TargetKey(call.Arguments)
	}
	return call.Name
}

// ResultFailed reports whether a tool result is an error the model should react
// to. Router-level failures (truncated/invalid JSON args, a hallucinated tool
// name) count under any name; otherwise the tool's own Failed shape decides.
func ResultFailed(name, result string) bool {
	if strings.Contains(result, "(cancelled)") {
		return false
	}
	// Router-level failures arrive under any tool name and bypass the per-tool
	// shapes: truncated/invalid JSON args (the failure that makes a model
	// re-emit the same too-large write for minutes) and a hallucinated tool
	// name. Both must count as failures or the repeated-failure nudge never
	// fires on exactly the loops it was built for.
	t := strings.TrimSpace(result)
	if strings.HasPrefix(t, "(tool arguments were not valid JSON") || strings.HasPrefix(t, "(unknown tool:") {
		return true
	}
	if tl, ok := registryByName[name]; ok {
		return tl.Failed(result)
	}
	return false
}
