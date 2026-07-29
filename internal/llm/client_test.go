package llm_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"

	"github.com/profoundmentalretardation/problem-helper/internal/config"
	"github.com/profoundmentalretardation/problem-helper/internal/llm"
	"github.com/profoundmentalretardation/problem-helper/internal/store"
)

// fakeRecorder is an in-memory CallRecorder so client tests don't need a
// real Postgres — the client's own behavior (retry, usage extraction, cost)
// is independent of where the row ultimately lands.
type fakeRecorder struct {
	calls []store.LLMCall
	err   error
}

func (f *fakeRecorder) InsertLLMCall(_ context.Context, c store.LLMCall) error {
	if f.err != nil {
		return f.err
	}
	f.calls = append(f.calls, c)
	return nil
}

var testSchema = map[string]any{
	"type":                 "object",
	"properties":           map[string]any{"verdict": map[string]any{"type": "string"}},
	"required":             []any{"verdict"},
	"additionalProperties": false,
}

func chatResponseBody(content string, promptTokens, cachedTokens, completionTokens int) string {
	body := map[string]any{
		"choices": []map[string]any{
			{"message": map[string]any{"content": content}},
		},
		"usage": map[string]any{
			"prompt_tokens":     promptTokens,
			"completion_tokens": completionTokens,
			"prompt_tokens_details": map[string]any{
				"cached_tokens": cachedTokens,
			},
		},
	}
	data, err := json.Marshal(body)
	if err != nil {
		panic(err)
	}
	return string(data)
}

func newTestServer(t *testing.T, responses ...string) (*httptest.Server, *int) {
	t.Helper()
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-key" {
			t.Errorf("missing/wrong Authorization header: %q", r.Header.Get("Authorization"))
		}
		if calls >= len(responses) {
			t.Fatalf("unexpected extra HTTP call (got %d, scripted %d)", calls+1, len(responses))
		}
		body := responses[calls]
		calls++
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv, &calls
}

func testPricing() map[string]config.PricingConfig {
	return map[string]config.PricingConfig{
		"gpt-test": {Input: 3.00, CachedInput: 1.50, Output: 15.00},
	}
}

func TestChat_Success(t *testing.T) {
	srv, calls := newTestServer(t, chatResponseBody(`{"verdict":"approve"}`, 1000, 200, 50))
	recorder := &fakeRecorder{}
	client := llm.New(srv.URL, "test-key", recorder, testPricing())

	resp, err := client.Chat(context.Background(), llm.Request{
		RequestID:   uuid.New(),
		Agent:       "guardrail",
		Model:       "gpt-test",
		Temperature: 0.2,
		Attempt:     1,
		Messages:    []llm.Message{{Role: "user", Content: "hello"}},
		SchemaName:  "verdict",
		Schema:      testSchema,
	})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if *calls != 1 {
		t.Fatalf("HTTP calls = %d, want 1", *calls)
	}

	var parsed map[string]string
	if err := json.Unmarshal(resp.JSON, &parsed); err != nil {
		t.Fatalf("response JSON did not parse: %v", err)
	}
	if parsed["verdict"] != "approve" {
		t.Errorf("verdict = %q, want %q", parsed["verdict"], "approve")
	}

	if resp.Usage != (llm.Usage{InputTokens: 1000, CachedInputTokens: 200, OutputTokens: 50}) {
		t.Errorf("usage = %+v, want extracted from response", resp.Usage)
	}
	wantCost := llm.Cost(resp.Usage, testPricing()["gpt-test"])
	if resp.Cost != wantCost {
		t.Errorf("cost = %q, want %q", resp.Cost, wantCost)
	}

	if len(recorder.calls) != 1 {
		t.Fatalf("llm_calls rows recorded = %d, want 1", len(recorder.calls))
	}
	row := recorder.calls[0]
	if row.Agent != "guardrail" || row.Model != "gpt-test" || row.Attempt != 1 {
		t.Errorf("recorded row = %+v, want agent=guardrail model=gpt-test attempt=1", row)
	}
	if row.InputTokens != 1000 || row.CachedInputTokens != 200 || row.OutputTokens != 50 {
		t.Errorf("recorded token counts = %+v", row)
	}
	if row.Cost != wantCost {
		t.Errorf("recorded cost = %q, want %q", row.Cost, wantCost)
	}
}

