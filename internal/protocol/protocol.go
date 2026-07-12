// Package protocol is the headless NDJSON driver behind `codehamr --json`:
// the same agent loop the TUI runs (pack history, stream a chat round, execute
// tool calls, repeat until the model stops calling tools), but speaking
// newline-delimited JSON over stdin/stdout so a GUI harness can drive it.
//
// One JSON object per line. Commands arrive on stdin (prompt, approve, cancel,
// set_model, get_models); events leave on stdout (ready, assistant_delta,
// tool_call, tool_result, turn_done, ...). Protocol version 1; bump V together
// with the TS schemas in the harness's packages/protocol.
//
// The TUI's soft-nudge machinery (runaway/failure/verify) is deliberately
// absent here: the harness user has a visible transcript and a cancel button,
// which is the backstop those nudges approximate in a terminal.
package protocol

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"
	"sync"

	"github.com/codehamr/codehamr/internal/config"
	chmctx "github.com/codehamr/codehamr/internal/ctx"
	"github.com/codehamr/codehamr/internal/llm"
	"github.com/codehamr/codehamr/internal/tools"
)

// V is the protocol schema version stamped on every line in both directions.
const V = 1

// fallbackContextSize mirrors config's unexported defaultContextSize: the
// packing budget when neither the profile nor a server X-Context-Window header
// has provided one (cloud profiles before their first response).
const fallbackContextSize = 32768

// maxCommandLine bounds one stdin command line. Prompts with base64 image
// attachments are the only large payloads; 32MB comfortably fits several
// screenshots while still refusing a runaway writer.
const maxCommandLine = 32 << 20

// command is every client→agent line, one flat struct: with a handful of
// small variants, a tagged union isn't worth the decode ceremony.
type command struct {
	V        int        `json:"v"`
	Type     string     `json:"type"`
	Text     string     `json:"text,omitempty"`     // prompt
	Images   []imageAtt `json:"images,omitempty"`   // prompt
	CallID   string     `json:"callId,omitempty"`   // approve
	Decision string     `json:"decision,omitempty"` // approve: allow|deny
	Scope    string     `json:"scope,omitempty"`    // approve: once|session
	Name     string     `json:"name,omitempty"`     // set_model
	Mode     string     `json:"mode,omitempty"`     // set_mode: ask|auto
}

// Permission modes. ModeAsk gates every side-effecting tool behind the
// harness's allow/deny handshake; ModeAuto runs them unattended. Auto is a
// deliberate user choice, never a default: this agent runs bash.
const (
	ModeAsk  = "ask"
	ModeAuto = "auto"
)

// imageAtt mirrors the harness's wire field names for one attachment.
type imageAtt struct {
	MIME    string `json:"mime"`
	DataB64 string `json:"dataB64"`
}

// modelInfo is one profile as shown to the harness. The key never crosses the
// wire: the GUI edits config.yaml directly when it needs to manage keys.
type modelInfo struct {
	Name        string `json:"name"`
	LLM         string `json:"llm"`
	URL         string `json:"url"`
	ContextSize int    `json:"contextSize"`
}

// event is every agent→client line, one flat struct with omitempty so each
// type serializes only its own fields, mirroring command's shape choice.
type event struct {
	V             int            `json:"v"`
	Type          string         `json:"type"`
	Version       string         `json:"version,omitempty"`       // ready
	Active        string         `json:"activeModel,omitempty"`   // ready, models
	Models        []modelInfo    `json:"models,omitempty"`        // ready, models
	Text          string         `json:"text,omitempty"`          // assistant_delta, reasoning_delta
	CallID        string         `json:"callId,omitempty"`        // tool_call, tool_result
	Name          string         `json:"name,omitempty"`          // tool_call
	Args          map[string]any `json:"args,omitempty"`          // tool_call
	NeedsApproval *bool          `json:"needsApproval,omitempty"` // tool_call
	OK            *bool          `json:"ok,omitempty"`            // tool_result
	Background    *bool          `json:"background,omitempty"`    // tool_result: bash left a process running
	Output        *string        `json:"output,omitempty"`        // tool_result
	Usage         *usage         `json:"usage,omitempty"`         // turn_done
	Message       string         `json:"message,omitempty"`       // error, log
	Fatal         *bool          `json:"fatal,omitempty"`         // error
	Level         string         `json:"level,omitempty"`         // log
	Path          string         `json:"path,omitempty"`          // file_diff, preview
	UnifiedDiff   string         `json:"unifiedDiff,omitempty"`   // file_diff
	URL           string         `json:"url,omitempty"`           // preview
	HistoryLen    int            `json:"historyLen,omitempty"`    // ready: restored messages
	Mode          string         `json:"mode,omitempty"`          // ready, mode
}

type usage struct {
	PromptTokens     int `json:"promptTokens"`
	CompletionTokens int `json:"completionTokens"`
	// ContextWindow is the effective window the agent packs against for this
	// turn (server header value, config context_size, or fallback). Sent so the
	// harness's context meter has a denominator even for server-managed
	// profiles whose config omits context_size. Set at turn_done.
	ContextWindow int `json:"contextWindow,omitempty"`
}

