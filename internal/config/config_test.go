package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func fullEnv() map[string]string {
	return map[string]string{
		"DATABASE_URL":           "postgres://localhost/test",
		"LLM_BASE_URL":           "https://llm.example.com",
		"LLM_API_KEY":            "sk-test",
		"PLATFORM":               "ejudge",
		"API_TOKEN":              "api-token",
		"ADMIN_TOKEN":            "admin-token",
		"EJUDGE_URL":             "https://ejudge.example.com",
		"EJUDGE_SYSTEM_LOGIN":    "system",
		"EJUDGE_SYSTEM_PASSWORD": "secret",
	}
}

func lookupFrom(env map[string]string) func(string) (string, bool) {
	return func(key string) (string, bool) {
		v, ok := env[key]
		return v, ok
	}
}

func TestLoadEnv_AllPresent(t *testing.T) {
	env, err := LoadEnv(lookupFrom(fullEnv()))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if env.DatabaseURL != "postgres://localhost/test" {
		t.Errorf("DatabaseURL = %q", env.DatabaseURL)
	}
	if env.Platform != "ejudge" {
		t.Errorf("Platform = %q", env.Platform)
	}
}

func TestLoadEnv_MissingEachRequiredVar(t *testing.T) {
	for _, name := range requiredEnvVars {
		t.Run(name, func(t *testing.T) {
			env := fullEnv()
			delete(env, name)
			_, err := LoadEnv(lookupFrom(env))
			if err == nil {
				t.Fatalf("expected error for missing %s, got nil", name)
			}
			var missingErr *MissingEnvError
			if !errors.As(err, &missingErr) {
				t.Fatalf("expected *MissingEnvError, got %T: %v", err, err)
			}
			if missingErr.Name != name {
				t.Errorf("MissingEnvError.Name = %q, want %q", missingErr.Name, name)
			}
		})
	}
}

func TestLoadEnv_EmptyValueTreatedAsMissing(t *testing.T) {
	env := fullEnv()
	env["DATABASE_URL"] = ""
	_, err := LoadEnv(lookupFrom(env))
	if err == nil {
		t.Fatal("expected error for empty required var")
	}
}

const validAgentsYAML = `
defaults:
  n_submissions: 25
  daily_requests_per_user: 20
repair:
  model: "repair-model"
  temperature: 0.2
  reasoning_effort: ""
  max_retries: 3
  max_cost_per_retry: 0.05
  max_cost_per_loop: 0.25
  top_n_mistakes: 5
  n_tests_shown: 10
hint:
  model: "hint-model"
  temperature: 0.7
  max_retries: 3
  max_cost_per_retry: 0.05
  max_cost_per_loop: 0.20
guardrail:
  model: "guardrail-model"
  max_retries: 1
  max_cost_per_retry: 0.05
  max_cost_per_loop: 0.10
curator:
  model: "curator-model"
  max_retries: 2
  max_cost_per_retry: 0.05
  max_cost_per_loop: 0.30
pricing:
  repair-model: {input: 1.0, cached_input: 0.5, output: 2.0}
  hint-model: {input: 1.0, cached_input: 0.5, output: 2.0}
  guardrail-model: {input: 3.0, cached_input: 1.5, output: 6.0}
  curator-model: {input: 1.0, cached_input: 0.5, output: 2.0}
formatter:
  enabled: false
  command: ""
`

func TestParseAgents_Valid(t *testing.T) {
	cfg, err := ParseAgents([]byte(validAgentsYAML))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Repair.Model != "repair-model" {
		t.Errorf("Repair.Model = %q", cfg.Repair.Model)
	}
	if cfg.Defaults.NSubmissions != 25 {
		t.Errorf("Defaults.NSubmissions = %d", cfg.Defaults.NSubmissions)
	}
	if cfg.Guardrail.MaxRetries != 1 {
		t.Errorf("Guardrail.MaxRetries = %d, want 1", cfg.Guardrail.MaxRetries)
	}
}

func TestParseAgents_MissingAgentKey(t *testing.T) {
	for _, key := range []string{"repair", "hint", "guardrail", "curator"} {
		t.Run(key, func(t *testing.T) {
			doc := removeYAMLTopLevelKey(t, validAgentsYAML, key)
			_, err := ParseAgents([]byte(doc))
			if err == nil {
				t.Fatalf("expected error when %q is missing", key)
			}
		})
	}
}

