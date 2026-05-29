// Debug instrumentation. The whole file plus its four call sites
// (search for `dbgWrite`) are intentionally self-contained so this can
// be ripped out cleanly when no longer needed. Activated by `logging:
// true` in .codehamr/config.yaml; .codehamr/log.txt is truncated on
// every start so a session never appends onto a stale run.
package tui

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	chmctx "github.com/codehamr/codehamr/internal/ctx"
)

var (
	dbgMu   sync.Mutex
	dbgFile *os.File
)

// OpenDebugLog truncates <dir>/log.txt and opens it for writing. A failure
// is reported once on stderr and silently disables logging for the rest of
// the run — the debug log must never block the TUI from starting.
//
// 0o600 because the log captures every prompt the user submits — slash
// commands like /hamrpass <key> as well as bash arguments that may
// include secrets the user pasted into a heredoc. Even with the slash
// redaction below, the bash channel can carry creds in command-line
// arguments. Owner-only is the only honest answer.
func OpenDebugLog(dir string) {
	if dir == "" {
		return
	}
	path := filepath.Join(dir, "log.txt")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		fmt.Fprintln(os.Stderr, "⚠ debuglog:", err)
		return
	}
	dbgMu.Lock()
	dbgFile = f
	dbgMu.Unlock()
	dbgWritef("session", "codehamr started · project=%s", dir)
}

// CloseDebugLog flushes and closes the log. Idempotent.
func CloseDebugLog() {
	dbgMu.Lock()
	defer dbgMu.Unlock()
	if dbgFile != nil {
		_ = dbgFile.Close()
		dbgFile = nil
	}
}

// redactSlash strips secrets from a slash-command line before it lands
// in the debug log. /hamrpass <key> is the canonical case — the
// hamrpass key is a long-lived bearer token tied to the user's
// prepaid budget, and a debug log dropped on a shared box (or
// committed by accident, or attached to a bug report) shouldn't leak
// it. Other slash commands have nothing sensitive in their args, but
// the central hook means any future secret-bearing command is
// covered by editing one place.
//
// The split mirrors `runSlash`'s `strings.Fields(text)` — both must agree
// on what counts as the command name and its args, otherwise a multi-line
// `/hamrpass\n<key>` (Alt+Enter inserts a literal newline into the
// textarea, then submit forwards it to the dispatcher) activates the key
// successfully via runSlash but slips past a literal-space prefix match
// here, leaking the verbatim key into log.txt.
//
// The command-name match is case-folded: a mistyped `/HamrPass <key>` does
// not activate the key (dispatch is case-sensitive, so it errors out), but
// submit's redactSlash call would otherwise echo the verbatim token to
// scrollback, the recall ring, on-disk history, and log.txt — so redaction
// errs wider than dispatch, which is the safe direction.
func redactSlash(line string) string {
	fields := strings.Fields(line)
	if len(fields) == 0 || !strings.EqualFold(fields[0], "/hamrpass") {
		return line
	}
	if len(fields) == 1 {
		return line // no key portion to redact
	}
	return "/hamrpass <redacted>"
}

// dbgWritef appends one timestamped record. No-op when logging is off.
func dbgWritef(category, format string, args ...any) {
	dbgMu.Lock()
	defer dbgMu.Unlock()
	if dbgFile == nil {
		return
	}
	ts := time.Now().Format("15:04:05.000")
	body := fmt.Sprintf(format, args...)
	fmt.Fprintf(dbgFile, "[%s] %s\n%s\n\n", ts, category, body)
}

// dbgWriteMessage records a chmctx.Message in a human readable shape:
// content and tool calls each get their own labeled section.
// No-op when logging is off, so callers don't need to guard.
func dbgWriteMessage(category string, msg chmctx.Message) {
	dbgMu.Lock()
	enabled := dbgFile != nil
	dbgMu.Unlock()
	if !enabled {
		return
	}
	var b strings.Builder
	if msg.Content != "" {
		b.WriteString("CONTENT:\n")
		b.WriteString(msg.Content)
		b.WriteString("\n")
	}
	for _, tc := range msg.ToolCalls {
		args, _ := json.Marshal(tc.Arguments)
		fmt.Fprintf(&b, "TOOL_CALL %s id=%s args=%s\n", tc.Name, tc.ID, args)
	}
	if msg.ToolCallID != "" {
		fmt.Fprintf(&b, "tool=%s id=%s\n", msg.ToolName, msg.ToolCallID)
	}
	dbgWritef(category, "%s", strings.TrimRight(b.String(), "\n"))
}