// Runner is one headless session: one workspace, one conversation history,
// one child of the GUI harness.
type Runner struct {
	cfg        *config.Config
	client     *llm.Client
	system     string
	version    string
	projectDir string
	// treeText is the current file-tree system-prompt block, refreshed at
	// each turn start so the model never opens with a discovery `ls -R` and
	// sees its own writes reflected next turn. Owned by the turn goroutine.
	treeText string

	outMu sync.Mutex
	out   io.Writer

	history []chmctx.Message
	// liveCtxSize is the server-authoritative window from X-Context-Window,
	// outranking the profile's on-disk value once seen (same policy as the TUI).
	liveCtxSize int

	// busy is owned by the stdin loop: set before spawning a turn goroutine,
	// cleared by the turn's done callback, read only in the stdin loop.
	busyMu sync.Mutex
	busy   bool

	turnMu     sync.Mutex
	turnCancel context.CancelFunc
	// turnSeq tokens each turn so a finished turn's deferred cleanup can't
	// null out the cancel func a newer turn just installed.
	turnSeq uint64

	// approvals routes an approve command to the turn goroutine blocked on
	// that callId. Registered before the tool_call event is emitted, so an
	// instant reply can't race the registration.
	approveMu sync.Mutex
	approvals map[string]chan approval
	// sessionAllowed holds tool names the user granted "session" scope;
	// their later calls skip the gate.
	sessionAllowed map[string]bool
	// mode is ModeAsk (gate every side-effecting tool) or ModeAuto (run them
	// unattended). Written by the stdin loop between turns, read by the turn
	// goroutine; set_mode is rejected mid-turn so the two never race.
	mode string

	// noImages latches on once the active endpoint rejects image input
	// (vision-less model). Images then stay in history — a later switch to a
	// vision model gets them back — but are stripped from the wire, so one
	// rejected image can't poison every following request. Reset on
	// set_model: the new endpoint may support vision. Owned by the turn
	// goroutine except for that idle-time reset, same discipline as client.
	noImages bool
}

type approval struct {
	allow bool
	scope string
}

// Run drives a session over stdin/stdout until stdin closes. projectDir
// anchors the system prompt exactly as the TUI does.
func Run(cfg *config.Config, client *llm.Client, projectDir, version string) error {
	r := &Runner{
		cfg:            cfg,
		client:         client,
		system:         config.DefaultSystemPrompt + "\n\nWorking directory: " + projectDir,
		version:        version,
		projectDir:     projectDir,
		out:            os.Stdout,
		approvals:      map[string]chan approval{},
		sessionAllowed: map[string]bool{},
		mode:           ModeAsk, // safe default; the harness opts into auto
	}
	r.loadSession()
	r.emitModels("ready")

	sc := bufio.NewScanner(os.Stdin)
	sc.Buffer(make([]byte, 64<<10), maxCommandLine)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var cmd command
		if err := json.Unmarshal(line, &cmd); err != nil {
			r.emitError(fmt.Sprintf("bad command line: %v", err), false)
			continue
		}
		r.dispatch(cmd)
	}
	// stdin closed: the harness is gone. Cancel any in-flight turn and leave.
	r.cancelTurn()
	return sc.Err()
}

func (r *Runner) dispatch(cmd command) {
	switch cmd.Type {
	case "prompt":
		if !r.tryAcquireTurn() {
			r.emitError("a turn is already in progress", false)
			return
		}
		go r.runTurn(cmd.Text, cmd.Images)
	case "approve":
		r.deliverApproval(cmd)
	case "cancel":
		r.cancelTurn()
	case "set_model":
		if r.isBusy() {
			r.emitError("cannot switch models mid-turn", false)
			return
		}
		if err := r.cfg.SetActive(cmd.Name); err != nil {
			r.emitError(err.Error(), false)
			return
		}
		p := r.cfg.ActiveProfile()
		r.client = llm.New(r.cfg.ActiveURL(), p.LLM, p.ResolvedKey())
		r.liveCtxSize = 0 // new endpoint, stale header value no longer applies
		r.noImages = false
		r.emitModels("models")
	case "get_models":
		r.emitModels("models")
	case "set_mode":
		if cmd.Mode != ModeAsk && cmd.Mode != ModeAuto {
			r.emitError("unknown mode: "+cmd.Mode, false)
			return
		}
		// Mid-turn switching would race the turn goroutine's gate checks and
		// silently change the rules under a pending approval.
		if r.isBusy() {
			r.emitError("cannot change permission mode mid-turn", false)
			return
		}
		r.mode = cmd.Mode
		r.emit(event{V: V, Type: "mode", Mode: r.mode})
	case "clear":
		if r.isBusy() {
			r.emitError("cannot clear mid-turn", false)
			return
		}
		r.history = nil
		_ = os.Remove(r.sessionPath())
		r.emit(event{V: V, Type: "cleared"})
	case "compact":
		if !r.tryAcquireTurn() {
			r.emitError("a turn is already in progress", false)
			return
		}
		go r.runCompact()
	default:
		r.emitError(fmt.Sprintf("unknown command type: %q", cmd.Type), false)
	}
}

