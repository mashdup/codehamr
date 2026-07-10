package protocol

import (
	"io/fs"
	"path/filepath"
	"strings"
)

// The tree snapshot spares the model its ritual opening `ls -R` (and the
// user the approval click for it). Bounded hard: entry cap, depth cap, and
// heavy-directory skips, because it rides inside the system prompt where
// every token displaces conversation history.
const (
	treeMaxEntries = 300
	treeMaxDepth   = 6
)

// treeSkipDirs are directories whose contents never earn their tokens:
// dependency stores, build output, VCS internals. The directory name itself
// still appears (with a marker), so the model knows it exists.
var treeSkipDirs = map[string]bool{
	".git":         true,
	".codehamr":    true,
	"node_modules": true,
	"vendor":       true,
	"__pycache__":  true,
	".venv":        true,
	"venv":         true,
	".next":        true,
	"target":       true,
	"dist":         true,
	"out":          true,
	"release":      true,
	".idea":        true,
}

// buildTreeSection returns the system-prompt block describing the project
// layout, or "" for an effectively empty directory.
func buildTreeSection(root string) string {
	tree, truncated := buildTree(root)
	if tree == "" {
		return ""
	}
	note := ""
	if truncated {
		note = " (truncated at " + itoa(treeMaxEntries) + " entries; ls for the rest)"
	}
	return "Project file tree" + note + ", refreshed each turn — trust it over re-running ls:\n" + tree
}

// buildTree walks root breadth-limited and returns an indented listing plus
// whether the entry cap cut it short. Hidden directories are skipped except
// .github (CI config is real project structure); hidden FILES (.gitignore,
// .env.example) stay — they're one line each and often load-bearing.
func buildTree(root string) (string, bool) {
	var b strings.Builder
	count := 0
	truncated := false
	_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || path == root {
			return nil // unreadable entries are skipped, never fatal
		}
		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			return nil
		}
		rel = filepath.ToSlash(rel)
		depth := strings.Count(rel, "/")
		name := d.Name()
		indent := strings.Repeat("  ", depth)
		if d.IsDir() {
			if treeSkipDirs[name] || (strings.HasPrefix(name, ".") && name != ".github") {
				if count < treeMaxEntries {
					b.WriteString(indent + name + "/ (contents omitted)\n")
					count++
				}
				return filepath.SkipDir
			}
			if depth >= treeMaxDepth {
				return filepath.SkipDir
			}
		}
		if count >= treeMaxEntries {
			truncated = true
			return fs.SkipAll
		}
		if d.IsDir() {
			b.WriteString(indent + name + "/\n")
		} else {
			b.WriteString(indent + name + "\n")
		}
		count++
		return nil
	})
	return b.String(), truncated
}

// itoa avoids strconv for one call site.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var digits []byte
	for n > 0 {
		digits = append([]byte{byte('0' + n%10)}, digits...)
		n /= 10
	}
	return string(digits)
}
