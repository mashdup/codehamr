//go:build windows

package tools

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
)

// shellPath resolves a POSIX shell on Windows, where upstream's hardcoded
// /bin/sh can never exist. Git for Windows is the canonical source of one.
//
// Known Git install locations are probed BEFORE PATH: with WSL installed,
// PATH lookup of "bash" finds C:\Windows\System32\bash.exe, which boots a WSL
// distro — a different filesystem view and environment than the project dir
// the agent is anchored to. Resolved once and cached; a shell doesn't appear
// or vanish mid-session.
var shellOnce sync.Once
var shellExe string
var shellErr error

func shellPath() (string, error) {
	shellOnce.Do(func() {
		// The GUI harness bundles its own POSIX shell (busybox-w32 shipped as
		// sh.exe) and points here via CODEHAMR_SHELL, so a packaged app needs no
		// Git for Windows. Honoured first; the Git-Bash probing below stays as the
		// fallback for the TUI, a bare `codehamr` on PATH, or an absent bundle.
		if p := os.Getenv("CODEHAMR_SHELL"); p != "" {
			if st, err := os.Stat(p); err == nil && !st.IsDir() {
				shellExe = p
				return
			}
		}
		var candidates []string
		for _, env := range []string{"ProgramFiles", "ProgramFiles(x86)", "LocalAppData"} {
			if base := os.Getenv(env); base != "" {
				candidates = append(candidates,
					filepath.Join(base, "Git", "bin", "bash.exe"),
					filepath.Join(base, "Programs", "Git", "bin", "bash.exe"),
				)
			}
		}
		for _, p := range candidates {
			if st, err := os.Stat(p); err == nil && !st.IsDir() {
				shellExe = p
				return
			}
		}
		// PATH fallback: sh.exe first (never the WSL launcher; System32 ships
		// bash.exe only), then bash.exe for setups that renamed or relocated.
		for _, name := range []string{"sh.exe", "bash.exe"} {
			if p, err := exec.LookPath(name); err == nil {
				shellExe = p
				return
			}
		}
		shellErr = errors.New("no POSIX shell found: the bash tool needs one on Windows - install Git for Windows (https://gitforwindows.org) or put sh.exe on PATH")
	})
	return shellExe, shellErr
}

// setProcessGroup is a no-op on Windows: Setpgid and negative-PID kill aren't
// portable. exec.CommandContext's default Kill stops the shell itself;
// backgrounded children may outlive it, the same trade-off the TUI accepts.
func setProcessGroup(_ *exec.Cmd) {}