func TestParseAgents_MissingModelPerAgent(t *testing.T) {
	doc := `
defaults: {n_submissions: 25, daily_requests_per_user: 20}
repair: {max_retries: 3}
hint: {model: "hint-model"}
guardrail: {model: "guardrail-model"}
curator: {model: "curator-model"}
pricing:
  hint-model: {input: 1, cached_input: 0.5, output: 2}
  guardrail-model: {input: 1, cached_input: 0.5, output: 2}
  curator-model: {input: 1, cached_input: 0.5, output: 2}
formatter: {enabled: false, command: ""}
`
	_, err := ParseAgents([]byte(doc))
	if err == nil {
		t.Fatal("expected error for repair agent missing model")
	}
}

// Both defaults fail open when zero — an omitted n_submissions removes the
// cap on how many submissions a request scrapes, and an omitted
// daily_requests_per_user makes the rate limiter 429 every request — so a
// missing or mistyped key has to fail at startup, not at first traffic.
func TestParseAgents_DefaultsMustBePositive(t *testing.T) {
	for _, tt := range []struct{ name, defaults string }{
		{"n_submissions missing", "{daily_requests_per_user: 20}"},
		{"n_submissions zero", "{n_submissions: 0, daily_requests_per_user: 20}"},
		{"n_submissions negative", "{n_submissions: -1, daily_requests_per_user: 20}"},
		{"daily_requests_per_user missing", "{n_submissions: 25}"},
		{"daily_requests_per_user zero", "{n_submissions: 25, daily_requests_per_user: 0}"},
		{"daily_requests_per_user negative", "{n_submissions: 25, daily_requests_per_user: -5}"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			doc := replaceYAMLDefaults(t, validAgentsYAML, tt.defaults)
			if _, err := ParseAgents([]byte(doc)); err == nil {
				t.Fatalf("expected error for defaults %s", tt.defaults)
			}
		})
	}
}

// The valid document must still parse — a both-directions check, so the
// validation above can't pass by rejecting everything.
func TestParseAgents_ValidDefaultsAccepted(t *testing.T) {
	doc := replaceYAMLDefaults(t, validAgentsYAML, "{n_submissions: 25, daily_requests_per_user: 20}")
	cfg, err := ParseAgents([]byte(doc))
	if err != nil {
		t.Fatalf("ParseAgents: %v", err)
	}
	if cfg.Defaults.NSubmissions != 25 || cfg.Defaults.DailyRequestsPerUser != 20 {
		t.Errorf("Defaults = %+v, want {25 20}", cfg.Defaults)
	}
}

// replaceYAMLDefaults swaps the document's whole defaults block for an
// inline mapping, so a test can express "this key is absent" as well as
// "this key holds a bad value".
func replaceYAMLDefaults(t *testing.T, doc, inline string) string {
	t.Helper()
	stripped := removeYAMLTopLevelKey(t, doc, "defaults")
	return "defaults: " + inline + "\n" + stripped
}

func TestParseAgents_UnknownAgentKeyRejected(t *testing.T) {
	doc := validAgentsYAML + "\nunknown_agent:\n  model: \"x\"\n"
	_, err := ParseAgents([]byte(doc))
	if err == nil {
		t.Fatal("expected error for unknown top-level key")
	}
}

func TestParseAgents_UnknownFieldWithinAgentRejected(t *testing.T) {
	doc := `
defaults: {n_submissions: 25, daily_requests_per_user: 20}
repair: {model: "repair-model", not_a_real_field: 1}
hint: {model: "hint-model"}
guardrail: {model: "guardrail-model"}
curator: {model: "curator-model"}
pricing:
  repair-model: {input: 1, cached_input: 0.5, output: 2}
  hint-model: {input: 1, cached_input: 0.5, output: 2}
  guardrail-model: {input: 1, cached_input: 0.5, output: 2}
  curator-model: {input: 1, cached_input: 0.5, output: 2}
formatter: {enabled: false, command: ""}
`
	_, err := ParseAgents([]byte(doc))
	if err == nil {
		t.Fatal("expected error for unknown field inside agent block")
	}
}

