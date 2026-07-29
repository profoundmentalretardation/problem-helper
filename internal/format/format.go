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

const (
	// maxOutputBytes caps the formatted source the command may print. A
	// submission that reaches the judge is orders of magnitude smaller.
	maxOutputBytes = 8 << 20

	// maxStderrBytes caps the diagnostics, which are interpolated into a
	// Warning that is persisted verbatim on an events row.
	maxStderrBytes = 8 << 10
)

// limitedBuffer is a bytes.Buffer that stops accepting data past limit and
// records that it did. Writes past the limit are reported as accepted, so the
// child is not killed by an EPIPE mid-write and cmd.Run's error still describes
// what actually happened.
type limitedBuffer struct {
	buf        bytes.Buffer
	limit      int
	overflowed bool
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	n := len(p)
	if room := b.limit - b.buf.Len(); room > 0 {
		if n > room {
			b.overflowed = true
			p = p[:room]
		}
		b.buf.Write(p)
	} else if n > 0 {
		b.overflowed = true
	}
	return n, nil
}

func (b *limitedBuffer) String() string { return b.buf.String() }

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
	// Bounded, because the timeout and WaitDelay bound how long the command
	// runs but not how much it writes: a misconfigured `command` in agents.yaml
	// can emit gigabytes well inside the 5s window, per worker goroutine, inside
	// the one step whose contract is that it never aborts the repair loop.
	// Over-limit output is discarded rather than truncated — a formatter that
	// outruns the cap is not one whose partial output should be submitted to
	// the judge — which the "produced no output" branch below already handles.
	stdout := &limitedBuffer{limit: maxOutputBytes}
	stderr := &limitedBuffer{limit: maxStderrBytes}
	cmd.Stdout = stdout
	cmd.Stderr = stderr

	if err := cmd.Run(); err != nil {
		if errors.Is(runCtx.Err(), context.DeadlineExceeded) {
			return Result{Code: code, Warning: fmt.Sprintf("format: command timed out after %s", timeout)}
		}
		if errors.Is(ctx.Err(), context.Canceled) {
			return Result{Code: code, Warning: "format: canceled"}
		}
		return Result{Code: code, Warning: fmt.Sprintf("format: command failed: %v: %s", err, strings.TrimSpace(stderr.String()))}
	}

	if stdout.overflowed {
		return Result{Code: code, Warning: fmt.Sprintf("format: command produced more than %d bytes of output", maxOutputBytes)}
	}

	// A formatter that exits 0 having printed nothing is broken, not
	// unanimous: taking its output would submit an empty file for
	// verification and diff the hint against nothing.
	if strings.TrimSpace(stdout.String()) == "" {
		return Result{Code: code, Warning: "format: command produced no output"}
	}

	return Result{Code: stdout.String()}
}
