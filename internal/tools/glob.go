package tools

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	chmctx "github.com/codehamr/codehamr/internal/ctx"
)

// GlobName / GrepName are the wire names for the two side-effect-free search
// tools. Kept beside the file-tool names so schema, router, and status share
// one source.
const (
	GlobName = "glob"
	GrepName = "grep"
)

// ignoredDirs are directory names never descended into by glob/grep: VCS
// metadata and dependency/build trees that would bury real source matches in
// thousands of vendored hits (the same dirs a coding agent almost never means
// to search). Matched by base name at any depth.
var ignoredDirs = map[string]bool{
	".git":         true,
	"node_modules": true,
	"vendor":       true,
	"dist":         true,
	"build":        true,
	".venv":        true,
	"venv":         true,
	"__pycache__":  true,
	".next":        true,
	"target":       true,
	".idea":        true,
	".cache":       true,
}

// maxGlobResults / maxGrepMatches bound each search's output so a broad
// pattern (`**` or `.`) can't flood the model's context; the tail note names
// the cap so the model narrows the pattern instead of assuming it saw
// everything.
const (
	maxGlobResults = 400
	maxGrepMatches = 200
)

// globToRegexp compiles a shell-style glob into an anchored regexp matched
// against a slash-separated relative path. `**` spans directories (`.*`), a
// single `*` stops at a separator (`[^/]*`), `?` matches one non-separator
// rune; every other metacharacter is escaped so a literal `.` in `*.go` can't
// act as a wildcard. Returns an error the tool surfaces to the model rather
// than panicking on a malformed pattern.
func globToRegexp(pattern string) (*regexp.Regexp, error) {
	var b strings.Builder
	b.WriteString("^")
	for i := 0; i < len(pattern); i++ {
		c := pattern[i]
		switch c {
		case '*':
			if i+1 < len(pattern) && pattern[i+1] == '*' {
				// `**/` spans zero or more directory segments (so `**/*.go`
				// also matches a root-level file); a bare `**` spans any run
				// of characters including separators.
				if i+2 < len(pattern) && pattern[i+2] == '/' {
					b.WriteString("(?:.*/)?")
					i += 2 // consume the second '*' and the '/'
				} else {
					b.WriteString(".*")
					i++ // consume the second '*'
				}
			} else {
				b.WriteString("[^/]*")
			}
		case '?':
			b.WriteString("[^/]")
		default:
			b.WriteString(regexp.QuoteMeta(string(c)))
		}
	}
	b.WriteString("$")
	return regexp.Compile(b.String())
}

// Glob walks root and returns every file whose slash-separated relative path
// matches pattern, one per line, skipping ignoredDirs. Errors and the
// no-match case come back in the string (bash convention). Directory entries
// are not returned - the model wants files to read, not folders.
func Glob(root, pattern string) string {
	if pattern == "" {
		return "(empty pattern)"
	}
	if root == "" {
		root = "."
	}
	re, err := globToRegexp(pattern)
	if err != nil {
		return fmt.Sprintf("(invalid pattern: %v)", err)
	}
	var matches []string
	truncated := false
	walkErr := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // unreadable entry: skip, don't abort the whole walk
		}
		if d.IsDir() {
			if path != root && ignoredDirs[d.Name()] {
				return fs.SkipDir
			}
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return nil
		}
		rel = filepath.ToSlash(rel)
		if re.MatchString(rel) {
			matches = append(matches, rel)
			if len(matches) >= maxGlobResults {
				truncated = true
				return fs.SkipAll
			}
		}
		return nil
	})
	if walkErr != nil {
		return fmt.Sprintf("(glob error: %v)", walkErr)
	}
	if len(matches) == 0 {
		return fmt.Sprintf("no files match %q under %s", pattern, root)
	}
	out := strings.Join(matches, "\n")
	if truncated {
		out += fmt.Sprintf("\n(capped at %d results - narrow the pattern to see the rest)", maxGlobResults)
	}
	return chmctx.Truncate(out)
}

// globTool is the registry entry for glob: a side-effect-free file-name search
// (Safe, never needs approval) that mutates nothing.
type globTool struct{}

func (globTool) Name() string           { return GlobName }
func (globTool) Safe() bool             { return true }
func (globTool) Mutates() bool          { return false }
func (globTool) Schema() map[string]any { return globSchema() }

func (globTool) Run(_ context.Context, args map[string]any) string {
	pattern, _ := args["pattern"].(string)
	root, _ := args["path"].(string)
	return Glob(root, pattern)
}

func (globTool) InlineStatus(args map[string]any) string {
	pattern, _ := args["pattern"].(string)
	return "▶ glob: " + pattern
}

func (globTool) Failed(result string) bool {
	// Success is a newline list of paths or a plain "no files match" line;
	// every failure is wrapped in parens. A matched path can't start with "("
	// (it's a relative filename), so a prefix match is safe here.
	t := strings.TrimSpace(result)
	return strings.HasPrefix(t, "(empty pattern)") ||
		strings.HasPrefix(t, "(invalid pattern:") ||
		strings.HasPrefix(t, "(glob error:")
}

func (globTool) TargetKey(args map[string]any) string {
	pattern, _ := args["pattern"].(string)
	return GlobName + "|" + pattern
}

func globSchema() map[string]any {
	return map[string]any{
		"type": "function",
		"function": map[string]any{
			"name":        GlobName,
			"description": "Find files by name with a shell glob, recursively. Prefer this over `ls -R`/`find` in bash for locating files - skips .git, node_modules, vendor and other noise, and caps output. Use `**` to span directories: `**/*.go`, `src/**/*.ts`, `Dockerfile`.",
			"parameters": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"pattern": map[string]any{
						"type":        "string",
						"description": "Glob matched against each file's path relative to the search root. `**` spans directories, `*` stops at a slash, `?` is one character.",
					},
					"path": map[string]any{
						"type":        "string",
						"description": "Directory to search under. Defaults to the working directory.",
					},
				},
				"required": []string{"pattern"},
			},
		},
	}
}

// looksBinary reports whether the first chunk of b contains a NUL byte, the
// cheap heuristic grep uses to skip binaries so a matched byte-run inside an
// object file or image can't dump garbage into the model's context.
func looksBinary(b []byte) bool {
	n := len(b)
	if n > 8000 {
		n = 8000
	}
	for i := 0; i < n; i++ {
		if b[i] == 0 {
			return true
		}
	}
	return false
}

// readForSearch reads a file for grep, refusing non-regular files (a FIFO
// would block the walk forever) and returning nil for anything unreadable so
// the walk skips it silently rather than aborting.
func readForSearch(path string) []byte {
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() {
		return nil
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	return raw
}
