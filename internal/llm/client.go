// Package llm is the OpenAI-compatible chat client every agent (repair,
// hint, guardrail, curator) talks through: it asks for a JSON response
// shaped by a schema, retries once if the reply isn't valid JSON matching
// it, extracts token usage, computes cost, and records one llm_calls row
// per underlying HTTP call — successful or not, since a failed parse still
// spent real tokens. Scripted (scripted.go) fulfills the same ChatClient
// interface for tests that run without an API key.
package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/profoundmentalretardation/problem-helper/internal/config"
	"github.com/profoundmentalretardation/problem-helper/internal/store"
)

// Message is one chat message.
type Message struct {
	Role    string
	Content string
}

// Request is one structured chat call.
type Request struct {
	RequestID       uuid.UUID
	Agent           string
	Model           string
	Temperature     float64
	ReasoningEffort string
	Attempt         int
	Messages        []Message
	SchemaName      string
	Schema          map[string]any
}

// Response is a validated structured reply plus its accounting.
type Response struct {
	JSON  json.RawMessage
	Usage Usage
	Cost  string
}

// ChatClient is fulfilled by both Client (real network calls) and Scripted
// (canned test replies), so agent code never knows which it has.
type ChatClient interface {
	Chat(ctx context.Context, req Request) (Response, error)
}

// CallRecorder persists one llm_calls row; *store.Store satisfies it.
type CallRecorder interface {
	InsertLLMCall(ctx context.Context, c store.LLMCall) error
}

// ErrInvalidResponse is returned when the model's reply is not valid JSON
// matching the requested schema, even after one retry.
var ErrInvalidResponse = errors.New("llm: model did not return valid JSON matching the schema")

// Client is the real OpenAI-compatible implementation.
type Client struct {
	baseURL string
	apiKey  string
	http    *http.Client
	rec     CallRecorder
	pricing map[string]config.PricingConfig
}

// New builds a Client against an OpenAI-compatible base URL (no trailing
// "/chat/completions" — that's appended per call).
func New(baseURL, apiKey string, rec CallRecorder, pricing map[string]config.PricingConfig) *Client {
	return &Client{
		baseURL: baseURL,
		apiKey:  apiKey,
		http:    &http.Client{Timeout: 60 * time.Second},
		rec:     rec,
		pricing: pricing,
	}
}

// Chat sends messages, requesting a reply shaped by req.Schema. If the
// reply isn't valid JSON matching the schema, it retries exactly once with
// the invalid reply and an explanation appended to the conversation. Every
// underlying HTTP call is recorded as its own llm_calls row regardless of
// validity — a rejected reply still spent real tokens. The returned Usage
// and Cost cover every call this Chat made, not just the last one.
func (c *Client) Chat(ctx context.Context, req Request) (Response, error) {
	messages := append([]Message(nil), req.Messages...)

	// The caller's cost caps are charged what this Chat call actually spent,
	// not what its last HTTP call spent: a rejected reply burned real tokens,
	// so returning only the retry's usage would let a model that keeps
	// missing the schema overshoot max_cost_per_retry / max_cost_per_loop by
	// a whole call. Usage is summed and cost recomputed from the total —
	// Cost is linear in tokens, so that equals the sum of the per-call costs.
	var total Usage

	var lastErr error
	for attempt := 0; attempt < 2; attempt++ {
		start := time.Now()
		content, usage, err := c.rawChat(ctx, req, messages)
		if err != nil {
			return Response{}, err
		}
		latency := time.Since(start)
		cost := Cost(usage, c.pricing[req.Model])

		total.InputTokens += usage.InputTokens
		total.CachedInputTokens += usage.CachedInputTokens
		total.OutputTokens += usage.OutputTokens

		if recErr := c.record(ctx, req, messages, content, usage, cost, latency); recErr != nil {
			return Response{}, recErr
		}

		if valErr := validateJSON(content, req.Schema); valErr != nil {
			lastErr = valErr
			messages = append(messages,
				Message{Role: "assistant", Content: content},
				Message{Role: "user", Content: fmt.Sprintf(
					"Your last reply was not valid JSON matching the schema: %s. Reply again with ONLY valid JSON matching the schema.",
					valErr)},
			)
			continue
		}

		return Response{
			JSON:  json.RawMessage(content),
			Usage: total,
			Cost:  Cost(total, c.pricing[req.Model]),
		}, nil
	}

	return Response{}, fmt.Errorf("%w: %v", ErrInvalidResponse, lastErr)
}

func (c *Client) record(ctx context.Context, req Request, messages []Message, content string, usage Usage, cost string, latency time.Duration) error {
	if c.rec == nil {
		return nil
	}
	row := callRow(req, content, usage, cost)
	row.LatencyMS = int(latency.Milliseconds())
	row.Prompt = renderPrompt(messages)
	if err := c.rec.InsertLLMCall(ctx, row); err != nil {
		return fmt.Errorf("llm: recording call: %w", err)
	}
	return nil
}