// ---------------------------------------------------------------------------
// Turn loop
// ---------------------------------------------------------------------------

func (r *Runner) runTurn(text string, images []imageAtt) {
	turnCtx, cancel := context.WithCancel(context.Background())
	seq := r.installTurn(cancel)
	r.treeText = buildTreeSection(r.projectDir)
	// A panic in the turn loop must reach the harness as a readable fatal
	// error, not kill the process silently mid-session. The stack still goes
	// to stderr for the post-mortem; the session survives for the next prompt.
	defer func() {
		if p := recover(); p != nil {
			fmt.Fprintf(os.Stderr, "panic in turn: %v\n%s", p, debug.Stack())
			fatal := true
			r.finishTurn(event{V: V, Type: "error", Fatal: &fatal,
				Message: fmt.Sprintf("internal error (panic): %v — the session is still alive, but please report this", p)})
		}
	}()
	defer r.dropTurn(seq, cancel)

	user := chmctx.Message{Role: chmctx.RoleUser, Content: text}
	for _, img := range images {
		user.Images = append(user.Images, chmctx.Image{MIME: img.MIME, DataB64: img.DataB64})
	}
	// Warn on EVERY latched send, not only at latch time: silently stripping
	// an attachment the user just added reads as "the model is ignoring me".
	if r.noImages && len(user.Images) > 0 {
		r.emit(event{V: V, Type: "log", Level: "warn",
			Message: "attached image(s) NOT sent: this endpoint rejected image input earlier in the session — switch to a vision model (e.g. a *-vl model) to use them"})
	}
	preTurnLen := len(r.history)
	r.history = append(r.history, user)

	var lastUsage *usage
	rounds := 0
	// Loop backstops — the TUI has these; the GUI driver deliberately shipped
	// without them, so a weak model could hammer a failing tool for 30+ minutes
	// until the user hit Cancel. Halt with a clear error instead.
	toolRounds := 0
	failKey := ""
	failStreak := 0
	failNudges := 0
	runawayNudged := false
	for {
		final, u, err := r.chatRound(turnCtx)
		if u != nil {
			// Stamp the effective window so the harness's context meter has a
			// denominator even when the config profile omits context_size.
			u.ContextWindow = r.activeContextSize()
			lastUsage = u
		}
		if err != nil {
			if turnCtx.Err() != nil {
				// User cancel: not an error, the turn just ends here. History
				// keeps only what completed; Pack's dangling/orphan passes
				// handle any half-finished tool exchange on the next turn.
				r.finishTurn(event{V: V, Type: "turn_done"})
				return
			}
			// Vision-less endpoint rejecting image input: degrade instead of
			// failing the turn. Latch noImages (buildMessages then strips
			// images from the wire; history keeps them) and rerun the round.
			// One-shot by construction: the latch makes this branch
			// unreachable on the retry. This also self-heals a session
			// poisoned by an image message saved before the rollback fix.
			if !r.noImages && historyHasImages(r.history) && isImageRejection(err) {
				r.noImages = true
				r.emit(event{V: V, Type: "log", Level: "warn",
					Message: "this endpoint rejects image input (" + firstLine(err.Error()) + ") — continuing without the attached images; switch to a vision model to use them"})
				continue
			}
			// First-round failure: the model never saw this message, so keep
			// it out of history — otherwise a message the server rejects
			// outright is re-sent inside every later request and poisons the
			// whole session until a manual clear. Rolling back makes
			// retry-after-fixing clean. Mid-turn failures keep history:
			// completed rounds really happened.
			if rounds == 0 {
				r.history = r.history[:preTurnLen]
			}
			nonFatal := false
			r.finishTurn(event{V: V, Type: "error", Message: err.Error(), Fatal: &nonFatal})
			return
		}
		rounds++
		r.history = append(r.history, *final)
		r.emit(event{V: V, Type: "assistant_done"})

		if len(final.ToolCalls) == 0 {
			r.finishTurn(event{V: V, Type: "turn_done", Usage: lastUsage})
			return
		}
		for i := range final.ToolCalls {
			call := &final.ToolCalls[i]
			if !r.runTool(turnCtx, call) {
				r.finishTurn(event{V: V, Type: "turn_done"})
				return // cancelled mid-dispatch
			}
			toolRounds++
			// Classify the just-recorded result (last history message) and track a
			// same-target failure streak — the weak-model loop this backstop exists
			// for. A user’s denial does not count.
			failed := false
			if n := len(r.history); n > 0 {
				rc := r.history[n-1].Content
				failed = !strings.HasPrefix(strings.TrimSpace(rc), "(denied") &&
					toolResultFailed(call.Name, rc)
			}
			if failed {
				if k := toolTargetKey(*call); k == failKey && failKey != "" {
					failStreak++
				} else {
					failKey, failStreak = k, 1
				}
			} else {
				failKey, failStreak = "", 0
			}
			if failStreak >= maxToolFailStreak {
				failNudges++
				if failNudges >= maxFailNudges {
					nonFatal := false
					r.finishTurn(event{V: V, Type: "error", Fatal: &nonFatal,
						Message: "stopped: the model kept repeating the same failing tool call and did not recover after a nudge — read the tool error above, or try a stronger model."})
					return
				}
				r.history = append(r.history, chmctx.Message{Role: chmctx.RoleSystem, Content: failNudgeText(failStreak)})
				failKey, failStreak = "", 0
			}
		}
		// Runaway backstop for turns that never finish even as the failing target
		// varies (so the same-target streak keeps resetting).
		if !runawayNudged && toolRounds >= maxToolRounds {
			runawayNudged = true
			r.history = append(r.history, chmctx.Message{Role: chmctx.RoleSystem, Content: runawayNudgeText(toolRounds)})
		}
		if toolRounds >= maxToolRoundsHard {
			nonFatal := false
			r.finishTurn(event{V: V, Type: "error", Fatal: &nonFatal,
				Message: fmt.Sprintf("stopped: %d tool calls this turn without finishing — likely stuck in a loop. Try a stronger model or a more specific prompt.", toolRounds)})
			return
		}
	}
}

