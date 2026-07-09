package protocol

import (
	"strings"
	"testing"
)

func TestUnifiedDiffIdentical(t *testing.T) {
	if d := unifiedDiff("f.txt", "a\nb\n", "a\nb\n"); d != "" {
		t.Fatalf("identical content must produce no diff, got %q", d)
	}
}

func TestUnifiedDiffSimpleReplace(t *testing.T) {
	d := unifiedDiff("f.txt", "one\ntwo\nthree\n", "one\nTWO\nthree\n")
	for _, want := range []string{"--- a/f.txt", "+++ b/f.txt", "-two", "+TWO", " one", " three"} {
		if !strings.Contains(d, want) {
			t.Fatalf("diff missing %q:\n%s", want, d)
		}
	}
}

func TestUnifiedDiffNewFile(t *testing.T) {
	d := unifiedDiff("new.txt", "", "hello\nworld\n")
	if !strings.Contains(d, "+hello") || !strings.Contains(d, "+world") {
		t.Fatalf("new-file diff should be all inserts:\n%s", d)
	}
	if strings.Contains(d, "\n-") {
		t.Fatalf("new-file diff must contain no deletions:\n%s", d)
	}
}

func TestUnifiedDiffSeparateHunks(t *testing.T) {
	var a, b strings.Builder
	for i := 0; i < 40; i++ {
		line := "line" + string(rune('a'+i%26))
		a.WriteString(line + "\n")
		if i == 2 {
			b.WriteString("CHANGED-EARLY\n")
		} else if i == 35 {
			b.WriteString("CHANGED-LATE\n")
		} else {
			b.WriteString(line + "\n")
		}
	}
	d := unifiedDiff("f.txt", a.String(), b.String())
	if got := strings.Count(d, "@@ -"); got != 2 {
		t.Fatalf("changes 30+ lines apart must produce 2 hunks, got %d:\n%s", got, d)
	}
}

func TestUnifiedDiffSkipsBinaryAndOversize(t *testing.T) {
	if d := unifiedDiff("f.bin", "a\x00b", "a\x00c"); d != "" {
		t.Fatalf("NUL-carrying content must be skipped, got %q", d)
	}
	big := strings.Repeat("x\n", maxDiffLines+1)
	if d := unifiedDiff("f.txt", big, big+"y\n"); d != "" {
		t.Fatalf("oversized content must be skipped, got %q", d)
	}
}

func TestUnifiedDiffTrailingNewlineNotPhantom(t *testing.T) {
	// A newline-terminated file must not diff against itself+split artifact.
	d := unifiedDiff("f.txt", "a\nb\n", "a\nb\nc\n")
	if strings.Contains(d, "-b") {
		t.Fatalf("append-only change must not delete the prior last line:\n%s", d)
	}
	if !strings.Contains(d, "+c") {
		t.Fatalf("appended line missing:\n%s", d)
	}
}
