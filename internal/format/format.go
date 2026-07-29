// Package format runs the optional external formatter step: the course's
// formatting rules applied to repaired code before it is submitted for
// verification and before it is diffed for loop 2. It is best-effort — a
// failing or slow formatter must never abort the repair loop.
package format

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// defaultTimeout bounds how long the external command may run.
const defaultTimeout = 5 * time.Second

// waitDelay bounds how long cmd.Wait may block on inherited pipes after the
// direct child has been killed. See the comment at its use site.
const waitDelay = time.Second

// Runner runs the formatter command configured in agents.yaml. The zero
// value is disabled and returns code untouched.
type Runner struct {
	Enabled bool
	Command string

	// Timeout overrides defaultTimeout; zero uses the default. Exposed for
	// tests, not part of agents.yaml.
	Timeout time.Duration
}

// Result is the outcome of Format.
type Result struct {
	Code string

	// Warning is non-empty when the formatter was enabled but its output
	// could not be used; Code is then the original, untouched input.
	Warning string
}

// Format runs the configured command with code on stdin and its stdout as
// the formatted result. Disabled runners, and any command failure or
// timeout, return the original code alongside a Warning describing what
// went wrong — Format never returns an error.
func (r Runner) Format(ctx context.Context, code string) Result {
	if !r.Enabled {
		return Result{Code: code}
	}

	fields := strings.Fields(r.Command)
	if len(fields) == 0 {
		return Result{Code: code, Warning: "format: enabled but command is empty"}
	}

	timeout := r.Timeout
	if timeout == 0 {
		timeout = defaultTimeout
	}
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(runCtx, fields[0], fields[1:]...)
	// CommandContext kills only the direct child, and because stdin/stdout/
	// stderr are not *os.File, os/exec wires them through pipes and cmd.Wait
	// blocks until the copying goroutines see EOF. A formatter that forks
	// (any `sh -c` wrapper) leaves a grandchild holding the pipe's write end,
	// so cmd.Run() blocks forever *past* the timeout — inside a step whose
	// contract is that it never aborts the repair loop, and with the worker's
	// heartbeat still ticking so the request is never even reclaimed.
	// WaitDelay bounds that wait and closes the pipes.
	cmd.WaitDelay = waitDelay
	cmd.Stdin = strings.NewReader(code)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		if errors.Is(runCtx.Err(), context.DeadlineExceeded) {
			return Result{Code: code, Warning: fmt.Sprintf("format: command timed out after %s", timeout)}
		}
		if errors.Is(ctx.Err(), context.Canceled) {
			return Result{Code: code, Warning: "format: canceled"}
		}
		return Result{Code: code, Warning: fmt.Sprintf("format: command failed: %v: %s", err, strings.TrimSpace(stderr.String()))}
	}

	// A formatter that exits 0 having printed nothing is broken, not
	// unanimous: taking its output would submit an empty file for
	// verification and diff the hint against nothing.
	if strings.TrimSpace(stdout.String()) == "" {
		return Result{Code: code, Warning: "format: command produced no output"}
	}

	return Result{Code: stdout.String()}
}