// installTurn registers this turn's cancel func and returns its token.
func (r *Runner) installTurn(cancel context.CancelFunc) uint64 {
	r.turnMu.Lock()
	defer r.turnMu.Unlock()
	r.turnSeq++
	r.turnCancel = cancel
	return r.turnSeq
}

// dropTurn releases the turn context; the seq guard keeps a finished turn's
// deferred cleanup from clearing a newer turn's cancel func.
func (r *Runner) dropTurn(seq uint64, cancel context.CancelFunc) {
	cancel()
	r.turnMu.Lock()
	if r.turnSeq == seq {
		r.turnCancel = nil
	}
	r.turnMu.Unlock()
}

// finishTurn persists the session, frees the busy slot, and only THEN emits
// the terminal event. The order is the contract: a client may send its next
// prompt the instant it sees turn_done/error, so busy must already be false —
// releasing in a defer raced exactly that and rejected the follow-up prompt.
// Every runTurn exit path funnels through here (the panic recovery included);
// nothing turn-owned may run after it.
func (r *Runner) finishTurn(e event) {
	r.saveSession()
	r.releaseTurn()
	r.emit(e)
}

// ---------------------------------------------------------------------------
// Compact: summarize the conversation into a single message, reclaiming
// context window on long sessions. Runs as a cancellable turn (busy slot + turn
// context) so the Stop button and mid-turn guards apply, calling the active
// model with a summarization prompt over a rendered transcript. On success the
// whole history is replaced by one summary message and a `compacted` event is
// emitted; on failure the history is left untouched.
// ---------------------------------------------------------------------------

const compactSystemPrompt = "You are compacting a coding session to save context window. " +
	"Summarize the conversation below into a concise but complete brief that lets the coding agent continue seamlessly. " +
	"Preserve: the user's goals and explicit requirements, key decisions and constraints, files created or modified and the substance of those changes, " +
	"important code/APIs discovered, commands run and their outcomes, and any unfinished tasks or next steps. " +
	"Omit pleasantries and repetition. Prefer bullet points. Output only the summary."

// compactedPrefix marks the summary message in history so a human reading
// session.json (or a later compaction) can tell it apart from a real user turn.
const compactedPrefix = "[Summary of the earlier conversation, compacted to save context]\n\n"

func (r *Runner) runCompact() {
	turnCtx, cancel := context.WithCancel(context.Background())
	seq := r.installTurn(cancel)
	defer func() {
		if p := recover(); p != nil {
			fmt.Fprintf(os.Stderr, "panic in compact: %v\n%s", p, debug.Stack())
			fatal := true
			r.finishTurn(event{V: V, Type: "error", Fatal: &fatal,
				Message: fmt.Sprintf("internal error (panic) during compact: %v", p)})
		}
	}()
	defer r.dropTurn(seq, cancel)

	prevLen := len(r.history)
	if prevLen == 0 {
		r.finishTurn(event{V: V, Type: "compacted", HistoryLen: 0, Message: "nothing to compact"})
		return
	}

	msgs := []chmctx.Message{
		{Role: chmctx.RoleSystem, Content: compactSystemPrompt},
		{Role: chmctx.RoleUser, Content: "Conversation to summarize:\n\n" + r.renderTranscript()},
	}

	var sb strings.Builder
	for ev := range r.client.Chat(turnCtx, msgs, nil) {
		switch ev.Kind {
		case llm.EventContent:
			sb.WriteString(ev.Content)
		case llm.EventError:
			if turnCtx.Err() != nil {
				r.finishTurn(event{V: V, Type: "turn_done"})
				return
			}
			nonFatal := false
			r.finishTurn(event{V: V, Type: "error", Fatal: &nonFatal,
				Message: "compact failed: " + ev.Err.Error()})
			return
		}
	}
	if turnCtx.Err() != nil { // cancelled after the stream closed cleanly
		r.finishTurn(event{V: V, Type: "turn_done"})
		return
	}

	summary := strings.TrimSpace(sb.String())
	if summary == "" {
		nonFatal := false
		r.finishTurn(event{V: V, Type: "error", Fatal: &nonFatal,
			Message: "compact produced an empty summary; conversation left unchanged"})
		return
	}

	r.history = []chmctx.Message{{Role: chmctx.RoleUser, Content: compactedPrefix + summary}}
	r.finishTurn(event{
		V: V, Type: "compacted",
		Text:       summary,
		HistoryLen: len(r.history),
		Message:    fmt.Sprintf("compacted %d messages into a summary", prevLen),
	})
}

