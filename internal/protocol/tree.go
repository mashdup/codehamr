package protocol

import (
	"os"
	"path/filepath"
	"strings"
)

// The tree snapshot spares the model its ritual opening `ls -R` (and the
// user the approval click for it). Bounded hard: entry cap, depth cap, and
// heavy-directory skips, because it rides on every turn where every token
// displaces conversation history. Indentation is a single space per level —
// two spaces made leading whitespace ~30% of the whole block for no signal.
const (
	treeMaxEntries = 300
	treeMaxDepth   = 6
	// collapseRun folds a run of same-extension ASSET siblings in one directory
	// into a single count line once it reaches this many. Asset names (icons,
	// fonts, media) almost never earn per-file tokens; source and config files
	// are always listed by name so the model can still go straight to read_file.
	collapseRun = 5
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

// treeCollapseExts are file extensions whose individual names rarely help the
// model — bulk assets that otherwise flood an icon/font/media directory with
// one line each. Collapsing only these keeps source and config filenames
// (.go/.ts/.json/.md/…) fully listed. Lowercase, leading dot.
var treeCollapseExts = map[string]bool{
	".png": true, ".jpg": true, ".jpeg": true, ".gif": true, ".svg": true,
	".ico": true, ".webp": true, ".bmp": true, ".tiff": true,
	".woff": true, ".woff2": true, ".ttf": true, ".otf": true, ".eot": true,
	".mp4": true, ".mov": true, ".webm": true, ".mp3": true, ".wav": true, ".ogg": true,
}

// buildTreeSection returns the layout block appended to the current user turn,
// or "" for an effectively empty directory. The wording deliberately overrides
// the embedded prompt's investigate-with-ls instruction: without the explicit
// "do not run ls", models dutifully list the directory anyway (observed live:
// an opening `ls -F` despite the tree being present).
func buildTreeSection(root string) string {
	tree, truncated := buildTree(root)
	if tree == "" {
		return ""
	}
	note := "Project file tree at session start — the layout is already mapped, so do NOT run `ls`, `find`, or " +
		"`tree` to see it; go straight to `read_file`/`grep` on these paths. Shown once: track any files you add from here."
	if truncated {
		note += " Truncated at " + itoa(treeMaxEntries) + " entries; list only what lies beyond."
	}
	return note + "\n" + tree
}

// buildTree walks root depth-first, one directory at a time, and returns an
// indented listing plus whether the entry cap cut it short. Hidden directories
// are skipped except .github (CI config is real project structure); hidden
// FILES (.gitignore, .env.example) stay — they're one line each and often
// load-bearing. Per-directory walking (vs a flat WalkDir) is what lets asset
// runs collapse: siblings are visited together, so their extensions can be
// tallied before any line is written.
func buildTree(root string) (string, bool) {
	var b strings.Builder
	count := 0
	truncated := false

	// walk lists dir's immediate children at indent `depth`. Returns false when
	// the entry cap tripped and the whole walk must stop.
	var walk func(dir string, depth int) bool
	walk = func(dir string, depth int) bool {
		entries, err := os.ReadDir(dir) // sorted by name
		if err != nil {
			return true // unreadable dir is skipped, never fatal
		}
		// Tally asset extensions across this directory's files so a run can be
		// collapsed to a single line; the first member emits the count, the rest
		// are suppressed.
		assetCount := map[string]int{}
		for _, e := range entries {
			if !e.IsDir() {
				if ext := fileExt(e.Name()); treeCollapseExts[ext] {
					assetCount[ext]++
				}
			}
		}
		shown := map[string]bool{}
		indent := strings.Repeat(" ", depth)
		for _, e := range entries {
			if count >= treeMaxEntries {
				truncated = true
				return false
			}
			name := e.Name()
			if e.IsDir() {
				if treeSkipDirs[name] || (strings.HasPrefix(name, ".") && name != ".github") {
					b.WriteString(indent + name + "/ (contents omitted)\n")
					count++
					continue
				}
				if depth >= treeMaxDepth {
					continue // past the depth cap: name and contents both omitted
				}
				b.WriteString(indent + name + "/\n")
				count++
				if !walk(filepath.Join(dir, name), depth+1) {
					return false
				}
				continue
			}
			if ext := fileExt(name); assetCount[ext] >= collapseRun {
				if shown[ext] {
					continue
				}
				shown[ext] = true
				b.WriteString(indent + itoa(assetCount[ext]) + " *" + ext + " files\n")
				count++
				continue
			}
			b.WriteString(indent + name + "\n")
			count++
		}
		return true
	}
	walk(root, 0)
	return b.String(), truncated
}

// fileExt returns the lowercased extension (with dot) or "" when there is none.
// A leading dot (".env") is a dotfile, not an extension, so it returns "".
func fileExt(name string) string {
	i := strings.LastIndexByte(name, '.')
	if i <= 0 {
		return ""
	}
	return strings.ToLower(name[i:])
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
