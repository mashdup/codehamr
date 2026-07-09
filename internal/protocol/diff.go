package protocol

import (
	"fmt"
	"strings"
)

// Diff limits: past these the diff is skipped (the harness shows the tool
// result text instead). A 2000×2000 LCS table is ~16MB transient, the ceiling
// worth paying per edit; 1MB of content is no longer a human-reviewable diff.
const (
	maxDiffLines = 2000
	maxDiffBytes = 1 << 20
	diffContext  = 3
)

// unifiedDiff renders a classic unified diff between two file states, or ""
// when a diff isn't worth emitting (identical, binary-ish, or oversized).
// Plain LCS on lines: small, dependency-free, and tool-call-sized edits are
// exactly its sweet spot.
func unifiedDiff(path, before, after string) string {
	if before == after ||
		len(before) > maxDiffBytes || len(after) > maxDiffBytes ||
		strings.ContainsRune(before, 0) || strings.ContainsRune(after, 0) {
		return ""
	}
	a, b := splitLines(before), splitLines(after)
	if len(a) > maxDiffLines || len(b) > maxDiffLines {
		return ""
	}

	ops := diffOps(a, b)
	hunks := groupHunks(ops, len(a), len(b))
	if len(hunks) == 0 {
		return ""
	}

	var sb strings.Builder
	fmt.Fprintf(&sb, "--- a/%s\n+++ b/%s\n", path, path)
	for _, h := range hunks {
		fmt.Fprintf(&sb, "@@ -%d,%d +%d,%d @@\n", h.aStart+1, h.aLen, h.bStart+1, h.bLen)
		for _, op := range h.ops {
			switch op.kind {
			case opEqual:
				sb.WriteString(" " + a[op.aIdx] + "\n")
			case opDelete:
				sb.WriteString("-" + a[op.aIdx] + "\n")
			case opInsert:
				sb.WriteString("+" + b[op.bIdx] + "\n")
			}
		}
	}
	return sb.String()
}

// splitLines splits without swallowing a trailing newline distinction: the
// final empty element of a newline-terminated file is dropped so it doesn't
// render as a phantom changed line.
func splitLines(s string) []string {
	lines := strings.Split(s, "\n")
	if n := len(lines); n > 0 && lines[n-1] == "" {
		lines = lines[:n-1]
	}
	return lines
}

type opKind int

const (
	opEqual opKind = iota
	opDelete
	opInsert
)

type diffOp struct {
	kind       opKind
	aIdx, bIdx int
}

// diffOps computes the edit script via an LCS length table walked backwards.
func diffOps(a, b []string) []diffOp {
	n, m := len(a), len(b)
	// lcs[i][j] = LCS length of a[i:], b[j:], flattened.
	w := m + 1
	lcs := make([]int32, (n+1)*w)
	for i := n - 1; i >= 0; i-- {
		for j := m - 1; j >= 0; j-- {
			if a[i] == b[j] {
				lcs[i*w+j] = lcs[(i+1)*w+j+1] + 1
			} else {
				lcs[i*w+j] = max32(lcs[(i+1)*w+j], lcs[i*w+j+1])
			}
		}
	}
	ops := make([]diffOp, 0, n+m)
	i, j := 0, 0
	for i < n && j < m {
		switch {
		case a[i] == b[j]:
			ops = append(ops, diffOp{opEqual, i, j})
			i++
			j++
		case lcs[(i+1)*w+j] >= lcs[i*w+j+1]:
			ops = append(ops, diffOp{opDelete, i, -1})
			i++
		default:
			ops = append(ops, diffOp{opInsert, -1, j})
			j++
		}
	}
	for ; i < n; i++ {
		ops = append(ops, diffOp{opDelete, i, -1})
	}
	for ; j < m; j++ {
		ops = append(ops, diffOp{opInsert, -1, j})
	}
	return ops
}

func max32(x, y int32) int32 {
	if x > y {
		return x
	}
	return y
}

type hunk struct {
	aStart, aLen int
	bStart, bLen int
	ops          []diffOp
}

// groupHunks keeps changed ops plus diffContext equal lines around them,
// merging runs whose context overlaps, the standard unified-diff shape.
func groupHunks(ops []diffOp, _, _ int) []hunk {
	// Indices of non-equal ops.
	changed := make([]int, 0, len(ops))
	for idx, op := range ops {
		if op.kind != opEqual {
			changed = append(changed, idx)
		}
	}
	if len(changed) == 0 {
		return nil
	}

	var hunks []hunk
	start := maxInt(changed[0]-diffContext, 0)
	end := minInt(changed[0]+diffContext, len(ops)-1)
	for _, c := range changed[1:] {
		if c-diffContext <= end+1 {
			end = minInt(c+diffContext, len(ops)-1)
			continue
		}
		hunks = append(hunks, makeHunk(ops[start:end+1]))
		start = maxInt(c-diffContext, 0)
		end = minInt(c+diffContext, len(ops)-1)
	}
	hunks = append(hunks, makeHunk(ops[start:end+1]))
	return hunks
}

func makeHunk(ops []diffOp) hunk {
	h := hunk{aStart: -1, bStart: -1, ops: ops}
	for _, op := range ops {
		if op.aIdx >= 0 {
			if h.aStart < 0 {
				h.aStart = op.aIdx
			}
			h.aLen++
		}
		if op.bIdx >= 0 {
			if h.bStart < 0 {
				h.bStart = op.bIdx
			}
			h.bLen++
		}
	}
	// Pure-insert/-delete hunks anchor at the position the change lands.
	if h.aStart < 0 {
		h.aStart = 0
	}
	if h.bStart < 0 {
		h.bStart = 0
	}
	return h
}

func maxInt(x, y int) int {
	if x > y {
		return x
	}
	return y
}

func minInt(x, y int) int {
	if x < y {
		return x
	}
	return y
}
