// Package config loads the service configuration: required environment
// variables and the checked-in agents.yaml.
package config

import (
	"bytes"
	"fmt"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

// requiredEnvVars is also the source of truth for TestLoadEnv_MissingEachRequiredVar.
var requiredEnvVars = []string{
	"DATABASE_URL",
	"LLM_BASE_URL",
	"LLM_API_KEY",
	"PLATFORM",
	"API_TOKEN",
	"ADMIN_TOKEN",
	"EJUDGE_URL",
	"EJUDGE_SYSTEM_LOGIN",
	"EJUDGE_SYSTEM_PASSWORD",
}

// Env holds every required environment variable.
type Env struct {
	DatabaseURL          string
	LLMBaseURL           string
	LLMAPIKey            string
	Platform             string
	APIToken             string
	AdminToken           string
	EjudgeURL            string
	EjudgeSystemLogin    string
	EjudgeSystemPassword string
	// EjudgeContestID is the one ejudge contest both sessions are scoped to.
	// Optional, defaulting to defaultEjudgeContestID: pointing the service at
	// a course whose contest id is not "1" was otherwise impossible, and it
	// failed *silently* — the client logged into contest 1, found no runs and
	// answered no_submissions for every student.
	EjudgeContestID string
}

// defaultEjudgeContestID matches ejudge's own first contest, and the default
// the platform client has always used.
const defaultEjudgeContestID = "1"

// MissingEnvError reports a required environment variable that was absent
// or empty.
type MissingEnvError struct {
	Name string
}

func (e *MissingEnvError) Error() string {
	return fmt.Sprintf("config: required environment variable %s is not set", e.Name)
}

// LoadEnv reads every required environment variable via lookup (typically
// os.LookupEnv). An empty value is treated the same as a missing one.
func LoadEnv(lookup func(string) (string, bool)) (Env, error) {
	values := make(map[string]string, len(requiredEnvVars))
	for _, name := range requiredEnvVars {
		v, ok := lookup(name)
		if !ok || v == "" {
			return Env{}, &MissingEnvError{Name: name}
		}
		values[name] = v
	}
	contestID, ok := lookup("EJUDGE_CONTEST_ID")
	if !ok || contestID == "" {
		contestID = defaultEjudgeContestID
	}
	return Env{
		DatabaseURL:          values["DATABASE_URL"],
		LLMBaseURL:           values["LLM_BASE_URL"],
		LLMAPIKey:            values["LLM_API_KEY"],
		Platform:             values["PLATFORM"],
		APIToken:             values["API_TOKEN"],
		AdminToken:           values["ADMIN_TOKEN"],
		EjudgeURL:            values["EJUDGE_URL"],
		EjudgeSystemLogin:    values["EJUDGE_SYSTEM_LOGIN"],
		EjudgeSystemPassword: values["EJUDGE_SYSTEM_PASSWORD"],
		EjudgeContestID:      contestID,
	}, nil
}

// DefaultsConfig holds the agents.yaml "defaults" block.
type DefaultsConfig struct {
	NSubmissions         int `yaml:"n_submissions"`
	DailyRequestsPerUser int `yaml:"daily_requests_per_user"`
}

// AgentConfig holds the per-agent settings shared by repair/hint/guardrail/curator.
type AgentConfig struct {
	Model           string  `yaml:"model"`
	Temperature     float64 `yaml:"temperature"`
	ReasoningEffort string  `yaml:"reasoning_effort"`
	MaxRetries      int     `yaml:"max_retries"`
	MaxCostPerRetry float64 `yaml:"max_cost_per_retry"`
	MaxCostPerLoop  float64 `yaml:"max_cost_per_loop"`
	TopNMistakes    int     `yaml:"top_n_mistakes"`
	NTestsShown     int     `yaml:"n_tests_shown"`
}

// PricingConfig holds per-1M-token pricing for one model.
type PricingConfig struct {
	Input       float64 `yaml:"input"`
	CachedInput float64 `yaml:"cached_input"`
	Output      float64 `yaml:"output"`
}

// FormatterConfig holds the optional external formatter step.
type FormatterConfig struct {
	Enabled bool   `yaml:"enabled"`
	Command string `yaml:"command"`
}

// AgentsConfig is the parsed and validated agents.yaml.
type AgentsConfig struct {
	Defaults  DefaultsConfig
	Repair    AgentConfig
	Hint      AgentConfig
	Guardrail AgentConfig
	Curator   AgentConfig
	Pricing   map[string]PricingConfig
	Formatter FormatterConfig
}

// rawAgentsConfig mirrors AgentsConfig but with pointers for the required
// blocks, so a missing block can be told apart from an empty one; decoding
// this with KnownFields(true) also rejects unknown top-level and nested keys.
type rawAgentsConfig struct {
	Defaults  *DefaultsConfig          `yaml:"defaults"`
	Repair    *AgentConfig             `yaml:"repair"`
	Hint      *AgentConfig             `yaml:"hint"`
	Guardrail *AgentConfig             `yaml:"guardrail"`
	Curator   *AgentConfig             `yaml:"curator"`
	Pricing   map[string]PricingConfig `yaml:"pricing"`
	Formatter FormatterConfig          `yaml:"formatter"`
}

// modelFamily reduces a model id to the family prefix comparisons should
// use: "gpt-4.1-mini" and "gpt-4o" are the same family, "gpt-4.1-mini" and
// "claude-sonnet-5" are not.
//
// Routing prefixes are stripped rather than compared. Comparing the vendor
// segment of "openai/gpt-4.1-mini" would make it a different family from a
// bare "gpt-4.1-mini" — the same model written two ways would pass the
// guardrail-independence check — and would collapse every model behind a
// single gateway ("openrouter/openai/...", "openrouter/anthropic/...") into
// one family, rejecting a genuinely independent pair. Only the last path
// segment names the model, so that is what the family is taken from.
func modelFamily(model string) string {
	m := strings.ToLower(strings.TrimSpace(model))
	if i := strings.LastIndex(m, "/"); i >= 0 {
		m = m[i+1:]
	}
	family, _, _ := strings.Cut(m, "-")
	return family
}

// ParseAgents parses and validates an agents.yaml document.
func ParseAgents(data []byte) (AgentsConfig, error) {
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)

	var raw rawAgentsConfig
	if err := dec.Decode(&raw); err != nil {
		return AgentsConfig{}, fmt.Errorf("config: parsing agents.yaml: %w", err)
	}

	if raw.Defaults == nil {
		return AgentsConfig{}, fmt.Errorf("config: agents.yaml: %q block is required", "defaults")
	}
	// Both defaults fail *open* when zero, so an omitted or mistyped key is
	// far worse than a loud startup error: n_submissions == 0 skips the
	// truncation in every platform backend (the unbounded scrape maxNSubmissions
	// exists to prevent), and daily_requests_per_user == 0 makes the rate
	// limiter reject every single POST /help.
	if raw.Defaults.NSubmissions <= 0 {
		return AgentsConfig{}, fmt.Errorf(
			"config: agents.yaml: defaults.%s must be positive, got %d", "n_submissions", raw.Defaults.NSubmissions)
	}
	if raw.Defaults.DailyRequestsPerUser <= 0 {
		return AgentsConfig{}, fmt.Errorf(
			"config: agents.yaml: defaults.%s must be positive, got %d",
			"daily_requests_per_user", raw.Defaults.DailyRequestsPerUser)
	}

	agents := map[string]*AgentConfig{
		"repair":    raw.Repair,
		"hint":      raw.Hint,
		"guardrail": raw.Guardrail,
		"curator":   raw.Curator,
	}
	for name, agent := range agents {
		if agent == nil {
			return AgentsConfig{}, fmt.Errorf("config: agents.yaml: agent %q is required", name)
		}
		if agent.Model == "" {
			return AgentsConfig{}, fmt.Errorf("config: agents.yaml: agent %q is missing required field %q", name, "model")
		}
		if err := validateCaps(name, *agent); err != nil {
			return AgentsConfig{}, err
		}
	}

	for name, agent := range agents {
		if _, ok := raw.Pricing[agent.Model]; !ok {
			return AgentsConfig{}, fmt.Errorf("config: agents.yaml: model %q (agent %q) has no pricing entry", agent.Model, name)
		}
	}

	// The guardrail exists to catch what the hint writer got wrong; a model
	// asked to review its own output is not an independent check. Enforced
	// here so a config edit can't silently degrade the gate to
	// self-approval — the invariant is otherwise only a convention.
	if modelFamily(raw.Hint.Model) == modelFamily(raw.Guardrail.Model) {
		return AgentsConfig{}, fmt.Errorf(
			"config: agents.yaml: guardrail model %q must be a different model family than the hint model %q",
			raw.Guardrail.Model, raw.Hint.Model)
	}

	return AgentsConfig{
		Defaults:  *raw.Defaults,
		Repair:    *raw.Repair,
		Hint:      *raw.Hint,
		Guardrail: *raw.Guardrail,
		Curator:   *raw.Curator,
		Pricing:   raw.Pricing,
		Formatter: raw.Formatter,
	}, nil
}