// renderTranscript flattens history into a plain-text transcript for the
// summarizer, capped to roughly fit the active context (keeping the most recent
// text) so a huge history can't overflow the summarization request itself.
func (r *Runner) renderTranscript() string {
	var b strings.Builder
	for i := range r.history {
		m := &r.history[i]
		switch m.Role {
		case chmctx.RoleUser:
			b.WriteString("USER: ")
		case chmctx.RoleAssistant:
			b.WriteString("ASSISTANT: ")
		case chmctx.RoleTool:
			b.WriteString("TOOL[" + m.ToolName + "]: ")
		default:
			b.WriteString(strings.ToUpper(string(m.Role)) + ": ")
		}
		for _, tc := range m.ToolCalls {
			args, _ := json.Marshal(tc.Arguments)
			b.WriteString(fmt.Sprintf("«calls %s(%s)» ", tc.Name, truncateStr(string(args), 300)))
		}
		b.WriteString(m.Content)
		if len(m.Images) > 0 {
			b.WriteString(" [image attached]")
		}
		b.WriteString("\n\n")
	}
	s := b.String()
	// Cap to ~60% of the context (chars ≈ tokens*4) so the summarization request
	// itself fits; keep the most recent tail, which matters most.
	if maxChars := r.activeContextSize() * 4 * 6 / 10; maxChars > 0 && len(s) > maxChars {
		s = "[…earlier conversation truncated…]\n\n" + s[len(s)-maxChars:]
	}
	return s
}

