package tools

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/codehamr/codehamr/internal/config"
)

// RememberName is the wire name for the project-memory tool.
const RememberName = "remember"

// rememberMaxFactBytes caps a single fact so one call can't dump a whole file
// into memory. A durable fact is a sentence or two; anything longer is
// transcript, not memory.
const rememberMaxFactBytes = 2000

// Remember appends one distilled fact to the project's persistent memory,
// stored OUTSIDE the repo (see config.AppendMemory) and loaded into the system
// prompt of every future chat. projectDir keys which project's memory the fact
// joins. Errors return in the string, bash convention.
func Remember(projectDir, fact string) string {
	fact = strings.TrimSpace(fact)
	if fact == "" {
		return "(empty fact: pass a concrete durable fact to remember)"
	}
	if len(fact) > rememberMaxFactBytes {
		return fmt.Sprintf("(fact too long: %d bytes exceeds the %d cap - memory is for a distilled fact, not a transcript)", len(fact), rememberMaxFactBytes)
	}
	size, err := config.AppendMemory(projectDir, fact)
	if err != nil {
		return fmt.Sprintf("(memory error: %v)", err)
	}
	return fmt.Sprintf("remembered (project memory now %d bytes); this fact will load into every future chat for this project", size)
}

// rememberTool is the registry entry for `remember`: it has an external side
// effect (writes the out-of-repo memory file) so it is not Safe, but it does
// NOT edit a file at args["path"], so it doesn't Mutate (no diff snapshot).
type rememberTool struct{}

func (rememberTool) Name() string           { return RememberName }
func (rememberTool) Safe() bool             { return false }
func (rememberTool) Mutates() bool          { return false }
func (rememberTool) Schema() map[string]any { return rememberSchema() }

func (rememberTool) Run(_ context.Context, args map[string]any) string {
	fact, _ := args["fact"].(string)
	// Tools run with cwd == the project root (the CLI Getwd's it, the desktop
	// spawns the agent with cwd set), so os.Getwd keys memory to the right
	// project without threading projectDir through the stateless Tool contract.
	dir, err := os.Getwd()
	if err != nil {
		return fmt.Sprintf("(memory error: cannot resolve working directory: %v)", err)
	}
	return Remember(dir, fact)
}

func (rememberTool) InlineStatus(args map[string]any) string {
	fact, _ := args["fact"].(string)
	return "▶ remember: " + firstLine(fact)
}

func (rememberTool) Failed(result string) bool {
	// Success starts with "remembered "; every error is paren-wrapped.
	return strings.HasPrefix(strings.TrimSpace(result), "(")
}

func (rememberTool) TargetKey(map[string]any) string {
	// One memory file per project, so the target is the tool itself; a repeated
	// failing remember keys on the same identity and trips the loop backstop.
	return RememberName
}

func rememberSchema() map[string]any {
	return map[string]any{
		"type": "function",
		"function": map[string]any{
			"name":        RememberName,
			"description": "Save one durable fact about THIS project to persistent memory kept outside the repo (it does NOT create a file in the user's workspace). The fact loads into the system prompt of every future chat, so the agent keeps learning the project the more it's used. Call it PROACTIVELY whenever the user states or you discover a durable fact - even if they don't say \"remember\": a build/test/lint command, where a subsystem lives, a project convention, the tech stack, how it's deployed or run, a recurring gotcha, or a stated preference about how the project works. NEVER tell the user you've noted or recorded something unless you actually call this tool in the same turn. Do NOT record transient task state, secrets, or one-off chatter. Keep each fact to a sentence or two.",
			"parameters": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"fact": map[string]any{
						"type":        "string",
						"description": "The durable fact to remember, phrased so it's useful with no other context (e.g. 'Build with `go build ./...`; tests run via `go test ./...`.').",
					},
				},
				"required": []string{"fact"},
			},
		},
	}
}
