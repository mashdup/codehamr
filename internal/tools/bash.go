// Package tools holds the local executors (bash, read_file, write_file,
// edit_file) and the router that dispatches assistant tool calls by name.
package tools

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"
	"unicode/utf8"
)

// Wire-format tool names. One source so schema, router, and inline-status
// switch can't drift apart.
const (
	BashName      = "bash"
	WriteFileName = "write_file"
	EditFileName  = "edit_file"
	ReadFileName  = "read_file"
)

// maxBashTimeoutSeconds caps the per-call timeout_seconds the model can
// request, a backstop against runaway loops (`sleep 99999`, `while true`)
// that would otherwise tie up the turn until Ctrl+C.
const maxBashTimeoutSeconds = 3600

// backgroundNote is appended to a bash result whose shell exited 0 but left a
// child holding the output pipes open (`cmd &`, `nohup`). It doubles as the
// wire signal the protocol driver reads back (WasBackgrounded) to set the
// tool_result event's background flag, and tells the model the process
// outlives the turn.
const backgroundNote = "\n(a background process is still running after the shell exited)"

// WasBackgrounded reports whether a bash result carries the backgroundNote
// tag. The protocol driver reads it to flag the tool_result; kept here so the
// tag string has one owner.
func WasBackgrounded(result string) bool {
	return strings.Contains(result, backgroundNote)
}

// Bash runs one shell command through /bin/sh -c and returns combined
// stdout+stderr. Non-zero exit is not an error; the model sees the failure
// and reacts.
//
// A pre-cancelled parent (Ctrl+C raced the dispatch) returns "(cancelled)"
// before the blank-command check, so a trivially-cancelled call isn't
// mistaken for a valid empty-args invocation.
func Bash(parent context.Context, command string, timeout time.Duration) string {
	if parent.Err() != nil {
		return "(cancelled)"
	}
	if strings.TrimSpace(command) == "" {
		return "(empty command)"
	}
	ctxT, cancel := context.WithTimeout(parent, timeout)
	defer cancel()

	// shellPath is platform-split: /bin/sh on Unix, a resolved Git-Bash-style
	// sh.exe on Windows (where upstream's hardcoded /bin/sh can never exist).
	sh, shErr := shellPath()
	if shErr != nil {
		return "(" + shErr.Error() + ")"
	}
	cmd := exec.CommandContext(ctxT, sh, "-c", command)
	// Shell gets its own process group + a Cancel that kills the whole group
	// on cancel/timeout (Unix; no-op on Windows). Without it, backgrounded
	// children (`cmd &`) outlive the parent shell and leak.
	setProcessGroup(cmd)
	// Cap the wait for stdout/stderr pipes to close after /bin/sh exits.
	// Backgrounded children inherit those pipe fds, so without this Run's
	// pipe-copy goroutine blocks for the full timeout even though the shell
	// is gone.
	cmd.WaitDelay = 100 * time.Millisecond
	// Bounded combined output instead of CombinedOutput's unbounded buffer: a
	// high-throughput command (`cat big.iso`, `grep -r "" /`) can emit hundreds
	// of MB/s and OOM-kill the whole TUI well before the timeout or Ctrl+C
	// react. ctx.Truncate keeps only head+tail anyway, so nothing the model
	// would see is lost. Stdout and Stderr get the SAME writer value, which
	// os/exec detects and funnels through one pipe: no locking needed.
	buf := &headTailBuffer{}
	cmd.Stdout = buf
	cmd.Stderr = buf
	err := cmd.Run()
	s := buf.String()
	// Name the capture drop at the END of the output, where ctx.Truncate's
	// tail keep guarantees the model sees it: the in-band seam marker sits at
	// the ~1MB offset, always inside Truncate's dropped middle, and Truncate's
	// own "total" would count the collapsed string, under-reporting the real
	// size by orders of magnitude.
	if d := buf.droppedBytes(); d > 0 {
		s += fmt.Sprintf("\n(output capped at capture: %d bytes total, %d bytes dropped mid-stream)", buf.totalBytes(), d)
	}
	if err != nil {
		switch {
		case ctxT.Err() == context.DeadlineExceeded:
			return s + fmt.Sprintf("\n(timeout after %s)", timeout)
		case parent.Err() == context.Canceled || ctxT.Err() == context.Canceled:
			// User Ctrl+C; name it rather than leak "signal: killed" noise.
			return s + "\n(cancelled)"
		case errors.Is(err, exec.ErrWaitDelay):
			// Shell exited 0; err is non-nil only because a backgrounded child
			// held the pipes past WaitDelay, not a failure. Return output as-is
			// (no spurious (exit: ...)), but tag it so the harness can badge the
			// tool card and the model knows the process outlives the turn - on
			// Windows the process-group kill is a no-op, so it genuinely does.
			// After the cancel/timeout cases so those signals win over a
			// coincident delay.
			return s + backgroundNote
		default:
			// Exit errors go into the output, exactly what the model needs.
			s += fmt.Sprintf("\n(exit: %v)", err)
		}
	}
	return s
}

// bashTool is the registry entry for bash: a side-effecting shell tool that is
// gated by approval, does not mutate a tracked file, and reports failure via an
// appended "(exit: N)" / "(timeout after ...)" marker.
type bashTool struct{}

func (bashTool) Name() string       { return BashName }
func (bashTool) Safe() bool         { return false }
func (bashTool) Mutates() bool      { return false }
func (bashTool) Schema() map[string]any { return bashSchema() }