func truncateStr(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// chatRound streams one LLM round, forwarding deltas as events, and returns
// the final assistant message. The tool calls inside it are executed by the
// caller; EventToolCall stream events are skipped because Final.ToolCalls
// carries the same resolved calls.
func (r *Runner) chatRound(turnCtx context.Context) (*chmctx.Message, *usage, error) {
	msgs := r.buildMessages()
	var final *chmctx.Message
	var u *usage
	for ev := range r.client.Chat(turnCtx, msgs, buildTools()) {
		switch ev.Kind {
		case llm.EventContent:
			r.emit(event{V: V, Type: "assistant_delta", Text: ev.Content})
		case llm.EventReasoning:
			r.emit(event{V: V, Type: "reasoning_delta", Text: ev.Content})
		case llm.EventDone:
			final = ev.Final
			if ev.ContextWindow > 0 {
				r.liveCtxSize = ev.ContextWindow
			}
			if ev.Tokens > 0 || ev.PromptTokens > 0 {
				u = &usage{PromptTokens: ev.PromptTokens, CompletionTokens: ev.Tokens}
			}
		case llm.EventError:
			return nil, u, ev.Err
		}
	}
	if final == nil {
		if err := turnCtx.Err(); err != nil {
			return nil, u, err
		}
		return nil, u, fmt.Errorf("stream closed without a final message")
	}
	// Local backends can stream tool calls with empty IDs; synthesize before
	// the call and its result enter history, or Pack's pairing passes would
	// drop the whole exchange as unpairable.
	for i := range final.ToolCalls {
		if final.ToolCalls[i].ID == "" {
			final.ToolCalls[i].ID = fmt.Sprintf("call_%d_%d", len(r.history), i)
		}
	}
	return final, u, nil
}

// runTool gates, executes, and records one tool call. Returns false when the
// turn was cancelled while gating or executing.
func (r *Runner) runTool(turnCtx context.Context, call *chmctx.ToolCall) bool {
	// Harness-only UI tools short-circuit: no shell, no approval, just an
	// event the GUI reacts to.
	if call.Name == previewFileName || call.Name == previewURLName {
		return r.runPreviewTool(call)
	}
	needs := r.needsApproval(call.Name)
	var decisionCh chan approval
	if needs {
		decisionCh = r.registerApproval(call.ID)
	}
	r.emit(event{
		V: V, Type: "tool_call",
		CallID: call.ID, Name: call.Name, Args: call.Arguments,
		NeedsApproval: &needs,
	})

	if needs {
		select {
		case d := <-decisionCh:
			if d.allow && d.scope == "session" {
				r.sessionAllowed[call.Name] = true
			}
			if !d.allow {
				r.recordToolResult(call, "(denied: the user declined to run this tool call)", false)
				return true
			}
		case <-turnCtx.Done():
			r.unregisterApproval(call.ID)
			return false
		}
	}

	// Snapshot the target before a mutating file tool runs so a diff can be
	// emitted after: the harness renders it inline in the tool card.
	var diffPath, diffBefore string
	if tools.Mutates(call.Name) {
		if p, _ := call.Arguments["path"].(string); p != "" {
			diffPath = p
			if b, err := os.ReadFile(p); err == nil {
				diffBefore = string(b)
			}
		}
	}

	result := tools.Execute(turnCtx, *call)
	if turnCtx.Err() != nil {
		// Cancelled mid-run: Execute already reported "(cancelled)"; record it
		// so the assistant's call stays paired, then stop dispatching.
		r.history = append(r.history, result)
		return false
	}
	r.history = append(r.history, result)
	ok := !toolResultFailed(call.Name, result.Content)
	te := event{V: V, Type: "tool_result", CallID: call.ID, OK: &ok, Output: &result.Content}
	if call.Name == tools.BashName && tools.WasBackgrounded(result.Content) {
		bg := true
		te.Background = &bg
	}
	r.emit(te)
	if ok && diffPath != "" {
		var after string
		if b, err := os.ReadFile(diffPath); err == nil {
			after = string(b)
		}
		if d := unifiedDiff(diffPath, diffBefore, after); d != "" {
			r.emit(event{V: V, Type: "file_diff", CallID: call.ID, Path: diffPath, UnifiedDiff: d})
		}
	}
	return true
}

// recordToolResult appends a synthetic tool message (e.g. a denial) to history
// and emits the matching tool_result, keeping the wire pairing intact.
func (r *Runner) recordToolResult(call *chmctx.ToolCall, content string, ok bool) {
	r.history = append(r.history, chmctx.Message{
		Role:       chmctx.RoleTool,
		Content:    content,
		ToolCallID: call.ID,
		ToolName:   call.Name,
	})
	r.emit(event{V: V, Type: "tool_result", CallID: call.ID, OK: &ok, Output: &content})
}

func (r *Runner) buildMessages() []chmctx.Message {
	budget := chmctx.Budget(r.activeContextSize())
	sys := r.system
	// The tree pays for itself out of the history budget (FixedSystem only
	// reserves for the embedded prompt), and is dropped entirely when it
	// would eat more than a quarter of a small context's budget.
	if r.treeText != "" {
		if tt := chmctx.Tokens(r.treeText); tt*4 < budget {
			sys += "\n\n" + r.treeText
			budget -= tt
		}
	}
	packed := chmctx.Pack(r.history, budget)
	out := make([]chmctx.Message, 0, len(packed.Messages)+1)
	out = append(out, chmctx.Message{Role: chmctx.RoleSystem, Content: sys})
	for _, m := range packed.Messages {
		if r.noImages {
			m.Images = nil // struct copy; history keeps the attachments
		}
		out = append(out, m)
	}
	return out
}

// historyHasImages reports whether any history message carries attachments —
// the precondition for reading an endpoint error as an image rejection.
func historyHasImages(history []chmctx.Message) bool {
	for i := range history {
		if len(history[i].Images) > 0 {
			return true
		}
	}
	return false
}

// isImageRejection matches the error shapes vision-less OpenAI-compatible
// servers return for image content. Each needle is the provider's own phrase:
// llama.cpp/Ollama's "image input is not supported ... provide the mmproj"
// (observed live) and the "does not support image(s)" family several shims
// use. Deliberately narrow — a random 500 must fail the turn loudly, not
// silently strip the user's attachments.
func isImageRejection(err error) bool {
	s := strings.ToLower(err.Error())
	return strings.Contains(s, "image input is not supported") ||
		strings.Contains(s, "mmproj") ||
		strings.Contains(s, "does not support image")
}

// firstLine mirrors llm.firstLine for compact log messages.
func firstLine(s string) string {
	if i := strings.IndexAny(s, "\r\n"); i >= 0 {
		return strings.TrimSpace(s[:i])
	}
	return strings.TrimSpace(s)
}

func (r *Runner) activeContextSize() int {
	if r.liveCtxSize > 0 {
		return r.liveCtxSize
	}
	if cs := r.cfg.ActiveProfile().ContextSize; cs > 0 {
		return cs
	}
	return fallbackContextSize
}

// needsApproval gates side-effecting tools (bash, write_file, edit_file)
// behind the harness's allow/deny UI. A tool the registry marks Safe
// (read_file) never gates; auto mode and a session-scope allow both lift it.
func (r *Runner) needsApproval(name string) bool {
	if tools.Safe(name) || r.mode == ModeAuto {
		return false
	}
	return !r.sessionAllowed[name]
}

// ---------------------------------------------------------------------------
// Preview tools: harness-only, protocol-mode-only. They don't touch the
// filesystem or shell — they emit a `preview` event the GUI turns into an
// open preview panel (file viewer or live browser). The TUI never sees them.
// ---------------------------------------------------------------------------

const (
	previewFileName = "preview_file"
	previewURLName  = "preview_url"
)

// runPreviewTool validates, emits the preview event, and records a synthetic
// result. Never blocks, never needs approval — the panel is visible and
// closable, which is the user's control.
func (r *Runner) runPreviewTool(call *chmctx.ToolCall) bool {
	needs := false
	r.emit(event{
		V: V, Type: "tool_call",
		CallID: call.ID, Name: call.Name, Args: call.Arguments,
		NeedsApproval: &needs,
	})
	switch call.Name {
	case previewFileName:
		p, _ := call.Arguments["path"].(string)
		abs, err := r.workspacePath(p)
		if err != nil {
			r.recordToolResult(call, "("+err.Error()+")", false)
			return true
		}
		if info, err := os.Stat(abs); err != nil || info.IsDir() {
			r.recordToolResult(call, "(not a previewable file: "+p+")", false)
			return true
		}
		r.emit(event{V: V, Type: "preview", Path: abs})
		r.recordToolResult(call, "(opened in the user's preview panel)", true)
	case previewURLName:
		u, _ := call.Arguments["url"].(string)
		u = strings.TrimSpace(u)
		if u == "" {
			r.recordToolResult(call, "(empty url)", false)
			return true
		}
		if !strings.HasPrefix(u, "http://") && !strings.HasPrefix(u, "https://") {
			u = "http://" + u // bare localhost:8080 etc.
		}
		r.emit(event{V: V, Type: "preview", URL: u})
		r.recordToolResult(call, "(opened in the user's live browser panel)", true)
	}
	return true
}

// workspacePath resolves p against the project dir and refuses paths that
// escape it — the preview panel is scoped to the workspace.
func (r *Runner) workspacePath(p string) (string, error) {
	if strings.TrimSpace(p) == "" {
		return "", fmt.Errorf("empty path")
	}
	abs := p
	if !filepath.IsAbs(abs) {
		abs = filepath.Join(r.projectDir, p)
	}
	abs = filepath.Clean(abs)
	root := filepath.Clean(r.projectDir)
	if abs != root && !strings.HasPrefix(abs, root+string(filepath.Separator)) {
		return "", fmt.Errorf("path is outside the workspace")
	}
	return abs, nil
}

func buildTools() []llm.Tool {
	// Registry tools first (bash, read_file, write_file, edit_file today), in
	// registry order — a new local tool joins automatically. The two preview
	// tools are harness-only and appended after.
	schemas := tools.Schemas()
	out := make([]llm.Tool, 0, len(schemas)+2)
	for _, s := range schemas {
		out = append(out, schemaToTool(s))
	}
	out = append(out,
		llm.Tool{
			Type: "function",
			Function: llm.FunctionDef{
				Name: previewFileName,
				Description: "Show a workspace file to the USER in the harness's preview panel " +
					"(code, markdown, images, pdf, docx). Use after creating or changing a file " +
					"the user should look at, or when they ask to see one. Shows it to the user " +
					"only — it returns no content to you; use read_file to read.",
				Parameters: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"path": map[string]any{
							"type":        "string",
							"description": "File path, absolute or relative to the working directory.",
						},
					},
					"required": []string{"path"},
				},
			},
		},
		llm.Tool{
			Type: "function",
			Function: llm.FunctionDef{
				Name: previewURLName,
				Description: "Open a URL in the USER's live browser panel inside the harness. " +
					"Use to show a running app or demo — e.g. http://localhost:5173 after starting " +
					"a dev server. Renders for the user only; nothing is returned to you.",
				Parameters: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"url": map[string]any{
							"type":        "string",
							"description": "URL to open, e.g. http://localhost:8080.",
						},
					},
					"required": []string{"url"},
				},
			},
		},
	)
	return out
}

