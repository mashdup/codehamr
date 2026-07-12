package tools

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"

	chmctx "github.com/codehamr/codehamr/internal/ctx"
)

// WebFetchName is the wire name for the URL-fetch tool.
const WebFetchName = "web_fetch"

// webFetchTimeout bounds a fetch so a hung server can't tie up the turn; the
// per-call context still cancels it on Ctrl+C.
const webFetchTimeout = 30 * time.Second

// maxFetchBytes caps the response body read before HTML-stripping so a
// multi-megabyte page can't blow memory before ctx.Truncate ever runs.
const maxFetchBytes = 5 << 20

var (
	// scriptRe / styleRe drop <script>/<style> blocks whole (content included)
	// so their bodies don't survive tag-stripping as visible noise. Go's RE2
	// has no backreferences, so the two tags get their own patterns.
	scriptRe = regexp.MustCompile(`(?is)<script[^>]*>.*?</\s*script\s*>`)
	styleRe  = regexp.MustCompile(`(?is)<style[^>]*>.*?</\s*style\s*>`)
	// tagRe strips any remaining HTML tag.
	tagRe = regexp.MustCompile(`(?s)<[^>]+>`)
	// wsRe collapses the whitespace runs left where tags were removed.
	wsRe = regexp.MustCompile(`[ \t]*\n[ \t\n]*\n[ \t]*`)
)

// htmlEntities is the small set of named entities common enough in body text
// to be worth decoding; numeric entities are handled separately.
var htmlEntities = strings.NewReplacer(
	"&amp;", "&", "&lt;", "<", "&gt;", ">", "&quot;", `"`,
	"&#39;", "'", "&apos;", "'", "&nbsp;", " ",
)

// stripHTML turns an HTML document into rough plain text: drop script/style,
// remove tags, decode the common entities, collapse blank-line runs. Not a
// real renderer - just enough that the model reads prose instead of markup.
func stripHTML(body string) string {
	body = scriptRe.ReplaceAllString(body, " ")
	body = styleRe.ReplaceAllString(body, " ")
	body = tagRe.ReplaceAllString(body, "")
	body = htmlEntities.Replace(body)
	body = wsRe.ReplaceAllString(body, "\n\n")
	return strings.TrimSpace(body)
}

// looksHTML reports whether a Content-Type or body sniff says HTML, so JSON,
// plain text, and source files pass through untouched while pages get
// stripped.
func looksHTML(contentType, body string) bool {
	if strings.Contains(strings.ToLower(contentType), "html") {
		return true
	}
	head := body
	if len(head) > 512 {
		head = head[:512]
	}
	head = strings.ToLower(head)
	return strings.Contains(head, "<!doctype html") || strings.Contains(head, "<html")
}

// WebFetch GETs url and returns its body as text: HTML is stripped to prose,
// everything else passes through, all clamped to the tool-output budget.
// Network and HTTP errors come back in the string (bash convention). Only
// http/https are allowed - a file:// URL would turn this side-effect-free
// fetch into a local-file read that dodges read_file's guards.
func WebFetch(ctx context.Context, url string) string {
	url = strings.TrimSpace(url)
	if url == "" {
		return "(empty url)"
	}
	if !strings.HasPrefix(url, "http://") && !strings.HasPrefix(url, "https://") {
		return "(invalid url: only http:// and https:// are supported)"
	}
	ctxT, cancel := context.WithTimeout(ctx, webFetchTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctxT, http.MethodGet, url, nil)
	if err != nil {
		return fmt.Sprintf("(invalid url: %v)", err)
	}
	// A real UA: some hosts 403 the default Go client string.
	req.Header.Set("User-Agent", "Mozilla/5.0 (compatible; codehamr/1.0; +https://github.com/codehamr/codehamr)")
	req.Header.Set("Accept", "text/html,application/xhtml+xml,text/plain,application/json;q=0.9,*/*;q=0.8")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		if ctxT.Err() == context.DeadlineExceeded {
			return fmt.Sprintf("(fetch timeout after %s)", webFetchTimeout)
		}
		return fmt.Sprintf("(fetch error: %v)", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxFetchBytes))
	if err != nil {
		return fmt.Sprintf("(read error: %v)", err)
	}
	body := string(raw)
	if looksHTML(resp.Header.Get("Content-Type"), body) {
		body = stripHTML(body)
	} else {
		body = strings.TrimSpace(body)
	}
	if body == "" {
		body = "(empty body)"
	}
	// Non-2xx still returns the body (an API error page is useful) but names
	// the status so the model reacts to a 404/500 instead of trusting it.
	header := fmt.Sprintf("HTTP %s — %s\n\n", resp.Status, url)
	if resp.StatusCode >= 400 {
		return chmctx.Truncate(fmt.Sprintf("(HTTP %d) %s\n\n%s", resp.StatusCode, url, body))
	}
	return chmctx.Truncate(header + body)
}

// webFetchTool is the registry entry for web_fetch. It hits the network, so it
// is NOT Safe (approval-gated like bash); it mutates no tracked file.
type webFetchTool struct{}

func (webFetchTool) Name() string           { return WebFetchName }
func (webFetchTool) Safe() bool             { return false }
func (webFetchTool) Mutates() bool          { return false }
func (webFetchTool) Schema() map[string]any { return webFetchSchema() }

func (webFetchTool) Run(ctx context.Context, args map[string]any) string {
	url, _ := args["url"].(string)
	return WebFetch(ctx, url)
}

func (webFetchTool) InlineStatus(args map[string]any) string {
	url, _ := args["url"].(string)
	return "▶ web_fetch: " + firstLine(url)
}

func (webFetchTool) Failed(result string) bool {
	// Success starts with "HTTP 2xx — url"; every transport/arg failure and
	// every >=400 status is paren-wrapped at the front.
	return strings.HasPrefix(strings.TrimSpace(result), "(")
}

func (webFetchTool) TargetKey(args map[string]any) string {
	url, _ := args["url"].(string)
	return WebFetchName + "|" + strings.TrimSpace(url)
}

func webFetchSchema() map[string]any {
	return map[string]any{
		"type": "function",
		"function": map[string]any{
			"name":        WebFetchName,
			"description": "Fetch a URL over http/https and return its body as text (HTML is stripped to readable prose; JSON and plain text pass through). Use for reading docs, release notes, or an API response. Output is truncated to the tool budget.",
			"parameters": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"url": map[string]any{
						"type":        "string",
						"description": "The http:// or https:// URL to fetch.",
					},
				},
				"required": []string{"url"},
			},
		},
	}
}