func (bashTool) Run(parent context.Context, args map[string]any) string {
	cmd, _ := args["cmd"].(string)
	// Default 2m, overridable per call up to 1h. Clamp seconds BEFORE the
	// Duration multiply: 1e18 would overflow int64 into a negative duration,
	// and 0.5 would truncate to 0 and cancel before the shell runs, so floor
	// at 1.
	timeout := 2 * time.Minute
	if secs, ok := args["timeout_seconds"].(float64); ok && secs > 0 {
		secs = min(max(secs, 1), maxBashTimeoutSeconds)
		timeout = time.Duration(secs) * time.Second
	}
	return Bash(parent, cmd, timeout)
}

func (bashTool) InlineStatus(args map[string]any) string {
	cmd, _ := args["cmd"].(string)
	return "▶ bash: " + firstLine(cmd)
}

func (bashTool) Failed(result string) bool {
	// "(empty command)" is bash's malformed-call outcome (missing/blank cmd),
	// exact-matched: successful bash output can legitimately start with "(", so
	// no prefix match. It must count as a failure or a model looping on empty
	// calls never builds a streak and slips past the backstop.
	t := strings.TrimSpace(result)
	return strings.Contains(result, "\n(exit: ") || strings.Contains(result, "(timeout after ") ||
		t == "(empty command)"
}

func (bashTool) TargetKey(args map[string]any) string {
	cmd, _ := args["cmd"].(string)
	if i := strings.IndexByte(cmd, '\n'); i >= 0 {
		cmd = cmd[:i]
	}
	return BashName + "|" + strings.TrimSpace(cmd)
}

// bashSchema is the OpenAI tool definition for bash, exposed by every profile.
func bashSchema() map[string]any {
	return map[string]any{
		"type": "function",
		"function": map[string]any{
			"name":        BashName,
			"description": "Run a shell command in the user's environment. Combined stdout+stderr is returned. Use targeted commands (head, tail, wc) to avoid the 6k truncation. For searching code use the `grep`/`glob` tools, not `grep -r`/`find` here; for reading a file use `read_file`.",
			"parameters": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"cmd": map[string]any{
						"type":        "string",
						"description": "The shell command to execute.",
					},
					"timeout_seconds": map[string]any{
						"type":        "integer",
						"description": "Optional per call timeout in seconds. Default 120, hard capped at 3600. Raise for commands you expect to run long (pytest on large suites, docker build, DB migrations).",
					},
				},
				"required": []string{"cmd"},
			},
		},
	}
}

// bashOutputHead / bashOutputTail bound what headTailBuffer retains: first 1MB
// plus last 1MB of combined output. Far above ctx.Truncate's ~24KB relevance
// threshold (nothing the model would ever see is dropped), small enough that a
// firehose command can't OOM the process.
const (
	bashOutputHead = 1 << 20
	bashOutputTail = 1 << 20
)

// headTailBuffer is an io.Writer keeping the first bashOutputHead and the last
// bashOutputTail bytes written, discarding the middle. The tail is a fixed ring
// so a firehose costs a bounded copy, never an allocation per write.
type headTailBuffer struct {
	head      []byte
	ring      []byte
	pos       int   // next write index in ring
	tailBytes int64 // total bytes routed to the ring
}

func (w *headTailBuffer) Write(p []byte) (int, error) {
	n := len(p)
	if room := bashOutputHead - len(w.head); room > 0 {
		take := min(room, len(p))
		w.head = append(w.head, p[:take]...)
		p = p[take:]
	}
	if len(p) == 0 {
		return n, nil
	}
	if w.ring == nil {
		w.ring = make([]byte, bashOutputTail)
	}
	w.tailBytes += int64(len(p))
	if len(p) >= bashOutputTail {
		copy(w.ring, p[len(p)-bashOutputTail:])
		w.pos = 0
		return n, nil
	}
	k := copy(w.ring[w.pos:], p)
	w.pos = (w.pos + k) % bashOutputTail
	if k < len(p) {
		w.pos = copy(w.ring, p[k:])
	}
	return n, nil
}

// droppedBytes is how many middle bytes the ring discarded; 0 while the whole
// output still fits head+tail.
func (w *headTailBuffer) droppedBytes() int64 {
	if d := w.tailBytes - int64(len(w.ring)); d > 0 {
		return d
	}
	return 0
}

// totalBytes is the full combined output size as written, kept or not.
func (w *headTailBuffer) totalBytes() int64 {
	return int64(len(w.head)) + w.tailBytes
}

// String reassembles head + tail. The in-band seam marker keeps the two
// halves from reading as contiguous; the accurate size report lives in the
// caller's end-of-output note (this marker sits at the ~1MB offset, inside
// the middle ctx.Truncate drops, so the model never reads it).
func (w *headTailBuffer) String() string {
	switch {
	case w.tailBytes == 0:
		return string(w.head)
	case w.tailBytes <= int64(len(w.ring)):
		return string(w.head) + string(w.ring[:w.tailBytes])
	default:
		return string(w.head) +
			fmt.Sprintf("\n───── %d bytes OMITTED here (capture cap) ─────\n", w.droppedBytes()) +
			string(w.ring[w.pos:]) + string(w.ring[:w.pos])
	}
}

func firstLine(s string) string {
	// IndexAny over both separators, not just '\n', so a CR or CRLF first line
	// (old-mac or pasted command) cuts at the same point instead of leaking a
	// bare '\r' that would yank the status line's cursor back. Mirrors
	// llm.firstLine.
	if i := strings.IndexAny(s, "\r\n"); i >= 0 {
		s = s[:i]
	}
	if len(s) > 120 {
		// Snap the cut back to a rune boundary, else a multi-byte char gets
		// split and the printed status carries invalid UTF-8 to the terminal.
		cut := 117
		for cut > 0 && !utf8.RuneStart(s[cut]) {
			cut--
		}
		s = s[:cut] + "..."
	}
	return s
}
