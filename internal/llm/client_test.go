package llm_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
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

	resp, err := client.Chat(context.Background(), llm.Request{
		RequestID:  uuid.New(),
		Agent:      "repair",
		Model:      "gpt-test",
		Messages:   []llm.Message{{Role: "user", Content: "hi"}},
		SchemaName: "verdict",
		Schema:     testSchema,
	})
	if !errors.Is(err, llm.ErrInvalidResponse) {
		t.Fatalf("Chat: err = %v, want ErrInvalidResponse after both attempts return invalid JSON", err)
	}
	if *calls != 2 {
		t.Fatalf("HTTP calls = %d, want 2 (initial + one retry, then give up)", *calls)
	}
	// Both failed calls still cost money and must be recorded.
	if len(recorder.calls) != 2 {
		t.Fatalf("llm_calls rows recorded = %d, want 2", len(recorder.calls))
	}
	// ...and the caller has to be able to charge its cost caps for them.
	// Returning a zero Response here let a model that keeps missing the
	// schema overshoot max_cost_per_retry and max_cost_per_loop by two whole
	// calls, while the llm_calls rows said otherwise.
	if resp.Usage.InputTokens != 200 || resp.Usage.OutputTokens != 20 {
		t.Errorf("usage on the error return = %+v, want both calls summed (200 in / 20 out)", resp.Usage)
	}
	if resp.Cost == "" || resp.Cost == "0.000000" {
		t.Errorf("cost on the error return = %q, want the two calls' spend", resp.Cost)
	}
}

// OpenAI's strict structured-output mode is not "validate harder": it
// rejects any schema that omits additionalProperties:false or leaves a
// property out of required. All three of this service's schemas do both, so
// sending strict:true would 400 every call against a real endpoint and put
// every request in status=failed — invisible to fixtures that never look at
// response_format.
func TestChat_DoesNotRequestStrictSchemaMode(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Fatalf("decoding request body: %v", err)
		}
		_, _ = w.Write([]byte(chatResponseBody(`{"verdict":"approve"}`, 1, 0, 1)))
	}))
	t.Cleanup(srv.Close)
	client := llm.New(srv.URL, "test-key", &fakeRecorder{}, testPricing())

	if _, err := client.Chat(context.Background(), llm.Request{
		RequestID:  uuid.New(),
		Agent:      "repair",
		Model:      "gpt-test",
		Messages:   []llm.Message{{Role: "user", Content: "hi"}},
		SchemaName: "verdict",
		Schema:     testSchema,
	}); err != nil {
		t.Fatalf("Chat: %v", err)
	}

	format, _ := gotBody["response_format"].(map[string]any)
	schema, _ := format["json_schema"].(map[string]any)
	if schema["strict"] != false {
		t.Errorf("response_format.json_schema.strict = %v, want false", schema["strict"])
	}
}

// A failed call is still a call. The prompt was burned on the provider's
// side, so skipping the llm_calls row left a repeatedly-erroring provider
// invisible to cost analytics — and to anyone asking "what did this request
// actually do?". Usage is genuinely unknown here, so the row carries zeroes
// and the error text in place of a response.
func TestChat_HTTPErrorIsRecorded(t *testing.T) {
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
	if len(recorder.calls) != 1 {
		t.Fatalf("llm_calls rows recorded = %d, want 1 (a failed call still happened)", len(recorder.calls))
	}
	if got := recorder.calls[0]; got.InputTokens != 0 || got.OutputTokens != 0 {
		t.Errorf("usage = %d/%d, want 0/0 — it is not known on this path", got.InputTokens, got.OutputTokens)
	}
	if !strings.Contains(recorder.calls[0].Response, "500") {
		t.Errorf("response = %q, want it to carry the failure", recorder.calls[0].Response)
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

// cancelOnRecord cancels the caller's context at the exact moment the row is
// being written, which is the window the detached recording context exists to
// survive: the provider has already answered and the tokens are already paid
// for, so a claim lost (or a shutdown, or a request deadline) between the
// response and the insert must not lose the llm_calls row.
type cancelOnRecord struct {
	cancel        context.CancelFunc
	calls         []store.LLMCall
	sawCanceled   bool
	sawNoDeadline bool
}

func (f *cancelOnRecord) InsertLLMCall(ctx context.Context, c store.LLMCall) error {
	f.cancel()
	if ctx.Err() != nil {
		f.sawCanceled = true
	}
	if _, ok := ctx.Deadline(); !ok {
		f.sawNoDeadline = true
	}
	f.calls = append(f.calls, c)
	return nil
}

// A successful call is recorded even if the caller's context dies before the
// insert lands — the "every model call writes an llm_calls row" invariant is
// about calls that happened, and this one did.
func TestChat_SuccessIsRecordedWhenContextDiesBeforeTheInsert(t *testing.T) {
	srv, _ := newTestServer(t, chatResponseBody(`{"verdict":"approve"}`, 100, 0, 10))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	recorder := &cancelOnRecord{cancel: cancel}
	client := llm.New(srv.URL, "test-key", recorder, testPricing())

	if _, err := client.Chat(ctx, llm.Request{
		RequestID: uuid.New(), Agent: "guardrail", Model: "gpt-test", Attempt: 1,
		Messages: []llm.Message{{Role: "user", Content: "hi"}}, Schema: testSchema,
	}); err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if len(recorder.calls) != 1 {
		t.Fatalf("llm_calls rows recorded = %d, want 1", len(recorder.calls))
	}
	if recorder.sawCanceled {
		t.Error("record ran on the caller's canceled context; it must be detached from it")
	}
	if recorder.sawNoDeadline {
		t.Error("detached record context has no deadline; it must be bounded by recordTimeout")
	}
}

// Same for the schema-invalid reply: it burned tokens too, and it is the call
// a retry-happy model produces most of.
func TestChat_InvalidReplyIsRecordedWhenContextDiesBeforeTheInsert(t *testing.T) {
	srv, _ := newTestServer(t,
		chatResponseBody(`{"nope":"missing verdict"}`, 100, 0, 10),
		chatResponseBody(`{"verdict":"approve"}`, 100, 0, 10),
	)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	recorder := &cancelOnRecord{cancel: cancel}
	client := llm.New(srv.URL, "test-key", recorder, testPricing())

	// The cancel fires during the first record, so the retry's HTTP call fails
	// on the dead context — but the first, paid call is still on the books.
	_, _ = client.Chat(ctx, llm.Request{
		RequestID: uuid.New(), Agent: "guardrail", Model: "gpt-test", Attempt: 1,
		Messages: []llm.Message{{Role: "user", Content: "hi"}}, Schema: testSchema,
	})
	if len(recorder.calls) == 0 {
		t.Fatal("llm_calls rows recorded = 0, want at least the schema-invalid call")
	}
	if recorder.sawCanceled {
		t.Error("record ran on the caller's canceled context; it must be detached from it")
	}
}
