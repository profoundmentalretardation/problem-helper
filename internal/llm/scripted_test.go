package llm_test

import (
	"context"
	"encoding/json"
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