// callRow builds the llm_calls row shared by Client and Scripted.
func callRow(req Request, content string, usage Usage, cost string) store.LLMCall {
	return store.LLMCall{
		RequestID:         req.RequestID,
		Agent:             req.Agent,
		Model:             req.Model,
		InputTokens:       usage.InputTokens,
		CachedInputTokens: usage.CachedInputTokens,
		OutputTokens:      usage.OutputTokens,
		Cost:              cost,
		Attempt:           req.Attempt,
		Response:          content,
	}
}

// renderPrompt flattens the message history into a plain-text audit trail
// stored alongside the response — not re-parsed, just kept for humans.
func renderPrompt(messages []Message) string {
	parts := make([]string, len(messages))
	for i, m := range messages {
		parts[i] = fmt.Sprintf("[%s] %s", m.Role, m.Content)
	}
	return strings.Join(parts, "\n\n")
}

// validateJSON reports whether content parses as a JSON object containing
// every field schema declares "required". This is not full JSON Schema
// validation — just enough to catch a model that ignored the shape it was
// asked for.
func validateJSON(content string, schema map[string]any) error {
	var parsed map[string]any
	if err := json.Unmarshal([]byte(content), &parsed); err != nil {
		return fmt.Errorf("invalid JSON: %w", err)
	}
	required, _ := schema["required"].([]any)
	for _, r := range required {
		name, _ := r.(string)
		if _, ok := parsed[name]; !ok {
			return fmt.Errorf("missing required field %q", name)
		}
	}
	return nil
}

type wireMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type wireJSONSchema struct {
	Name   string         `json:"name"`
	Schema map[string]any `json:"schema"`
	Strict bool           `json:"strict"`
}

type wireResponseFormat struct {
	Type       string         `json:"type"`
	JSONSchema wireJSONSchema `json:"json_schema"`
}

type wireRequest struct {
	Model           string             `json:"model"`
	Messages        []wireMessage      `json:"messages"`
	Temperature     float64            `json:"temperature"`
	ReasoningEffort string             `json:"reasoning_effort,omitempty"`
	ResponseFormat  wireResponseFormat `json:"response_format"`
}

type wireUsage struct {
	PromptTokens        int `json:"prompt_tokens"`
	CompletionTokens    int `json:"completion_tokens"`
	PromptTokensDetails struct {
		CachedTokens int `json:"cached_tokens"`
	} `json:"prompt_tokens_details"`
}

type wireResponse struct {
	Choices []struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
	} `json:"choices"`
	Usage wireUsage `json:"usage"`
}

func (c *Client) rawChat(ctx context.Context, req Request, messages []Message) (string, Usage, error) {
	wireMessages := make([]wireMessage, len(messages))
	for i, m := range messages {
		wireMessages[i] = wireMessage(m)
	}
	body := wireRequest{
		Model:           req.Model,
		Messages:        wireMessages,
		Temperature:     req.Temperature,
		ReasoningEffort: req.ReasoningEffort,
		ResponseFormat: wireResponseFormat{
			Type: "json_schema",
			JSONSchema: wireJSONSchema{
				Name:   req.SchemaName,
				Schema: req.Schema,
				Strict: true,
			},
		},
	}

	buf, err := json.Marshal(body)
	if err != nil {
		return "", Usage{}, fmt.Errorf("llm: encoding request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/chat/completions", bytes.NewReader(buf))
	if err != nil {
		return "", Usage{}, fmt.Errorf("llm: building request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+c.apiKey)

	httpResp, err := c.http.Do(httpReq)
	if err != nil {
		return "", Usage{}, fmt.Errorf("llm: chat completion request: %w", err)
	}
	defer func() { _ = httpResp.Body.Close() }()

	data, err := io.ReadAll(httpResp.Body)
	if err != nil {
		return "", Usage{}, fmt.Errorf("llm: reading response: %w", err)
	}
	if httpResp.StatusCode != http.StatusOK {
		return "", Usage{}, fmt.Errorf("llm: chat completion request failed: %s: %s", httpResp.Status, data)
	}

	var wr wireResponse
	if err := json.Unmarshal(data, &wr); err != nil {
		return "", Usage{}, fmt.Errorf("llm: decoding response: %w", err)
	}
	if len(wr.Choices) == 0 {
		return "", Usage{}, fmt.Errorf("llm: response had no choices")
	}

	usage := Usage{
		InputTokens:       wr.Usage.PromptTokens,
		CachedInputTokens: wr.Usage.PromptTokensDetails.CachedTokens,
		OutputTokens:      wr.Usage.CompletionTokens,
	}
	return wr.Choices[0].Message.Content, usage, nil
}