// Config is the fully loaded service configuration.
type Config struct {
	Env    Env
	Agents AgentsConfig
}

// Load reads the required environment variables and the agents.yaml file
// at agentsPath.
func Load(agentsPath string) (*Config, error) {
	env, err := LoadEnv(os.LookupEnv)
	if err != nil {
		return nil, err
	}

	data, err := os.ReadFile(agentsPath)
	if err != nil {
		return nil, fmt.Errorf("config: reading %s: %w", agentsPath, err)
	}

	agents, err := ParseAgents(data)
	if err != nil {
		return nil, err
	}

	return &Config{Env: env, Agents: agents}, nil
}

func validateCaps(name string, agent AgentConfig) error {
	type namedCap struct {
		field string
		value float64
	}
	caps := []namedCap{
		{"top_n_mistakes", float64(agent.TopNMistakes)},
		{"n_tests_shown", float64(agent.NTestsShown)},
	}
	for _, c := range caps {
		if c.value < 0 {
			return fmt.Errorf("config: agents.yaml: agent %q field %q must not be negative, got %v", name, c.field, c.value)
		}
	}

	// These three fail *open* at zero, so a missing or mistyped key has to
	// be a startup error rather than a surprise in production — the same
	// reason the defaults block is validated. Every enforcement point reads
	// a zero cost cap as "unlimited" (repair.go, hint.go, curator.go), so
	// the shipped config was running with no ceiling on model spend at all;
	// and MaxRetries is compared as `attempts >= MaxRetries`, so zero makes
	// the loop return no_fix/no_hint without ever calling a model.
	required := []namedCap{
		{"max_retries", float64(agent.MaxRetries)},
		{"max_cost_per_retry", agent.MaxCostPerRetry},
		{"max_cost_per_loop", agent.MaxCostPerLoop},
	}
	for _, c := range required {
		if c.value <= 0 {
			return fmt.Errorf("config: agents.yaml: agent %q field %q must be positive, got %v", name, c.field, c.value)
		}
	}
	return nil
}