func TestChat_RetriesOnceOnInvalidJSON(t *testing.T) {
	srv, calls := newTestServer(t,
		chatResponseBody(`not valid json`, 100, 0, 10),
		chatResponseBody(`{"verdict":"approve"}`, 120, 0, 12),
	)
	recorder := &fakeRecorder{}
	client := llm.New(srv.URL, "test-key", recorder, testPricing())

	resp, err := client.Chat(context.Background(), llm.Request{
		RequestID:  uuid.New(),
		Agent:      "hint",
		Model:      "gpt-test",
		Attempt:    1,
		Messages:   []llm.Message{{Role: "user", Content: "hello"}},
		SchemaName: "verdict",
		Schema:     testSchema,
	})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if *calls != 2 {
		t.Fatalf("HTTP calls = %d, want 2 (one retry)", *calls)
	}
	var parsed map[string]string
	if err := json.Unmarshal(resp.JSON, &parsed); err != nil || parsed["verdict"] != "approve" {
		t.Fatalf("expected the retried valid response, got %s (err=%v)", resp.JSON, err)
	}

	// Both calls cost real money — both must be recorded, not just the
	// successful one.
	if len(recorder.calls) != 2 {
		t.Fatalf("llm_calls rows recorded = %d, want 2 (both the invalid and the retry)", len(recorder.calls))
	}

	// ...and the caller's cost caps are charged for both. Returning only the
	// retry's usage would let a model that keeps missing the schema spend
	// twice what max_cost_per_retry / max_cost_per_loop allow.
	wantUsage := llm.Usage{InputTokens: 220, CachedInputTokens: 0, OutputTokens: 22}
	if resp.Usage != wantUsage {
		t.Errorf("Usage = %+v, want %+v (both calls summed)", resp.Usage, wantUsage)
	}
	wantCost := llm.Cost(wantUsage, testPricing()["gpt-test"])
	if resp.Cost != wantCost {
		t.Errorf("Cost = %s, want %s (both calls, not just the retry)", resp.Cost, wantCost)
	}
	if resp.Cost == llm.Cost(llm.Usage{InputTokens: 120, OutputTokens: 12}, testPricing()["gpt-test"]) {
		t.Errorf("Cost = %s, which is only the retry's cost — the rejected call was not charged", resp.Cost)
	}
}

func TestChat_MissingRequiredFieldTreatedAsInvalid(t *testing.T) {
	srv, calls := newTestServer(t,
		chatResponseBody(`{"unrelated":"field"}`, 100, 0, 10),
		chatResponseBody(`{"verdict":"reject"}`, 100, 0, 10),
	)
	recorder := &fakeRecorder{}
	client := llm.New(srv.URL, "test-key", recorder, testPricing())

	resp, err := client.Chat(context.Background(), llm.Request{
		RequestID:  uuid.New(),
		Agent:      "guardrail",
		Model:      "gpt-test",
		Messages:   []llm.Message{{Role: "user", Content: "hi"}},
		SchemaName: "verdict",
		Schema:     testSchema,
	})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if *calls != 2 {
		t.Fatalf("HTTP calls = %d, want 2", *calls)
	}
	var parsed map[string]string
	if err := json.Unmarshal(resp.JSON, &parsed); err != nil || parsed["verdict"] != "reject" {
		t.Fatalf("expected the second response, got %s", resp.JSON)
	}
}

