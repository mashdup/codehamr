package tools

import (
	"context"
	"fmt"
	"io/fs"
	"path/filepath"
	"regexp"
	"strings"

	chmctx "github.com/codehamr/codehamr/internal/ctx"
)

// Grep searches file contents under root for pattern (a Go RE2 regexp),
// returning "relpath:line:text" per match, skipping ignoredDirs and binary
// files. An optional include glob (e.g. "*.go") filters which files are read.
// Errors and the no-match case come back in the string (bash convention).
func Grep(root, pattern, include string) string {
	if pattern == "" {
		return "(empty pattern)"
	}
	if root == "" {
		root = "."
	}
	re, err := regexp.Compile(pattern)
	if err != nil {
		return fmt.Sprintf("(invalid regexp: %v)", err)
	}
	var includeRe *regexp.Regexp
	if include != "" {
		includeRe, err = globToRegexp(include)
		if err != nil {
			return fmt.Sprintf("(invalid include glob: %v)", err)
		}
	}
	var lines []string
	truncated := false
	walkErr := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
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
		// The include glob matches the base name (so "*.go" works without a
		// leading "**/"), falling back to the full relative path so a
		// directory-qualified include ("src/**/*.ts") still works.
		if includeRe != nil && !includeRe.MatchString(filepath.Base(rel)) && !includeRe.MatchString(rel) {
			return nil
		}
		raw := readForSearch(path)
		if raw == nil || looksBinary(raw) {
			return nil
		}
		for n, line := range strings.Split(string(raw), "\n") {
			if re.MatchString(line) {
				// Trim to keep one runaway minified line from blowing the
				// budget on a single match; the line number still locates it.
				text := strings.TrimRight(line, "\r")
				if len(text) > 300 {
					text = text[:300] + "…"
				}
				lines = append(lines, fmt.Sprintf("%s:%d:%s", rel, n+1, text))
				if len(lines) >= maxGrepMatches {
					truncated = true
					return fs.SkipAll
				}
			}
		}
		return nil
	})
	if walkErr != nil {
		return fmt.Sprintf("(grep error: %v)", walkErr)
	}
	if len(lines) == 0 {
		return fmt.Sprintf("no matches for %q under %s", pattern, root)
	}
	out := strings.Join(lines, "\n")
	if truncated {
		out += fmt.Sprintf("\n(capped at %d matches - narrow the pattern or set include to see the rest)", maxGrepMatches)
	}
	return chmctx.Truncate(out)
}

// grepTool is the registry entry for grep: a side-effect-free content search
// (Safe, never needs approval) that mutates nothing.
type grepTool struct{}

func (grepTool) Name() string           { return GrepName }
func (grepTool) Safe() bool             { return true }
func (grepTool) Mutates() bool          { return false }
func (grepTool) Schema() map[string]any { return grepSchema() }

func (grepTool) Run(_ context.Context, args map[string]any) string {
	pattern, _ := args["pattern"].(string)
	root, _ := args["path"].(string)
	include, _ := args["include"].(string)
	return Grep(root, pattern, include)
}

func (grepTool) InlineStatus(args map[string]any) string {
	pattern, _ := args["pattern"].(string)
	return "▶ grep: " + firstLine(pattern)
}

func (grepTool) Failed(result string) bool {
	// Success is "relpath:line:text" lines or a plain "no matches" line; every
	// failure is wrapped in parens. A "relpath:..." match line never starts
	// with "(", so a prefix match is safe.
	t := strings.TrimSpace(result)
	return strings.HasPrefix(t, "(empty pattern)") ||
		strings.HasPrefix(t, "(invalid regexp:") ||
		strings.HasPrefix(t, "(invalid include glob:") ||
		strings.HasPrefix(t, "(grep error:")
}

func (grepTool) TargetKey(args map[string]any) string {
	pattern, _ := args["pattern"].(string)
	return GrepName + "|" + pattern
}

func grepSchema() map[string]any {
	return map[string]any{
		"type": "function",
		"function": map[string]any{
			"name":        GrepName,
			"description": "Search file contents for a regular expression (Go RE2 syntax), recursively. Prefer this over `grep -r`/`rg` in bash for finding code - skips .git, node_modules, vendor and binaries, and caps output. Returns matching lines as `path:line:text`.",
			"parameters": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"pattern": map[string]any{
						"type":        "string",
						"description": "Go RE2 regular expression matched against each line.",
					},
					"path": map[string]any{
						"type":        "string",
						"description": "Directory to search under. Defaults to the working directory.",
					},
					"include": map[string]any{
						"type":        "string",
						"description": "Optional glob limiting which files are searched, e.g. `*.go` or `src/**/*.ts`.",
					},
				},
				"required": []string{"pattern"},
			},
		},
	}
}
