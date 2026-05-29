package tools

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	chmctx "github.com/codehamr/codehamr/internal/ctx"
)

func TestWriteFileHappy(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "hello.txt")
	content := "line one\nline two with 'quotes' and $dollar and `backticks`\n"
	s := WriteFile(path, content)
	if !strings.Contains(s, "wrote") || !strings.Contains(s, "hello.txt") {
		t.Fatalf("status wrong: %q", s)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("readback: %v", err)
	}
	if string(got) != content {
		t.Fatalf("content mismatch: %q", got)
	}
}

func TestWriteFileCreatesParentDirs(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "nested", "deep", "file.txt")
	s := WriteFile(path, "x")
	if !strings.Contains(s, "wrote 1 bytes") {
		t.Fatalf("status wrong: %q", s)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("file not created: %v", err)
	}
}

func TestWriteFileEmptyPath(t *testing.T) {
	if WriteFile("", "x") != "(empty path)" {
		t.Fatal("empty path handling wrong")
	}
}

// TestWriteFileMkdirErrorWhenParentIsFile exercises the (mkdir error) branch
// (write.go:18-19): if a path component is a regular file, os.MkdirAll fails
// and WriteFile must return the error as part of the output string (the bash
// convention), never as a Go error. Triggered via "a file in the path" rather
// than a read-only dir because tests run as root, and root bypasses directory
// permission bits — a chmod-based negative test would false-pass.
func TestWriteFileMkdirErrorWhenParentIsFile(t *testing.T) {
	dir := t.TempDir()
	blocker := filepath.Join(dir, "iam-a-file")
	if err := os.WriteFile(blocker, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	// blocker is a file, so MkdirAll(blocker/sub) must fail.
	got := WriteFile(filepath.Join(blocker, "sub", "out.txt"), "data")
	if !strings.HasPrefix(got, "(mkdir error:") {
		t.Fatalf("expected (mkdir error: ...) string, got %q", got)
	}
}

// TestWriteFileWriteErrorWhenTargetIsDir exercises the (write error) branch
// (write.go:22-23): writing to a path that is itself an existing directory
// fails at os.WriteFile, and the error must come back in the output string.
// Root-safe trigger (a directory target), same reason as above.
func TestWriteFileWriteErrorWhenTargetIsDir(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "imadir")
	if err := os.Mkdir(target, 0o755); err != nil {
		t.Fatal(err)
	}
	got := WriteFile(target, "data")
	if !strings.HasPrefix(got, "(write error:") {
		t.Fatalf("expected (write error: ...) string, got %q", got)
	}
}

func TestExecuteWriteFileWrapsResult(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "out.txt")
	call := chmctx.ToolCall{
		ID:   "call_w",
		Name: "write_file",
		Arguments: map[string]any{
			"path":    path,
			"content": "hello",
		},
	}
	msg := Execute(context.Background(), call)
	if msg.Role != chmctx.RoleTool || msg.ToolCallID != "call_w" || msg.ToolName != "write_file" {
		t.Fatalf("bad message: %+v", msg)
	}
	if !strings.Contains(msg.Content, "wrote 5 bytes") {
		t.Fatalf("content missing: %q", msg.Content)
	}
	got, _ := os.ReadFile(path)
	if string(got) != "hello" {
		t.Fatalf("file content wrong: %q", got)
	}
}

func TestInlineStatusWriteFile(t *testing.T) {
	s := InlineStatus(chmctx.ToolCall{
		Name:      "write_file",
		Arguments: map[string]any{"path": "/tmp/foo.txt", "content": "x"},
	})
	if !strings.HasPrefix(s, "▶ write_file: /tmp/foo.txt") {
		t.Fatalf("bad inline status: %q", s)
	}
}
