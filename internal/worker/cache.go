// Package worker orchestrates the request pipeline: queue claim, step
// resume, and the pieces that sit between the agent loops and the store.
//
// cache.go is the hint cache lookup (Task 11 scope): a pure read over the
// hints table, keyed by (problem_id, post-shield code hash). The pipeline
// step that decides whether to use it — and the "zero LLM calls on a hit"
// end-to-end assertion — is wired in Task 13.
package worker

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"

	"github.com/profoundmentalretardation/problem-helper/internal/store"
)

// HintFinder is the narrow store dependency Lookup needs; *store.Store
// satisfies it.
type HintFinder interface {
	FindApprovedHint(ctx context.Context, problemID, codeHash string) (*store.Hint, error)
}

// HashCode returns the hint-cache key for a post-shield submission's code:
// the sha256 hex digest of the code after normalizing line endings and
// trimming leading/trailing whitespace, so incidental formatting
// differences (a trailing newline, CRLF vs LF) don't fragment the cache.
func HashCode(code string) string {
	normalized := strings.ReplaceAll(code, "\r\n", "\n")
	normalized = strings.TrimSpace(normalized)
	sum := sha256.Sum256([]byte(normalized))
	return hex.EncodeToString(sum[:])
}

// Lookup returns the approved hint cached for this problem + post-shield
// code hash, and whether one was found. The cache is deliberately
// cross-user: an approved hint only ever carries diff + working code, never
// user-specific data, so it is safe to reuse for another student who hits
// the same defect.
//
// A store error is returned, not folded into "miss": the two are not
// interchangeable outcomes. A miss means run both model loops and submit to
// the judge as the system user; a transient DB fault reported as a miss
// spends all of that on a request whose hint was already on file.
func Lookup(ctx context.Context, hints HintFinder, problemID, codeHash string) (*store.Hint, bool, error) {
	hint, err := hints.FindApprovedHint(ctx, problemID, codeHash)
	if err != nil {
		return nil, false, fmt.Errorf("worker: looking up cached hint: %w", err)
	}
	if hint == nil {
		return nil, false, nil
	}
	return hint, true, nil
}