// schemaToTool mirrors tui.schemaToTool: unwrap the shared map-shaped schema
// into the typed llm.Tool.
func schemaToTool(s map[string]any) llm.Tool {
	fn := s["function"].(map[string]any)
	return llm.Tool{
		Type: s["type"].(string),
		Function: llm.FunctionDef{
			Name:        fn["name"].(string),
			Description: fn["description"].(string),
			Parameters:  fn["parameters"].(map[string]any),
		},
	}
}

// --- Loop backstops: nudge, then halt, a weak model stuck repeating a failing
// tool. The TUI has richer versions; the GUI driver shipped without any, so a
// loop ran until the user cancelled. -----------------------------------------

const (
	maxToolFailStreak = 5   // same-target failures before a nudge (matches the TUI)
	maxFailNudges     = 2   // failure nudges that do not help → halt the turn
	maxToolRounds     = 75  // tool calls before the runaway nudge (matches the TUI)
	maxToolRoundsHard = 150 // absolute per-turn tool-call cap → halt the turn
)

const nudgeOrigin = "[Automated codehamr check - not a message from your user.] "

// toolTargetKey delegates to the tools registry for the identity a repeated-
// failure loop is detected on; each tool owns its own key shape there.
func toolTargetKey(call chmctx.ToolCall) string {
	return tools.TargetKey(call)
}