// max_retries and the two cost caps all fail *open* at zero: every
// enforcement point reads a zero cost cap as "unlimited", and MaxRetries is
// compared as `attempts >= MaxRetries`, so zero makes the loop return
// no_fix/no_hint without ever calling a model. A missing or mistyped key
// therefore has to be a startup error, for the same reason the defaults
// block is validated. The shipped agents.yaml had all three at zero, i.e.
// no ceiling on model spend at all.
func TestParseAgents_FailOpenCapsMustBePositive(t *testing.T) {
	for _, agent := range []string{"repair", "hint", "guardrail", "curator"} {
		for _, field := range []string{"max_retries", "max_cost_per_retry", "max_cost_per_loop"} {
			for _, value := range []string{"0", ""} {
				name := agent + "/" + field
				if value == "" {
					name += "/missing"
				} else {
					name += "/zero"
				}
				t.Run(name, func(t *testing.T) {
					doc := setAgentField(t, validAgentsYAML, agent, field, value)
					if _, err := ParseAgents([]byte(doc)); err == nil {
						t.Fatalf("expected error for %s %s = %q", agent, field, value)
					}
				})
			}
		}
	}
}

// setAgentField rewrites one field of one agent block, or removes it
// entirely when value is empty.
func setAgentField(t *testing.T, doc, agent, field, value string) string {
	t.Helper()
	lines := strings.Split(doc, "\n")
	inAgent := false
	var out []string
	for _, line := range lines {
		if !strings.HasPrefix(line, " ") && strings.HasSuffix(strings.TrimSpace(line), ":") {
			inAgent = strings.TrimSuffix(strings.TrimSpace(line), ":") == agent
		}
		if inAgent && strings.HasPrefix(strings.TrimSpace(line), field+":") {
			if value == "" {
				continue
			}
			out = append(out, "  "+field+": "+value)
			continue
		}
		out = append(out, line)
	}
	return strings.Join(out, "\n")
}

func TestParseAgents_NegativeCapsRejected(t *testing.T) {
	cases := []string{"max_retries", "max_cost_per_retry", "max_cost_per_loop", "top_n_mistakes", "n_tests_shown"}
	for _, field := range cases {
		t.Run(field, func(t *testing.T) {
			doc := `
defaults: {n_submissions: 25, daily_requests_per_user: 20}
repair:
  model: "repair-model"
  ` + field + `: -1
hint: {model: "hint-model"}
guardrail: {model: "guardrail-model"}
curator: {model: "curator-model"}
pricing:
  repair-model: {input: 1, cached_input: 0.5, output: 2}
  hint-model: {input: 1, cached_input: 0.5, output: 2}
  guardrail-model: {input: 1, cached_input: 0.5, output: 2}
  curator-model: {input: 1, cached_input: 0.5, output: 2}
formatter: {enabled: false, command: ""}
`
			_, err := ParseAgents([]byte(doc))
			if err == nil {
				t.Fatalf("expected error for negative %s", field)
			}
		})
	}
}

func TestParseAgents_MissingPricingEntryForConfiguredModel(t *testing.T) {
	doc := `
defaults: {n_submissions: 25, daily_requests_per_user: 20}
repair: {model: "repair-model"}
hint: {model: "hint-model"}
guardrail: {model: "guardrail-model"}
curator: {model: "curator-model"}
pricing:
  repair-model: {input: 1, cached_input: 0.5, output: 2}
  hint-model: {input: 1, cached_input: 0.5, output: 2}
  guardrail-model: {input: 1, cached_input: 0.5, output: 2}
formatter: {enabled: false, command: ""}
`
	_, err := ParseAgents([]byte(doc))
	if err == nil {
		t.Fatal("expected error: curator-model has no pricing entry")
	}
}

