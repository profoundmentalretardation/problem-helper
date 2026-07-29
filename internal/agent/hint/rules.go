package hint

import (
	"fmt"
	"regexp"
	"strings"
)

// These are the leaks a model does not get a vote on: cheap and
// deterministic, so a hint that was never going to pass costs nothing.
// Ported from research/02_hint_loop.ipynb.
var (
	callExprPattern   = regexp.MustCompile(`[A-Za-z_][\w.]*\s*\((?:[^()]|\([^()]*\))*\)`)
	lineRefPattern    = regexp.MustCompile(`(?i)\b(line|lines)\s*#?\s*\d+`)
	prescribesPattern = regexp.MustCompile(`(?i)\b(replace|change|swap)\b[^.]{0,60}\b(with|to|for)\b`)
	codeSpanPattern   = regexp.MustCompile("```|`[^`]{6,}`")
)

// flatten collapses whitespace runs to a single space, so multi-line
// fragments compare equal regardless of how they wrapped.
func flatten(text string) string {
	return strings.Join(strings.Fields(text), " ")
}

// leakFragments returns lines from fixed (and the call expressions inside
// them) that do not appear in original — candidate leaks a hint must not
// quote. Anything the student already wrote is excluded: quoting their own
// code back at them reveals nothing.
func leakFragments(original, fixed string) []string {
	old := make(map[string]bool)
	for _, l := range strings.Split(original, "\n") {
		old[l] = true
	}
	flatOriginal := flatten(original)

	var out []string
	seen := make(map[string]bool)
	for _, line := range strings.Split(fixed, "\n") {
		if old[line] || strings.TrimSpace(line) == "" {
			continue
		}
		candidates := append([]string{line}, callExprPattern.FindAllString(line, -1)...)
		for _, candidate := range candidates {
			frag := flatten(candidate)
			if len(frag) >= 10 && !strings.Contains(flatOriginal, frag) && !seen[frag] {
				seen[frag] = true
				out = append(out, frag)
			}
		}
	}
	return out
}

// looksExplicit returns the deterministic rules that fired against hint,
// given the student's original (broken) code and the verified fixed code.
// An empty result means nothing provably leaked, not that the hint is good
// — that judgement is the guardrail's.
func looksExplicit(hint, original, fixed string) []string {
	// An empty hint leaks nothing, so every other rule below passes it and a
	// guardrail asked "does this give the answer away?" plausibly approves
	// it — after which it is stored, cached under the submission's code hash
	// (poisoning the cache for every later student with the same defect) and
	// delivered as status=done. There is no judgement call here, so it is
	// rejected deterministically, before the guardrail call, like every other
	// hopeless hint.
	if strings.TrimSpace(hint) == "" {
		return []string{"is empty"}
	}

	var reasons []string
	flatHint := flatten(hint)
	for _, frag := range leakFragments(original, fixed) {
		if strings.Contains(flatHint, frag) {
			reasons = append(reasons, fmt.Sprintf("quotes the repaired code: %q", frag))
		}
	}
	if lineRefPattern.MatchString(hint) {
		reasons = append(reasons, "points at a line number")
	}
	if codeSpanPattern.MatchString(hint) {
		reasons = append(reasons, "contains a code span")
	}
	if prescribesPattern.MatchString(hint) {
		reasons = append(reasons, "prescribes the edit")
	}
	return reasons
}
