package tui

import (
	"context"
	"errors"
	"fmt"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/codehamr/codehamr/internal/cloud"
	"github.com/codehamr/codehamr/internal/llm"
)

// probeTimeout caps the activation hello-world request: long enough for a cold
// cloud route, short enough that a stuck backend doesn't hang "▶ probing".
const probeTimeout = 15 * time.Second

// probeMsg carries the outcome of an activation-time Probe (hello-world chat):
// validates URL+model+key in one round trip and harvests the live context
// window from response headers. profile is tagged explicitly so a late probe
// can't overwrite the wrong profile's window after a /models switch; cli tags
// the client instance the probe dialed with (mirroring pingMsg's baseURL tag),
// because two probes for the SAME profile can be in flight at once (startup
// probe + a /models or /hamrpass re-activation) and the profile name alone
// can't tell the superseded one from the fresh one.
type probeMsg struct {
	profile       string
	cli           *llm.Client
	contextWindow int
	budget        cloud.BudgetStatus
	silent        bool // suppress the "✓ active" line; startup probe only
	err           error
}

// probeBackend wraps llm.Client.Probe in a tea.Cmd, bounded by probeTimeout so
// a hung backend never freezes activation. silent=true (startup probe) skips
// the "✓ active" banner, just seeding live budget/ctx values.
func probeBackend(cli *llm.Client, profileName string, silent bool) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), probeTimeout)
		defer cancel()
		res, err := cli.Probe(ctx)
		return probeMsg{
			profile:       profileName,
			cli:           cli,
			contextWindow: res.ContextWindow,
			budget:        res.Budget,
			silent:        silent,
			err:           err,
		}
	}
}

// handleProbe consumes an activation-time Probe result. Success stores the live
// context window and prints the activation line; failure surfaces the error
// inline and leaves the active profile set. Late probes for a no-longer-active
// profile still update liveContextSize, ready for when the user switches back.
//
// Connection-state mutations (m.connected, m.budget) are gated on the probe's
// client still being the live one (rebuildClient swaps the pointer on every
// re-activation, so identity is exact). A probe for profile a finishing after
// a /models switch to b, or a hung probe finishing after a SAME-profile
// re-activation already succeeded, can't flip the fresh state on a stale
// outcome. The liveContextSize cache write below stays keyed on the profile
// name alone: seeding an inactive profile's window is deliberately allowed.
func (m Model) handleProbe(msg probeMsg) (tea.Model, tea.Cmd) {
	active := msg.cli == m.cli && msg.profile == m.cfg.Active
	if msg.err != nil {
		if active {
			m.connected = false
			// A 402 carries the depleted budget snapshot (Set=true, Remaining=0).
			// Paint it now; otherwise the status bar shows no "% pass" segment
			// until the first chat call also 402s.
			if msg.budget.Set {
				m.budget = msg.budget
			}
		}
		// Silent startup probes print no banner either way; an offline launch
		// shouldn't greet the user with "⚠ probe". connected=false suffices;
		// the next user action surfaces the real failure. Stale probes stay
		// quiet too: a superseded failure landing after the fresh probe's
		// "✓ active" would read as the backend dying right after coming up.
		if !msg.silent && active {
			m.appendLine(styleError.Render("⚠ probe " + msg.profile + ": " + probeErrorMessage(msg.err)))
		}
		return m, nil
	}
	if active {
		m.connected = true
	}
	if msg.budget.Set && active {
		m.budget = msg.budget
	}
	p, ok := m.cfg.Models[msg.profile]
	if !ok {
		// Profile vanished between dispatch and return (hand-edited config or
		// pruned by /models). Skip the cache write: an orphan key would
		// accumulate across a long session.
		return m, nil
	}
	if msg.contextWindow > 0 {
		m.liveContextSize[msg.profile] = msg.contextWindow
	}
	// Don't print "✓ active: <profile>" for a stale probe whose profile is no
	// longer active. (liveContextSize is set above.)
	if msg.silent || !active {
		return m, nil
	}
	suffix := ""
	if msg.contextWindow > 0 {
		suffix = fmt.Sprintf(" · ctx: %s", humanInt(msg.contextWindow))
	}
	// ActiveURL, not p.URL: only reached when this probe's profile is active,
	// and under a CODEHAMR_URL override the banner must name the endpoint that
	// was actually dialed, not the config value the override displaced.
	m.appendLine(styleOK.Render(fmt.Sprintf(
		"✓ active: %s · %s @ %s%s", msg.profile, p.LLM, m.cfg.ActiveURL(), suffix)))
	return m, nil
}

// probeErrorMessage maps cloud sentinel errors to human hints for the
// activation line. Falls back to the raw error string for anything else.
func probeErrorMessage(err error) string {
	switch {
	case errors.Is(err, cloud.ErrUnauthorized):
		return "key rejected"
	case errors.Is(err, cloud.ErrBudgetExhausted):
		return "budget exhausted"
	}
	if un, ok := errors.AsType[cloud.ErrUnreachable](err); ok {
		return "unreachable (" + un.Err.Error() + ")"
	}
	return err.Error()
}