// TestParseAgents_PricingValuesMustBeUsable pins that a pricing entry's
// *values* are validated, not merely its presence. They fail open exactly the
// way the cost caps do: KnownFields rejects unknown keys but not missing ones,
// so a block that omits `output` parses cleanly, llm.Cost prices every output
// token at zero, and max_cost_per_retry/max_cost_per_loop never bind — the
// model spend ceilings silently become unlimited. That has to fail at startup,
// not at first traffic.
func TestParseAgents_PricingValuesMustBeUsable(t *testing.T) {
	cases := []struct{ name, entry string }{
		{"output_missing", `{input: 1.0, cached_input: 0.5}`},
		{"output_zero", `{input: 1.0, cached_input: 0.5, output: 0}`},
		{"input_missing", `{cached_input: 0.5, output: 2.0}`},
		{"input_zero", `{input: 0, cached_input: 0.5, output: 2.0}`},
		{"input_negative", `{input: -1.0, cached_input: 0.5, output: 2.0}`},
		{"cached_input_negative", `{input: 1.0, cached_input: -0.5, output: 2.0}`},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			doc := strings.Replace(validAgentsYAML,
				`repair-model: {input: 1.0, cached_input: 0.5, output: 2.0}`,
				`repair-model: `+tt.entry, 1)
			if doc == validAgentsYAML {
				t.Fatal("fixture changed: repair-model pricing line not found")
			}
			if _, err := ParseAgents([]byte(doc)); err == nil {
				t.Fatalf("expected error for pricing %s", tt.entry)
			}
		})
	}
}

// The other direction: cached_input at zero is a legitimate price (a provider
// that does not bill cached input at all), not a fail-open hole.
func TestParseAgents_ZeroCachedInputPriceIsAccepted(t *testing.T) {
	doc := strings.Replace(validAgentsYAML,
		`repair-model: {input: 1.0, cached_input: 0.5, output: 2.0}`,
		`repair-model: {input: 1.0, cached_input: 0, output: 2.0}`, 1)
	if _, err := ParseAgents([]byte(doc)); err != nil {
		t.Fatalf("ParseAgents: %v", err)
	}
}

func TestParseAgents_DefaultsBlockRequired(t *testing.T) {
	doc := removeYAMLTopLevelKey(t, validAgentsYAML, "defaults")
	_, err := ParseAgents([]byte(doc))
	if err == nil {
		t.Fatal("expected error when defaults block is missing")
	}
}

func TestCheckedInAgentsYAML_ParsesAndValidates(t *testing.T) {
	path := filepath.Join("..", "..", "agents.yaml")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	cfg, err := ParseAgents(data)
	if err != nil {
		t.Fatalf("checked-in agents.yaml failed to validate: %v", err)
	}
	if cfg.Repair.Model == "" || cfg.Hint.Model == "" || cfg.Guardrail.Model == "" || cfg.Curator.Model == "" {
		t.Fatal("checked-in agents.yaml has an agent with an empty model")
	}
}

// removeYAMLTopLevelKey is a small test helper that strips a single
// top-level "key:" block (and its indented lines) from a YAML document.
func removeYAMLTopLevelKey(t *testing.T, doc, key string) string {
	t.Helper()
	lines := splitLines(doc)
	var out []string
	skipping := false
	for _, line := range lines {
		if len(line) > 0 && line[0] != ' ' && line[0] != '\t' {
			skipping = hasPrefixKey(line, key)
			if skipping {
				continue
			}
		} else if skipping {
			continue
		}
		out = append(out, line)
	}
	return joinLines(out)
}

func hasPrefixKey(line, key string) bool {
	return len(line) > len(key) && line[:len(key)] == key && line[len(key)] == ':'
}

func splitLines(s string) []string {
	var lines []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			lines = append(lines, s[start:i])
			start = i + 1
		}
	}
	lines = append(lines, s[start:])
	return lines
}

func joinLines(lines []string) string {
	out := ""
	for i, l := range lines {
		if i > 0 {
			out += "\n"
		}
		out += l
	}
	return out
}

