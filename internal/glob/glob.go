// Package glob implements the path matcher used by loopctl's conflict scheduler.
package glob

import "strings"

// Match reports whether a repository-relative path matches pattern.
func Match(pattern, path string) bool {
	p := split(pattern)
	s := split(path)
	type key struct{ i, j int }
	memo := map[key]bool{}
	seen := map[key]bool{}
	var visit func(int, int) bool
	visit = func(i, j int) bool {
		k := key{i, j}
		if seen[k] {
			return memo[k]
		}
		seen[k] = true
		if i == len(p) {
			memo[k] = j == len(s)
			return memo[k]
		}
		if p[i] == "**" {
			memo[k] = visit(i+1, j) || (j < len(s) && visit(i, j+1))
			return memo[k]
		}
		memo[k] = j < len(s) && segmentMatch(p[i], s[j]) && visit(i+1, j+1)
		return memo[k]
	}
	return valid(pattern) && valid(path) && visit(0, 0)
}

// PatternsOverlap reports conservatively whether any pattern in a can match a
// path also matched by any pattern in b.
func PatternsOverlap(a, b []string) bool {
	for _, x := range a {
		for _, y := range b {
			if patternOverlap(x, y) {
				return true
			}
		}
	}
	return false
}

func patternOverlap(a, b string) bool {
	if !valid(a) || !valid(b) {
		return true // invalid/unknown input must fail safe
	}
	x, y := split(a), split(b)
	type state struct{ i, j int }
	queue := []state{{0, 0}}
	seen := map[state]bool{}
	for len(queue) > 0 {
		q := queue[0]
		queue = queue[1:]
		if seen[q] {
			continue
		}
		seen[q] = true
		if q.i == len(x) && q.j == len(y) {
			return true
		}
		if q.i < len(x) && x[q.i] == "**" {
			queue = append(queue, state{q.i + 1, q.j})
		}
		if q.j < len(y) && y[q.j] == "**" {
			queue = append(queue, state{q.i, q.j + 1})
		}
		if q.i == len(x) || q.j == len(y) {
			continue
		}
		sx, sy := x[q.i], y[q.j]
		switch {
		case sx == "**" && sy == "**":
			// Epsilon transitions above are sufficient; consuming in lockstep
			// would return to the same state.
		case sx == "**":
			queue = append(queue, state{q.i, q.j + 1})
		case sy == "**":
			queue = append(queue, state{q.i + 1, q.j})
		case segmentOverlap(sx, sy):
			queue = append(queue, state{q.i + 1, q.j + 1})
		}
	}
	return false
}

func valid(s string) bool {
	return s != "" && !strings.HasPrefix(s, "/") && !strings.HasPrefix(s, "./") &&
		!strings.HasSuffix(s, "/") && !strings.Contains(s, "//") && !strings.Contains(s, "\\")
}

func split(s string) []string { return strings.Split(s, "/") }

func segmentMatch(pattern, value string) bool {
	// Standard '*' wildcard within one non-empty path segment.
	pi, vi, star, mark := 0, 0, -1, 0
	for vi < len(value) {
		if pi < len(pattern) && pattern[pi] == value[vi] {
			pi++
			vi++
		} else if pi < len(pattern) && pattern[pi] == '*' {
			star, mark, pi = pi, vi, pi+1
		} else if star >= 0 {
			mark++
			vi, pi = mark, star+1
		} else {
			return false
		}
	}
	for pi < len(pattern) && pattern[pi] == '*' {
		pi++
	}
	return pi == len(pattern) && value != ""
}

func segmentOverlap(a, b string) bool {
	type state struct {
		i, j     int
		consumed bool
	}
	queue := []state{{}}
	seen := map[state]bool{}
	for len(queue) > 0 {
		q := queue[0]
		queue = queue[1:]
		if seen[q] {
			continue
		}
		seen[q] = true
		if q.i == len(a) && q.j == len(b) && q.consumed {
			return true
		}
		if q.i < len(a) && a[q.i] == '*' {
			queue = append(queue, state{q.i + 1, q.j, q.consumed})
		}
		if q.j < len(b) && b[q.j] == '*' {
			queue = append(queue, state{q.i, q.j + 1, q.consumed})
		}
		if q.i == len(a) || q.j == len(b) {
			continue
		}
		ca, cb := a[q.i], b[q.j]
		switch {
		case ca == '*' && cb == '*':
			queue = append(queue, state{q.i, q.j, true})
		case ca == '*':
			queue = append(queue, state{q.i, q.j + 1, true})
		case cb == '*':
			queue = append(queue, state{q.i + 1, q.j, true})
		case ca == cb:
			queue = append(queue, state{q.i + 1, q.j + 1, true})
		}
	}
	return false
}
