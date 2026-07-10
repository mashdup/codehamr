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
// layout, or "" for an effectively empty directory. The wording deliberately
// overrides the embedded prompt's investigate-with-ls instruction: without
// the explicit "do not run ls", models dutifully list the directory anyway
// (observed live: an opening `ls -F` despite the tree being present).
func buildTreeSection(root string) string {
	tree, truncated := buildTree(root)
	if tree == "" {
		return ""
	}
	note := "The layout investigation is already done: this Project file tree is refreshed automatically " +
		"every turn (your own edits included), so do NOT run `ls`, `ls -R`, `find`, or `tree` just to see " +
		"the structure — go straight to `read_file`/`grep` on the paths below."
	if truncated {
		note += " It is truncated at " + itoa(treeMaxEntries) + " entries; list only what lies beyond it."
	} else {
		note += " Only directories marked \"(contents omitted)\" need listing if you truly need their contents."
	}
	return note + "\n" + tree
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
