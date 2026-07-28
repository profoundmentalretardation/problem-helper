package config

import (
	"errors"
	"os"
	"path/filepath"
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
  max_cost_per_retry: 0
  max_cost_per_loop: 0
  top_n_mistakes: 5
  n_tests_shown: 10
hint:
  model: "hint-model"
  temperature: 0.7
  max_retries: 3
  max_cost_per_retry: 0
  max_cost_per_loop: 0
guardrail:
  model: "guardrail-model"
curator:
  model: "curator-model"
  max_retries: 2
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
	if cfg.Guardrail.MaxRetries != 0 {
		t.Errorf("Guardrail.MaxRetries default = %d, want 0", cfg.Guardrail.MaxRetries)
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

func TestParseAgents_ZeroCapsAreUnlimited(t *testing.T) {
	cfg, err := ParseAgents([]byte(validAgentsYAML))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Repair.MaxCostPerRetry != 0 || cfg.Repair.MaxCostPerLoop != 0 {
		t.Fatalf("expected zero caps to parse as 0 (unlimited), got %+v", cfg.Repair)
	}
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
