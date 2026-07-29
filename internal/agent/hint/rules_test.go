package hint

import (
	"strings"
	"testing"
)

// fixture mirrors research/02_hint_loop.ipynb's FIXTURE: a sliding-window
// off-by-one where the loop stops one iteration early.
const (
	fixtureOriginal = `import sys
data = sys.stdin.read().split()
n, k = int(data[0]), int(data[1])
a = [int(x) for x in data[2:2 + n]]
window = sum(a[:k])
best = window
for i in range(1, n - k):
    window += a[i + k - 1] - a[i - 1]
    best = max(best, window)
print(best)
`
	fixtureGood = "Your loop covers every window but one. Which window never gets scored, and when would that change the answer?"
)

var fixtureFixed = strings.Replace(fixtureOriginal, "range(1, n - k)", "range(1, n - k + 1)", 1)

// TestLooksExplicit ports the "1. the rules, both directions" corpus from
// research/02_hint_loop.ipynb: four hints a student could apply
// mechanically, and two that make them think instead. A checker that
// rejects everything is as useless as one that rejects nothing, hence both
// directions.
func TestLooksExplicit(t *testing.T) {
	tests := []struct {
		label      string
		hint       string
		shouldFire bool
	}{
		{"quotes the repaired expression",
			"In the for statement, use range(1, n - k + 1) instead of what you have.", true},
		{"names a line number", "Look at line 7 of your solution.", true},
		{"prescribes the edit",
			"You should change the loop bound to cover one more window.", true},
		{"contains a code span", "Try `range(1, n - k + 1)` there.", true},
		{"a socratic hint passes", fixtureGood, false},
		{"concept talk passes",
			"Think about which elements your sliding window never reaches.", false},
	}

	for _, tt := range tests {
		t.Run(tt.label, func(t *testing.T) {
			reasons := looksExplicit(tt.hint, fixtureOriginal, fixtureFixed)
			fired := len(reasons) > 0
			if fired != tt.shouldFire {
				t.Errorf("looksExplicit(%q) fired = %v (reasons %v), want %v",
					tt.hint, fired, reasons, tt.shouldFire)
			}
		})
	}
}

func TestLooksExplicit_DoesNotFlagStudentsOwnCode(t *testing.T) {
	hint := "Reconsider what sys.stdin.read().split() gives you for the second line of input."
	if reasons := looksExplicit(hint, fixtureOriginal, fixtureFixed); len(reasons) > 0 {
		t.Errorf("looksExplicit flagged the student's own code as a leak: %v", reasons)
	}
}