func failNudgeText(streak int) string {
	return nudgeOrigin + fmt.Sprintf(
		"The last %d tool calls to the same target failed the same way. Stop repeating it - read the error, change your approach, or tell the user what's blocking you.",
		streak)
}

func runawayNudgeText(rounds int) string {
	return nudgeOrigin + fmt.Sprintf(
		"%d tool calls so far this turn without finishing. If you're still making real progress, keep going. If you're repeating a step that can't work here - a blocked install, a missing tool, a path failing the same way - stop chasing it; verify another way or tell the user what's blocking you.",
		rounds)
}

// toolResultFailed delegates to the tools registry, where each tool owns its
// own success/failure shape and router-level failures are handled once.
func toolResultFailed(name, result string) bool {
	return tools.ResultFailed(name, result)
}

// ---------------------------------------------------------------------------
// Approvals, turn bookkeeping, emit
// ---------------------------------------------------------------------------

func (r *Runner) registerApproval(callID string) chan approval {
	ch := make(chan approval, 1)
	r.approveMu.Lock()
	r.approvals[callID] = ch
	r.approveMu.Unlock()
	return ch
}

func (r *Runner) unregisterApproval(callID string) {
	r.approveMu.Lock()
	delete(r.approvals, callID)
	r.approveMu.Unlock()
}

func (r *Runner) deliverApproval(cmd command) {
	r.approveMu.Lock()
	ch, ok := r.approvals[cmd.CallID]
	if ok {
		delete(r.approvals, cmd.CallID)
	}
	r.approveMu.Unlock()
	if !ok {
		r.emitError(fmt.Sprintf("no pending approval for callId %q", cmd.CallID), false)
		return
	}
	ch <- approval{allow: cmd.Decision == "allow", scope: cmd.Scope}
}

func (r *Runner) tryAcquireTurn() bool {
	r.busyMu.Lock()
	defer r.busyMu.Unlock()
	if r.busy {
		return false
	}
	r.busy = true
	return true
}

func (r *Runner) releaseTurn() {
	r.busyMu.Lock()
	r.busy = false
	r.busyMu.Unlock()
}

func (r *Runner) isBusy() bool {
	r.busyMu.Lock()
	defer r.busyMu.Unlock()
	return r.busy
}

func (r *Runner) cancelTurn() {
	r.turnMu.Lock()
	if r.turnCancel != nil {
		r.turnCancel()
	}
	r.turnMu.Unlock()
}

func (r *Runner) emitModels(typ string) {
	names := r.cfg.ModelNames()
	models := make([]modelInfo, 0, len(names))
	for _, name := range names {
		p := r.cfg.Models[name]
		models = append(models, modelInfo{
			Name: name, LLM: p.LLM, URL: p.URL, ContextSize: p.ContextSize,
		})
	}
	e := event{V: V, Type: typ, Version: r.version, Active: r.cfg.Active, Models: models}
	if typ == "ready" {
		e.HistoryLen = len(r.history)
		e.Mode = r.mode
	}
	r.emit(e)
}

// ---------------------------------------------------------------------------
// Session persistence: the conversation survives harness restarts.
// ---------------------------------------------------------------------------

func (r *Runner) sessionPath() string {
	return filepath.Join(r.cfg.Dir, "session.json")
}

// loadSession restores prior history. Corrupt or missing files start fresh;
// resuming a conversation is a convenience, never a startup blocker.
func (r *Runner) loadSession() {
	b, err := os.ReadFile(r.sessionPath())
	if err != nil {
		return
	}
	var msgs []chmctx.Message
	if json.Unmarshal(b, &msgs) != nil {
		return
	}
	r.history = msgs
}

// saveSession writes history via temp+rename so a crash mid-write can't leave
// a truncated file that loadSession would then half-trust. 0600 like the rest
// of .codehamr: conversations quote the user's code.
func (r *Runner) saveSession() {
	b, err := json.Marshal(r.history)
	if err != nil {
		return
	}
	path := r.sessionPath()
	tmp := path + ".tmp"
	if os.WriteFile(tmp, b, 0o600) != nil {
		return
	}
	_ = os.Rename(tmp, path)
}

func (r *Runner) emitError(msg string, fatal bool) {
	r.emit(event{V: V, Type: "error", Message: msg, Fatal: &fatal})
}

// emit writes one event line. The mutex serializes the turn goroutine against
// the stdin loop (busy-rejections, models replies), keeping lines whole.
func (r *Runner) emit(e event) {
	r.outMu.Lock()
	defer r.outMu.Unlock()
	b, err := json.Marshal(e)
	if err != nil {
		return
	}
	b = append(b, '\n')
	_, _ = r.out.Write(b)
}
