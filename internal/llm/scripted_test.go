package llm_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/profoundmentalretardation/problem-helper/internal/llm"
)

func TestScripted_ReplaysInOrder(t *testing.T) {
	recorder := &fakeRecorder{}
	s := llm.NewScripted(recorder, testPricing(),
		llm.ScriptedResponse{JSON: `{"verdict":"first"}`, Usage: llm.Usage{InputTokens: 10, OutputTokens: 2}},
		llm.ScriptedResponse{JSON: `{"verdict":"second"}`, Usage: llm.Usage{InputTokens: 20, OutputTokens: 4}},
	)

	req := llm.Request{RequestID: uuid.New(), Agent: "repair", Model: "gpt-test", Attempt: 1}

	resp1, err := s.Chat(context.Background(), req)
	if err != nil {
		t.Fatalf("Chat 1: %v", err)
	}
	var v1 map[string]string
	_ = json.Unmarshal(resp1.JSON, &v1)
	if v1["verdict"] != "first" {
		t.Errorf("first call verdict = %q, want %q", v1["verdict"], "first")
	}

	resp2, err := s.Chat(context.Background(), req)
	if err != nil {
		t.Fatalf("Chat 2: %v", err)
	}
	var v2 map[string]string
	_ = json.Unmarshal(resp2.JSON, &v2)
	if v2["verdict"] != "second" {
		t.Errorf("second call verdict = %q, want %q", v2["verdict"], "second")
	}

	if s.Remaining() != 0 {
		t.Errorf("Remaining() = %d, want 0 after consuming both scripted responses", s.Remaining())
	}
	if len(s.Calls()) != 2 {
		t.Errorf("Calls() recorded %d requests, want 2", len(s.Calls()))
	}
	if len(recorder.calls) != 2 {
		t.Errorf("llm_calls rows recorded = %d, want 2 (scripted calls are logged like real ones)", len(recorder.calls))
	}
}

func TestScripted_PanicsWhenExhausted(t *testing.T) {
	s := llm.NewScripted(nil, nil, llm.ScriptedResponse{JSON: `{"verdict":"only"}`})
	req := llm.Request{Agent: "hint", Model: "gpt-test"}

	if _, err := s.Chat(context.Background(), req); err != nil {
		t.Fatalf("first scripted Chat: %v", err)
	}

	defer func() {
		if r := recover(); r == nil {
			t.Fatal("Chat: want panic when consulted beyond scripted responses, got none")
		}
	}()
	_, _ = s.Chat(context.Background(), req)
}

func TestScripted_ReturnsScriptedError(t *testing.T) {
	wantErr := context.DeadlineExceeded
	s := llm.NewScripted(nil, nil, llm.ScriptedResponse{Err: wantErr})

	_, err := s.Chat(context.Background(), llm.Request{Agent: "repair", Model: "gpt-test"})
	if err != wantErr {
		t.Fatalf("err = %v, want %v", err, wantErr)
	}
}

// A scripted failure is a call that happened, so it writes an llm_calls row
// exactly like the real Client's error path — otherwise every agent test
// built on a scripted transport failure or cancellation silently exempts
// itself from the "every model call writes a row" invariant it exists to
// model. The row is written even when the caller's context is already dead,
// which is the usual way this path is reached.
func TestScripted_ErrorIsStillRecorded(t *testing.T) {
	recorder := &fakeRecorder{}
	usage := llm.Usage{InputTokens: 10, OutputTokens: 0}
	s := llm.NewScripted(recorder, testPricing(), llm.ScriptedResponse{Err: context.Canceled, Usage: usage})

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	resp, err := s.Chat(ctx, llm.Request{Agent: "repair", Model: "gpt-test", Attempt: 1})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if resp.Usage != usage {
		t.Errorf("Usage = %+v, want %+v — a failed call still spent what it spent", resp.Usage, usage)
	}
	if len(recorder.calls) != 1 {
		t.Fatalf("llm_calls rows recorded = %d, want 1", len(recorder.calls))
	}
	if !strings.HasPrefix(recorder.calls[0].Response, "error: ") {
		t.Errorf("recorded response = %q, want the error text in place of a response", recorder.calls[0].Response)
	}
}

// Scripted mirrors the real Client's detached recording on the success path
// too: a context that dies between the reply and the insert must not cost the
// row, or agent tests would model an invariant the service doesn't have.
func TestScripted_SuccessIsRecordedWhenContextDiesBeforeTheInsert(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	recorder := &cancelOnRecord{cancel: cancel}
	s := llm.NewScripted(recorder, testPricing(), llm.ScriptedResponse{
		JSON:  `{"verdict":"approve"}`,
		Usage: llm.Usage{InputTokens: 10, OutputTokens: 5},
	})

	if _, err := s.Chat(ctx, llm.Request{Agent: "repair", Model: "gpt-test", Attempt: 1}); err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if len(recorder.calls) != 1 {
		t.Fatalf("llm_calls rows recorded = %d, want 1", len(recorder.calls))
	}
	if recorder.sawCanceled {
		t.Error("record ran on the caller's canceled context; it must be detached from it")
	}
}
