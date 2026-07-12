package tools

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// makeTree lays a small file tree under a temp dir for glob/grep tests,
// including an ignored node_modules dir that must never surface in results.
func makeTree(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	files := map[string]string{
		"main.go":             "package main\nfunc main() { println(\"hi\") }\n",
		"util.go":             "package main\n// TODO: refactor\nvar x = 1\n",
		"README.md":           "# Title\nsome prose TODO here\n",
		"src/app.ts":          "export const x = 1 // TODO wire up\n",
		"src/deep/nested.ts":  "const y = 2\n",
		"node_modules/dep.js": "// TODO this must be ignored\nvar z = 3\n",
	}
	for rel, content := range files {
		p := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func TestGlobMatchesAndSkipsIgnored(t *testing.T) {
	root := makeTree(t)
	out := Glob(root, "**/*.go")
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 2 {
		t.Fatalf("expected 2 .go files, got %d:\n%s", len(lines), out)
	}
	for _, want := range []string{"main.go", "util.go"} {
		if !strings.Contains(out, want) {
			t.Errorf("glob missing %s:\n%s", want, out)
		}
	}
	if strings.Contains(out, "dep.js") || strings.Contains(out, "node_modules") {
		t.Errorf("glob leaked ignored dir:\n%s", out)
	}
}

func TestGlobSingleStarStopsAtSlash(t *testing.T) {
	root := makeTree(t)
	// *.ts at the root should match nothing (all .ts live under src/), proving
	// a single star doesn't cross a directory boundary.
	if out := Glob(root, "*.ts"); !strings.HasPrefix(out, "no files match") {
		t.Fatalf("*.ts should not cross a slash, got:\n%s", out)
	}
	// src/*.ts matches only the direct child, not src/deep/nested.ts.
	out := Glob(root, "src/*.ts")
	if !strings.Contains(out, "src/app.ts") || strings.Contains(out, "nested.ts") {
		t.Fatalf("src/*.ts should match only app.ts, got:\n%s", out)
	}
}

func TestGlobInvalidAndEmpty(t *testing.T) {
	root := makeTree(t)
	if out := Glob(root, ""); !strings.Contains(out, "empty pattern") {
		t.Fatalf("empty pattern not reported: %s", out)
	}
	if !globTool.Failed(globTool{}, "(empty pattern)") {
		t.Fatal("Failed should flag empty pattern")
	}
	if globTool.Failed(globTool{}, "main.go\nutil.go") {
		t.Fatal("Failed must not flag a real match list")
	}
	if globTool.Failed(globTool{}, "no files match \"x\" under .") {
		t.Fatal("Failed must not flag the no-match line")
	}
}

func TestGrepFindsMatchesWithLineNumbers(t *testing.T) {
	root := makeTree(t)
	out := Grep(root, "TODO", "")
	if strings.Contains(out, "dep.js") {
		t.Fatalf("grep leaked ignored dir:\n%s", out)
	}
	// util.go's TODO is on line 2.
	if !strings.Contains(out, "util.go:2:") {
		t.Fatalf("grep missing util.go:2 match:\n%s", out)
	}
	for _, want := range []string{"README.md", "app.ts"} {
		if !strings.Contains(out, want) {
			t.Errorf("grep missing TODO in %s:\n%s", want, out)
		}
	}
}

func TestGrepIncludeFilter(t *testing.T) {
	root := makeTree(t)
	out := Grep(root, "TODO", "*.go")
	if !strings.Contains(out, "util.go") {
		t.Fatalf("grep include *.go should find util.go:\n%s", out)
	}
	if strings.Contains(out, "README.md") || strings.Contains(out, "app.ts") {
		t.Fatalf("grep include *.go should exclude non-go files:\n%s", out)
	}
}

func TestGrepInvalidRegexpAndNoMatch(t *testing.T) {
	root := makeTree(t)
	if out := Grep(root, "(", ""); !strings.Contains(out, "invalid regexp") {
		t.Fatalf("bad regexp not reported: %s", out)
	}
	out := Grep(root, "zzz-not-present", "")
	if !strings.HasPrefix(out, "no matches") {
		t.Fatalf("no-match line wrong: %s", out)
	}
	if grepTool.Failed(grepTool{}, out) {
		t.Fatal("Failed must not flag the no-match line")
	}
	if !grepTool.Failed(grepTool{}, "(invalid regexp: x)") {
		t.Fatal("Failed should flag invalid regexp")
	}
}

func TestGrepSkipsBinary(t *testing.T) {
	root := t.TempDir()
	// A file with a NUL byte and the pattern: must be skipped as binary.
	if err := os.WriteFile(filepath.Join(root, "blob.bin"), []byte("TODO\x00TODO"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "real.txt"), []byte("TODO here\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out := Grep(root, "TODO", "")
	if strings.Contains(out, "blob.bin") {
		t.Fatalf("grep should skip binary file:\n%s", out)
	}
	if !strings.Contains(out, "real.txt") {
		t.Fatalf("grep should still find text match:\n%s", out)
	}
}
