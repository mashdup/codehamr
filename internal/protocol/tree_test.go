package protocol

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func mkTree(t *testing.T, paths ...string) string {
	t.Helper()
	root := t.TempDir()
	for _, p := range paths {
		full := filepath.Join(root, filepath.FromSlash(p))
		if strings.HasSuffix(p, "/") {
			if err := os.MkdirAll(full, 0o755); err != nil {
				t.Fatal(err)
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func TestBuildTreeListsFilesAndDirs(t *testing.T) {
	root := mkTree(t, "src/main.go", "src/util/helper.go", "README.md")
	tree, truncated := buildTree(root)
	for _, want := range []string{"README.md", "src/", "main.go", "util/", "helper.go"} {
		if !strings.Contains(tree, want) {
			t.Fatalf("tree missing %q:\n%s", want, tree)
		}
	}
	if truncated {
		t.Fatal("small tree must not report truncation")
	}
}

func TestBuildTreeSkipsHeavyDirsButNamesThem(t *testing.T) {
	root := mkTree(t, "node_modules/react/index.js", ".git/HEAD", "app.ts")
	tree, _ := buildTree(root)
	if strings.Contains(tree, "index.js") || strings.Contains(tree, "HEAD") {
		t.Fatalf("skip-dir contents leaked into tree:\n%s", tree)
	}
	if !strings.Contains(tree, "node_modules/ (contents omitted)") {
		t.Fatalf("skipped dir should still be named:\n%s", tree)
	}
}

func TestBuildTreeKeepsHiddenFilesSkipsHiddenDirs(t *testing.T) {
	root := mkTree(t, ".gitignore", ".vscode/settings.json", ".github/workflows/ci.yml")
	tree, _ := buildTree(root)
	if !strings.Contains(tree, ".gitignore") {
		t.Fatalf("hidden files must stay:\n%s", tree)
	}
	if strings.Contains(tree, "settings.json") {
		t.Fatalf("hidden dir contents must be skipped:\n%s", tree)
	}
	if !strings.Contains(tree, "ci.yml") {
		t.Fatalf(".github is the exception and must be walked:\n%s", tree)
	}
}

func TestBuildTreeEntryCap(t *testing.T) {
	paths := make([]string, treeMaxEntries+50)
	for i := range paths {
		paths[i] = "f" + itoa(i) + ".txt"
	}
	root := mkTree(t, paths...)
	tree, truncated := buildTree(root)
	if !truncated {
		t.Fatal("expected truncation")
	}
	if n := strings.Count(tree, "\n"); n > treeMaxEntries {
		t.Fatalf("tree has %d lines, cap is %d", n, treeMaxEntries)
	}
}

func TestBuildTreeSectionEmptyDir(t *testing.T) {
	if s := buildTreeSection(t.TempDir()); s != "" {
		t.Fatalf("empty dir should produce no section, got %q", s)
	}
}

func TestBuildTreeCollapsesAssetRuns(t *testing.T) {
	paths := []string{"src/main.go", "src/util.go"}
	for i := 0; i < 6; i++ { // >= collapseRun asset siblings in one dir
		paths = append(paths, "assets/icon"+itoa(i)+".svg")
	}
	root := mkTree(t, paths...)
	tree, _ := buildTree(root)
	if !strings.Contains(tree, "6 *.svg files") {
		t.Fatalf("asset run should collapse to a count line:\n%s", tree)
	}
	if strings.Contains(tree, "icon0.svg") {
		t.Fatalf("collapsed asset names must not be listed individually:\n%s", tree)
	}
	// Source files are never collapsed — the model needs their names to read them.
	for _, want := range []string{"main.go", "util.go"} {
		if !strings.Contains(tree, want) {
			t.Fatalf("source file %q must stay listed:\n%s", want, tree)
		}
	}
}

func TestBuildTreeKeepsSmallAssetRuns(t *testing.T) {
	// Below collapseRun, listing the names is cheaper than losing them.
	root := mkTree(t, "logo.png", "hero.png")
	tree, _ := buildTree(root)
	for _, want := range []string{"logo.png", "hero.png"} {
		if !strings.Contains(tree, want) {
			t.Fatalf("small asset run must stay listed by name, missing %q:\n%s", want, tree)
		}
	}
}

func TestBuildTreeSingleSpaceIndent(t *testing.T) {
	root := mkTree(t, "src/util/helper.go")
	tree, _ := buildTree(root)
	// helper.go sits two levels deep: exactly two leading spaces, never four.
	if !strings.Contains(tree, "\n  helper.go\n") {
		t.Fatalf("expected single-space-per-level indent for helper.go:\n%q", tree)
	}
}
