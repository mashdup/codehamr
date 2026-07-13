package config

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	chmctx "github.com/codehamr/codehamr/internal/ctx"
)

// withTempConfigDir points os.UserConfigDir at a fresh temp dir for the test by
// overriding the per-OS env var it reads, so memory lands in an isolated
// location and never touches the developer's real config dir. Returns the temp
// root so the test can assert on file locations.
func withTempConfigDir(t *testing.T) string {
	t.Helper()
	tmp := t.TempDir()
	var key string
	switch runtime.GOOS {
	case "windows":
		key = "AppData"
	case "darwin":
		// os.UserConfigDir on darwin uses $HOME/Library/Application Support and
		// ignores XDG, so pin HOME instead.
		key = "HOME"
		tmp = filepath.Join(tmp, "home")
		if err := os.MkdirAll(filepath.Join(tmp, "Library", "Application Support"), 0o700); err != nil {
			t.Fatal(err)
		}
	default:
		key = "XDG_CONFIG_HOME"
	}
	t.Setenv(key, tmp)
	return tmp
}

// TestMemoryPathIsOutOfRepoAndPerProject: the memory file must live under the
// user config dir (never in the project), and two different projects must map
// to different files so knowledge never leaks across repos.
func TestMemoryPathIsOutOfRepoAndPerProject(t *testing.T) {
	withTempConfigDir(t)
	proj := t.TempDir()
	path := MemoryPath(proj)
	if path == "" {
		t.Fatal("MemoryPath returned empty with a valid config dir")
	}
	// Out of the repo: the memory path must not be under the project dir.
	if rel, err := filepath.Rel(proj, path); err == nil && !strings.HasPrefix(rel, "..") {
		t.Fatalf("memory path %q is INSIDE the project %q - must be out-of-repo", path, proj)
	}
	// Per-project: a different project keys to a different file.
	if other := MemoryPath(t.TempDir()); other == path {
		t.Fatalf("two projects collided on the same memory file: %q", path)
	}
}

// TestAppendAndLoadMemoryRoundTrips: a remembered fact must be readable back
// and reach the system prompt on the next load (the cross-chat learning path).
func TestAppendAndLoadMemoryRoundTrips(t *testing.T) {
	withTempConfigDir(t)
	proj := t.TempDir()

	if got := LoadMemory(proj); got != "" {
		t.Fatalf("fresh project should have no memory, got %q", got)
	}
	if _, err := AppendMemory(proj, "build with `go build ./...`"); err != nil {
		t.Fatal(err)
	}
	if _, err := AppendMemory(proj, "auth lives in internal/auth"); err != nil {
		t.Fatal(err)
	}
	mem := LoadMemory(proj)
	for _, want := range []string{"go build ./...", "internal/auth"} {
		if !strings.Contains(mem, want) {
			t.Fatalf("LoadMemory missing %q:\n%s", want, mem)
		}
	}
	// And it must surface in the built system prompt for the next chat.
	sp := SystemPrompt(proj)
	if !strings.Contains(sp, "internal/auth") || !strings.Contains(sp, "Project memory") {
		t.Fatalf("SystemPrompt did not embed project memory:\n%s", sp[:min(400, len(sp))])
	}
	if !strings.Contains(sp, "Working directory: "+proj) {
		t.Fatal("SystemPrompt dropped the working-directory anchor")
	}
}

// TestSystemPromptNoMemoryIsPlainAnchor: with no memory the prompt is exactly
// the embedded prompt plus the anchor - no stray preamble - so the FixedSystem
// reservation still holds for the common case.
func TestSystemPromptNoMemoryIsPlainAnchor(t *testing.T) {
	withTempConfigDir(t)
	proj := t.TempDir()
	want := DefaultSystemPrompt + "\n\nWorking directory: " + proj
	if got := SystemPrompt(proj); got != want {
		t.Fatal("SystemPrompt with no memory must equal embedded prompt + anchor")
	}
}

// TestMemoryFileStaysBounded: repeated appends must not grow the on-disk file
// without bound; the oldest facts are trimmed once past the cap.
func TestMemoryFileStaysBounded(t *testing.T) {
	withTempConfigDir(t)
	proj := t.TempDir()
	line := strings.Repeat("x", 500)
	var lastSize int
	for i := 0; i < 200; i++ {
		n, err := AppendMemory(proj, line)
		if err != nil {
			t.Fatal(err)
		}
		lastSize = n
	}
	if lastSize > memoryFileMaxBytes {
		t.Fatalf("memory file grew past cap: %d > %d", lastSize, memoryFileMaxBytes)
	}
}

// TestLoadedMemoryFitsFixedMemory pins the budget invariant: a maxed-out memory
// file, loaded and wrapped in the preamble exactly as SystemPrompt does, must
// fit the ctx.FixedMemory reservation. If a cap tweak breaks this, packing
// would silently over-fill the real context window. Bump memorySendCapBytes and
// ctx.FixedMemory together, never one alone.
func TestLoadedMemoryFitsFixedMemory(t *testing.T) {
	withTempConfigDir(t)
	proj := t.TempDir()
	// Fill well past the send cap so LoadMemory returns a full-cap payload.
	for i := 0; i < 400; i++ {
		if _, err := AppendMemory(proj, strings.Repeat("y", 200)); err != nil {
			t.Fatal(err)
		}
	}
	block := memoryPreamble + LoadMemory(proj) + "\n\n"
	if used := chmctx.Tokens(block); used > chmctx.FixedMemory {
		t.Fatalf("loaded memory block (%d tok) exceeds ctx.FixedMemory=%d; bump both memorySendCapBytes and FixedMemory together",
			used, chmctx.FixedMemory)
	}
}