// TestParseAgents_GuardrailFamily covers both directions of the
// guardrail-independence rule: a guardrail sharing the hint writer's family
// must be rejected (a model reviewing its own output is not an independent
// check), and a genuinely different family must be accepted.
func TestParseAgents_GuardrailFamily(t *testing.T) {
	cases := []struct {
		name       string
		hint       string
		guardrail  string
		wantReject bool
	}{
		{"same family, different sizes", "gpt-4.1-mini", "gpt-4o", true},
		{"same family, different case and spacing", "gpt-4.1-mini", " GPT-4o ", true},
		// A routing prefix must not disguise the same family as two: the
		// vendor segment differs textually, the model does not.
		{"same family behind a provider prefix", "openai/gpt-4.1-mini", "gpt-4.1-mini", true},
		{"different families", "gpt-4.1-mini", "claude-sonnet-5", false},
		// Both behind one gateway, but genuinely different models — the
		// shared prefix must not be mistaken for a shared family.
		{"different families behind one gateway", "openrouter/openai/gpt-4.1", "openrouter/anthropic/claude-sonnet-5", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			doc := strings.NewReplacer(
				`model: "hint-model"`, fmt.Sprintf("model: %q", tc.hint),
				`model: "guardrail-model"`, fmt.Sprintf("model: %q", tc.guardrail),
				"  hint-model: {input: 1.0, cached_input: 0.5, output: 2.0}",
				fmt.Sprintf("  %q: {input: 1.0, cached_input: 0.5, output: 2.0}", tc.hint),
				"  guardrail-model: {input: 3.0, cached_input: 1.5, output: 6.0}",
				fmt.Sprintf("  %q: {input: 3.0, cached_input: 1.5, output: 6.0}", tc.guardrail),
			).Replace(validAgentsYAML)

			_, err := ParseAgents([]byte(doc))
			if tc.wantReject {
				if err == nil {
					t.Fatalf("hint %q + guardrail %q accepted, want rejected", tc.hint, tc.guardrail)
				}
				if !strings.Contains(err.Error(), "different model family") {
					t.Errorf("err = %v, want it to name the different-family rule", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("hint %q + guardrail %q rejected: %v", tc.hint, tc.guardrail, err)
			}
		})
	}
}

// EJUDGE_CONTEST_ID is optional — ejudge's own first contest is "1", which is
// what the platform client has always used — but it has to be *readable*.
// ejudge scopes both sessions to a single contest, so a service that can only
// ever talk to contest 1 cannot serve any other course, and it fails silently:
// it logs in, finds no runs, and answers no_submissions for every student.
func TestLoadEnv_ContestIDDefaultsButIsOverridable(t *testing.T) {
	env, err := LoadEnv(lookupFrom(fullEnv()))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if env.EjudgeContestID != "1" {
		t.Errorf("EjudgeContestID = %q, want the default %q", env.EjudgeContestID, "1")
	}

	withID := fullEnv()
	withID["EJUDGE_CONTEST_ID"] = "42"
	env, err = LoadEnv(lookupFrom(withID))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if env.EjudgeContestID != "42" {
		t.Errorf("EjudgeContestID = %q, want %q", env.EjudgeContestID, "42")
	}
}

// WORKER_CONCURRENCY is optional and defaults to the one-at-a-time pool the
// worker has always used, but it has to be reachable: Worker.Concurrency
// existed with no wiring at all, so an instance ran exactly one help request at
// a time — one request holding the slot for the whole repair loop, judge
// polling included — and turning it up needed a recompile. A value that would
// silently disable the pool (zero, negative, non-numeric) is a startup error
// for the same reason the zero cost caps are.
func TestLoadEnv_WorkerConcurrencyDefaultsButIsOverridable(t *testing.T) {
	env, err := LoadEnv(lookupFrom(fullEnv()))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if env.WorkerConcurrency != 1 {
		t.Errorf("WorkerConcurrency = %d, want the default 1", env.WorkerConcurrency)
	}

	withN := fullEnv()
	withN["WORKER_CONCURRENCY"] = "8"
	env, err = LoadEnv(lookupFrom(withN))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if env.WorkerConcurrency != 8 {
		t.Errorf("WorkerConcurrency = %d, want 8", env.WorkerConcurrency)
	}

	for _, bad := range []string{"0", "-1", "many"} {
		withBad := fullEnv()
		withBad["WORKER_CONCURRENCY"] = bad
		if _, err := LoadEnv(lookupFrom(withBad)); err == nil {
			t.Errorf("WORKER_CONCURRENCY=%q was accepted, want a startup error", bad)
		}
	}
}
