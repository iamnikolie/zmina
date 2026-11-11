package crew

import (
	"fmt"
	"strings"
)

// diffContext is how many unchanged lines a hunk shows around a change.
const diffContext = 3

// diffCells bounds the work of a diff: a crew file is hundreds of lines, and a pair that would need more cells than
// this is not diffed line by line.
const diffCells = 16 << 20

// UnifiedDiff is what turns a into b, in unified format with a few lines of context; empty when they are the same.
func UnifiedDiff(a, b []byte, nameA, nameB string) string {
	if string(a) == string(b) {
		return ""
	}

	la, lb := splitLines(a), splitLines(b)

	ops, ok := diffOps(la, lb)
	if !ok {
		return fmt.Sprintf("--- %s\n+++ %s\n(too different to show line by line: %d lines against %d)\n", nameA, nameB, len(la), len(lb))
	}

	var out strings.Builder

	fmt.Fprintf(&out, "--- %s\n+++ %s\n", nameA, nameB)

	for _, h := range hunks(ops) {
		fmt.Fprintf(&out, "@@ -%s +%s @@\n", hunkRange(h.aStart, h.aCount), hunkRange(h.bStart, h.bCount))

		for _, o := range h.ops {
			out.WriteString(string(o.kind) + o.text + "\n")
		}
	}

	return out.String()
}

func splitLines(raw []byte) []string {
	if len(raw) == 0 {
		return nil
	}

	return strings.Split(strings.TrimSuffix(string(raw), "\n"), "\n")
}

// hunkRange is a hunk range as unified diffs write it: start,count, with the count left out when it is one.
func hunkRange(start, count int) string {
	if count == 1 {
		return fmt.Sprint(start)
	}

	return fmt.Sprintf("%d,%d", start, count)
}

type diffOp struct {
	kind byte // ' ' kept, '-' only in a, '+' only in b
	text string
}

// diffOps is the shortest edit script from a to b (longest common subsequence), false when the pair is too big.
func diffOps(a, b []string) ([]diffOp, bool) {
	var head, tail []diffOp

	for len(a) > 0 && len(b) > 0 && a[0] == b[0] {
		head = append(head, diffOp{' ', a[0]})
		a, b = a[1:], b[1:]
	}

	var rest []diffOp

	for len(a) > 0 && len(b) > 0 && a[len(a)-1] == b[len(b)-1] {
		rest = append(rest, diffOp{' ', a[len(a)-1]})
		a, b = a[:len(a)-1], b[:len(b)-1]
	}

	for i := len(rest) - 1; i >= 0; i-- {
		tail = append(tail, rest[i])
	}

	if (len(a)+1)*(len(b)+1) > diffCells {
		return nil, false
	}

	// lcs[i][j] is the length of the longest common subsequence of a[i:] and b[j:].
	lcs := make([][]int32, len(a)+1)
	for i := range lcs {
		lcs[i] = make([]int32, len(b)+1)
	}

	for i := len(a) - 1; i >= 0; i-- {
		for j := len(b) - 1; j >= 0; j-- {
			if a[i] == b[j] {
				lcs[i][j] = lcs[i+1][j+1] + 1
			} else {
				lcs[i][j] = max(lcs[i+1][j], lcs[i][j+1])
			}
		}
	}

	mid := make([]diffOp, 0, len(a)+len(b))
	i, j := 0, 0

	for i < len(a) && j < len(b) {
		switch {
		case a[i] == b[j]:
			mid = append(mid, diffOp{' ', a[i]})
			i++
			j++
		case lcs[i+1][j] >= lcs[i][j+1]:
			mid = append(mid, diffOp{'-', a[i]})
			i++
		default:
			mid = append(mid, diffOp{'+', b[j]})
			j++
		}
	}

	for ; i < len(a); i++ {
		mid = append(mid, diffOp{'-', a[i]})
	}

	for ; j < len(b); j++ {
		mid = append(mid, diffOp{'+', b[j]})
	}

	out := append(head, mid...)

	return append(out, tail...), true
}

type hunk struct {
	aStart, aCount, bStart, bCount int
	ops                            []diffOp
}

// hunks groups the changes of an edit script with their context; changes closer than twice the context share one.
func hunks(ops []diffOp) []hunk {
	// aAt[i] and bAt[i] are the 1-based lines of op i in a and b.
	aAt, bAt := make([]int, len(ops)+1), make([]int, len(ops)+1)
	a, b := 1, 1

	for i, o := range ops {
		aAt[i], bAt[i] = a, b

		if o.kind != '+' {
			a++
		}

		if o.kind != '-' {
			b++
		}
	}

	aAt[len(ops)], bAt[len(ops)] = a, b

	var out []hunk

	for i := 0; i < len(ops); {
		if ops[i].kind == ' ' {
			i++

			continue
		}

		from := max(i-diffContext, 0)
		to := i

		// Extend over the changes that follow while their context runs together.
		for j := i; j < len(ops); j++ {
			if ops[j].kind != ' ' {
				to = j
			} else if j-to > 2*diffContext {
				break
			}
		}

		end := min(to+diffContext+1, len(ops))
		h := hunk{ops: ops[from:end], aStart: aAt[from], bStart: bAt[from]}

		for _, o := range h.ops {
			if o.kind != '+' {
				h.aCount++
			}

			if o.kind != '-' {
				h.bCount++
			}
		}

		if h.aCount == 0 {
			h.aStart--
		}

		if h.bCount == 0 {
			h.bStart--
		}

		out = append(out, h)
		i = end
	}

	return out
}
