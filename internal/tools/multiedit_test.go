package tools

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeTemp(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "f.txt")
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func readBack(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestMultiEditAppliesAllHunksInOrder(t *testing.T) {
	p := writeTemp(t, "alpha beta gamma")
	out := MultiEdit(p, []multiEdit{
		{oldString: "alpha", newString: "ALPHA"},
		{oldString: "gamma", newString: "GAMMA"},
	})
	if strings.HasPrefix(out, "(") {
		t.Fatalf("unexpected failure: %s", out)
	}
	if got := readBack(t, p); got != "ALPHA beta GAMMA" {
		t.Fatalf("content = %q", got)
	}
}

func TestMultiEditHunkCanMatchEarlierResult(t *testing.T) {
	// A later hunk legitimately matches text an earlier hunk produced.
	p := writeTemp(t, "one")
	out := MultiEdit(p, []multiEdit{
		{oldString: "one", newString: "two"},
		{oldString: "two", newString: "three"},
	})
	if strings.HasPrefix(out, "(") {
		t.Fatalf("unexpected failure: %s", out)
	}
	if got := readBack(t, p); got != "three" {
		t.Fatalf("content = %q", got)
	}
}

func TestMultiEditAtomicOnFailure(t *testing.T) {
	p := writeTemp(t, "keep this line")
	out := MultiEdit(p, []multiEdit{
		{oldString: "keep", newString: "KEEP"},
		{oldString: "absent", newString: "X"}, // fails
	})
	if !strings.Contains(out, "hunk 2 not found") {
		t.Fatalf("expected hunk 2 failure, got: %s", out)
	}
	// The first hunk must NOT have been written: file untouched.
	if got := readBack(t, p); got != "keep this line" {
		t.Fatalf("file was mutated despite failure: %q", got)
	}
}

func TestMultiEditAmbiguousHunk(t *testing.T) {
	p := writeTemp(t, "x x x")
	out := MultiEdit(p, []multiEdit{{oldString: "x", newString: "y"}})
	if !strings.Contains(out, "ambiguous") {
		t.Fatalf("expected ambiguous, got: %s", out)
	}
	if got := readBack(t, p); got != "x x x" {
		t.Fatalf("file mutated on ambiguous hunk: %q", got)
	}
}

func TestMultiEditRejectsNoop(t *testing.T) {
	p := writeTemp(t, "abc")
	out := MultiEdit(p, []multiEdit{{oldString: "abc", newString: "abc"}})
	if !strings.Contains(out, "no change") {
		t.Fatalf("expected no-change rejection, got: %s", out)
	}
}

func TestMultiEditDeleteWithEmptyNew(t *testing.T) {
	p := writeTemp(t, "remove-me keep")
	out := MultiEdit(p, []multiEdit{{oldString: "remove-me ", newString: ""}})
	if strings.HasPrefix(out, "(") {
		t.Fatalf("delete should succeed, got: %s", out)
	}
	if got := readBack(t, p); got != "keep" {
		t.Fatalf("content = %q", got)
	}
}

func TestMultiEditEmptyEdits(t *testing.T) {
	p := writeTemp(t, "x")
	if out := MultiEdit(p, nil); !strings.Contains(out, "at least one") {
		t.Fatalf("expected empty-edits message, got: %s", out)
	}
}

func TestMultiEditDecodeAndRun(t *testing.T) {
	p := writeTemp(t, "hello world")
	args := map[string]any{
		"path": p,
		"edits": []any{
			map[string]any{"old_string": "hello", "new_string": "hi"},
		},
	}
	out := multiEditTool{}.Run(nil, args)
	if strings.HasPrefix(out, "(") {
		t.Fatalf("Run failed: %s", out)
	}
	if got := readBack(t, p); got != "hi world" {
		t.Fatalf("content = %q", got)
	}
}

func TestMultiEditRunRejectsMissingNewString(t *testing.T) {
	p := writeTemp(t, "hello")
	args := map[string]any{
		"path":  p,
		"edits": []any{map[string]any{"old_string": "hello"}}, // no new_string
	}
	out := multiEditTool{}.Run(nil, args)
	if !strings.Contains(out, "malformed edits") {
		t.Fatalf("expected malformed-edits, got: %s", out)
	}
	if got := readBack(t, p); got != "hello" {
		t.Fatalf("file mutated on malformed call: %q", got)
	}
}

func TestMultiEditFailedShape(t *testing.T) {
	tl := multiEditTool{}
	if !tl.Failed("(hunk 1 not found: x)") {
		t.Fatal("Failed should flag paren error")
	}
	if tl.Failed("edited /tmp/x: 2 hunks, -3 +5 bytes") {
		t.Fatal("Failed must not flag success line")
	}
}

func TestMultiEditPolicyFlags(t *testing.T) {
	tl := multiEditTool{}
	if tl.Safe() {
		t.Fatal("multi_edit must not be Safe (it mutates a file)")
	}
	if !tl.Mutates() {
		t.Fatal("multi_edit must report Mutates for the diff snapshot")
	}
	if tl.TargetKey(map[string]any{"path": "/a/b"}) != MultiEditName+"|/a/b" {
		t.Fatal("TargetKey should key on path")
	}
}
