package tools

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestWebFetchStripsHTML(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write([]byte(`<html><head><style>.x{color:red}</style><script>var a=1;alert(2)</script></head><body><h1>Hello &amp; welcome</h1><p>Some prose here.</p></body></html>`))
	}))
	defer srv.Close()

	out := WebFetch(context.Background(), srv.URL)
	if !strings.HasPrefix(out, "HTTP 200") {
		t.Fatalf("missing status header:\n%s", out)
	}
	if !strings.Contains(out, "Hello & welcome") || !strings.Contains(out, "Some prose here.") {
		t.Fatalf("prose not extracted:\n%s", out)
	}
	for _, leak := range []string{"alert(2)", "color:red", "<h1>", "<script"} {
		if strings.Contains(out, leak) {
			t.Fatalf("HTML/script/style leaked (%q):\n%s", leak, out)
		}
	}
}

func TestWebFetchPassesThroughJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"key": "value <not html>"}`))
	}))
	defer srv.Close()

	out := WebFetch(context.Background(), srv.URL)
	if !strings.Contains(out, `{"key": "value <not html>"}`) {
		t.Fatalf("JSON should pass through untouched:\n%s", out)
	}
}

func TestWebFetchReportsHTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		w.Write([]byte("nope"))
	}))
	defer srv.Close()

	out := WebFetch(context.Background(), srv.URL)
	if !strings.HasPrefix(out, "(HTTP 404)") {
		t.Fatalf("404 should be reported as failure:\n%s", out)
	}
	if !webFetchTool.Failed(webFetchTool{}, out) {
		t.Fatal("Failed should flag a 4xx result")
	}
	// Body still included so the model sees the error page.
	if !strings.Contains(out, "nope") {
		t.Fatalf("error body should be included:\n%s", out)
	}
}

func TestWebFetchRejectsNonHTTP(t *testing.T) {
	for _, url := range []string{"file:///etc/passwd", "ftp://x", ""} {
		out := WebFetch(context.Background(), url)
		if !strings.HasPrefix(out, "(") {
			t.Fatalf("scheme %q should be rejected, got: %s", url, out)
		}
	}
	if out := WebFetch(context.Background(), "file:///etc/passwd"); !strings.Contains(out, "only http") {
		t.Fatalf("file:// should name the http-only rule: %s", out)
	}
}

func TestWebFetchSuccessNotFailed(t *testing.T) {
	tl := webFetchTool{}
	if tl.Failed("HTTP 200 OK — http://x\n\nbody") {
		t.Fatal("2xx result must not be Failed")
	}
	if !tl.Failed("(fetch error: boom)") {
		t.Fatal("transport error should be Failed")
	}
}

func TestWebFetchPolicyFlags(t *testing.T) {
	tl := webFetchTool{}
	if tl.Safe() {
		t.Fatal("web_fetch must not be Safe (it hits the network)")
	}
	if tl.Mutates() {
		t.Fatal("web_fetch must not report Mutates")
	}
}