func TestChat_FailsAfterRetryExhausted(t *testing.T) {
	srv, calls := newTestServer(t,
		chatResponseBody(`still not json`, 100, 0, 10),
		chatResponseBody(`also not json`, 100, 0, 10),
	)
	recorder := &fakeRecorder{}
	client := llm.New(srv.URL, "test-key", recorder, testPricing())

	_, err := client.Chat(context.Background(), llm.Request{
		RequestID:  uuid.New(),
		Agent:      "repair",
		Model:      "gpt-test",
		Messages:   []llm.Message{{Role: "user", Content: "hi"}},
		SchemaName: "verdict",
		Schema:     testSchema,
	})
	if err == nil {
		t.Fatal("Chat: want error after both attempts return invalid JSON, got nil")
	}
	if *calls != 2 {
		t.Fatalf("HTTP calls = %d, want 2 (initial + one retry, then give up)", *calls)
	}
	// Both failed calls still cost money and must be recorded.
	if len(recorder.calls) != 2 {
		t.Fatalf("llm_calls rows recorded = %d, want 2", len(recorder.calls))
	}
}

func TestChat_HTTPErrorNotRecorded(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":"boom"}`))
	}))
	t.Cleanup(srv.Close)
	recorder := &fakeRecorder{}
	client := llm.New(srv.URL, "test-key", recorder, testPricing())

	_, err := client.Chat(context.Background(), llm.Request{
		RequestID:  uuid.New(),
		Agent:      "repair",
		Model:      "gpt-test",
		Messages:   []llm.Message{{Role: "user", Content: "hi"}},
		SchemaName: "verdict",
		Schema:     testSchema,
	})
	if err == nil {
		t.Fatal("Chat: want error on HTTP 500, got nil")
	}
	if len(recorder.calls) != 0 {
		t.Fatalf("llm_calls rows recorded = %d, want 0 (no usage was ever returned)", len(recorder.calls))
	}
}

func TestChat_RecorderErrorPropagates(t *testing.T) {
	srv, _ := newTestServer(t, chatResponseBody(`{"verdict":"approve"}`, 10, 0, 5))
	recorder := &fakeRecorder{err: fmt.Errorf("db down")}
	client := llm.New(srv.URL, "test-key", recorder, testPricing())

	_, err := client.Chat(context.Background(), llm.Request{
		RequestID:  uuid.New(),
		Agent:      "repair",
		Model:      "gpt-test",
		Messages:   []llm.Message{{Role: "user", Content: "hi"}},
		SchemaName: "verdict",
		Schema:     testSchema,
	})
	if err == nil {
		t.Fatal("Chat: want error when the recorder fails to persist, got nil")
	}
}

func TestChat_SendsModelTemperatureAndSchema(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Fatalf("decoding request body: %v", err)
		}
		_, _ = w.Write([]byte(chatResponseBody(`{"verdict":"approve"}`, 1, 0, 1)))
	}))
	t.Cleanup(srv.Close)
	recorder := &fakeRecorder{}
	client := llm.New(srv.URL, "test-key", recorder, testPricing())

	_, err := client.Chat(context.Background(), llm.Request{
		RequestID:       uuid.New(),
		Agent:           "repair",
		Model:           "gpt-test",
		Temperature:     0.42,
		ReasoningEffort: "high",
		Messages:        []llm.Message{{Role: "system", Content: "sys"}, {Role: "user", Content: "usr"}},
		SchemaName:      "verdict",
		Schema:          testSchema,
	})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}

	if gotBody["model"] != "gpt-test" {
		t.Errorf("model = %v, want gpt-test", gotBody["model"])
	}
	if gotBody["temperature"] != 0.42 {
		t.Errorf("temperature = %v, want 0.42", gotBody["temperature"])
	}
	if gotBody["reasoning_effort"] != "high" {
		t.Errorf("reasoning_effort = %v, want high", gotBody["reasoning_effort"])
	}
	rf, ok := gotBody["response_format"].(map[string]any)
	if !ok {
		t.Fatalf("response_format missing or wrong type: %v", gotBody["response_format"])
	}
	if rf["type"] != "json_schema" {
		t.Errorf("response_format.type = %v, want json_schema", rf["type"])
	}
	js, ok := rf["json_schema"].(map[string]any)
	if !ok {
		t.Fatalf("response_format.json_schema missing: %v", rf)
	}
	if js["name"] != "verdict" {
		t.Errorf("json_schema.name = %v, want verdict", js["name"])
	}
}
