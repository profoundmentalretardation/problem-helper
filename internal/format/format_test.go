package format_test

import (
	"context"
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
