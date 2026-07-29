package format_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/profoundmentalretardation/problem-helper/internal/format"
)

func TestFormat_Disabled_CodeUntouched(t *testing.T) {
	r := format.Runner{Enabled: false, Command: "tr a-z A-Z"}

	got := r.Format(context.Background(), "hello")

	if got.Code != "hello" {
		t.Errorf("code = %q, want unchanged %q", got.Code, "hello")
	}
	if got.Warning != "" {
		t.Errorf("warning = %q, want empty when disabled", got.Warning)
	}
}

func TestFormat_Enabled_UsesCommandOutput(t *testing.T) {
	r := format.Runner{Enabled: true, Command: "tr a-z A-Z"}

	got := r.Format(context.Background(), "hello")

	if got.Code != "HELLO" {
		t.Errorf("code = %q, want %q", got.Code, "HELLO")
	}
	if got.Warning != "" {
		t.Errorf("warning = %q, want empty on success", got.Warning)
	}
}

func TestFormat_CommandFails_OriginalCodeAndWarning(t *testing.T) {
	r := format.Runner{Enabled: true, Command: "false"}

	got := r.Format(context.Background(), "original code")

	if got.Code != "original code" {
		t.Errorf("code = %q, want original code preserved on failure", got.Code)
	}
	if got.Warning == "" {
		t.Errorf("warning = %q, want non-empty on command failure", got.Warning)
	}
}

func TestFormat_CommandNotFound_OriginalCodeAndWarning(t *testing.T) {
	r := format.Runner{Enabled: true, Command: "no-such-formatter-binary-xyz"}

	got := r.Format(context.Background(), "original code")

	if got.Code != "original code" {
		t.Errorf("code = %q, want original code preserved when command is missing", got.Code)
	}
	if got.Warning == "" {
		t.Error("warning = \"\", want non-empty when the command cannot be found")
	}
}

func TestFormat_Timeout_OriginalCodeAndWarning(t *testing.T) {
	r := format.Runner{Enabled: true, Command: "sleep 5", Timeout: 20 * time.Millisecond}

	got := r.Format(context.Background(), "original code")

	if got.Code != "original code" {
		t.Errorf("code = %q, want original code preserved on timeout", got.Code)
	}
	if !strings.Contains(got.Warning, "timed out") {
		t.Errorf("warning = %q, want it to mention the timeout", got.Warning)
	}
}

func TestFormat_EnabledEmptyCommand_OriginalCodeAndWarning(t *testing.T) {
	r := format.Runner{Enabled: true, Command: ""}

	got := r.Format(context.Background(), "original code")

	if got.Code != "original code" {
		t.Errorf("code = %q, want original code preserved", got.Code)
	}
	if got.Warning == "" {
		t.Error("warning = \"\", want non-empty when enabled with an empty command")
	}
}

// TestFormat_EmptyOutput_OriginalCodeAndWarning covers a formatter that exits
// 0 having printed nothing. Taking its output would submit an empty file for
// verification and diff the hint against nothing.
func TestFormat_EmptyOutput_OriginalCodeAndWarning(t *testing.T) {
	r := format.Runner{Enabled: true, Command: "true"}

	got := r.Format(context.Background(), "original code")

	if got.Code != "original code" {
		t.Errorf("code = %q, want original code preserved when the formatter prints nothing", got.Code)
	}
	if !strings.Contains(got.Warning, "no output") {
		t.Errorf("warning = %q, want it to mention the empty output", got.Warning)
	}
}

// TestFormat_CallerCanceled_ReportsCancellationNotTimeout distinguishes a
// shutting-down worker from a formatter that hung: both kill the command, but
// only one is the formatter's fault.
func TestFormat_CallerCanceled_ReportsCancellationNotTimeout(t *testing.T) {
	r := format.Runner{Enabled: true, Command: "sleep 5", Timeout: time.Minute}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	got := r.Format(ctx, "original code")

	if got.Code != "original code" {
		t.Errorf("code = %q, want original code preserved on cancellation", got.Code)
	}
	if !strings.Contains(got.Warning, "canceled") {
		t.Errorf("warning = %q, want it to mention cancellation", got.Warning)
	}
	if strings.Contains(got.Warning, "timed out") {
		t.Errorf("warning = %q, want cancellation reported distinctly from a timeout", got.Warning)
	}
}

// TestFormat_ForkingCommandCannotHangPastItsTimeout pins the WaitDelay.
// CommandContext kills only the direct child, and because stdin/stdout are
// wired through pipes cmd.Wait blocks until the copy goroutines see EOF — so
// a formatter that forks leaves a grandchild holding the write end and Format
// never returns, inside a step whose contract is that it never aborts the
// repair loop and with the worker's heartbeat still ticking so the request is
// never even reclaimed.
func TestFormat_ForkingCommandCannotHangPastItsTimeout(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not available")
	}
	// The script exits at once; the backgrounded sleep inherits — and keeps
	// open — the stdout pipe Format is reading from.
	script := filepath.Join(t.TempDir(), "forking-formatter")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nsleep 60 &\nexit 0\n"), 0o700); err != nil {
		t.Fatalf("writing helper script: %v", err)
	}

	r := format.Runner{Enabled: true, Command: script, Timeout: 200 * time.Millisecond}

	done := make(chan format.Result, 1)
	go func() { done <- r.Format(context.Background(), "x = 1\n") }()

	select {
	case got := <-done:
		// Either outcome is fine — what matters is that it returned, with the
		// student's code intact and a warning rather than an error.
		if got.Code != "x = 1\n" {
			t.Errorf("Code = %q, want the original input back", got.Code)
		}
		if got.Warning == "" {
			t.Error("Warning is empty, want the formatter failure reported")
		}
	case <-time.After(15 * time.Second):
		t.Fatal("Format never returned: a forking formatter hung past its timeout")
	}
}
