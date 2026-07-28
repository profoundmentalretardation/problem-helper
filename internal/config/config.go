// Package config loads the service configuration: required environment
// variables and the checked-in agents.yaml.
package config

import (
	"bytes"
	"fmt"
	"os"

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
}

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
		{"max_retries", float64(agent.MaxRetries)},
		{"max_cost_per_retry", agent.MaxCostPerRetry},
		{"max_cost_per_loop", agent.MaxCostPerLoop},
		{"top_n_mistakes", float64(agent.TopNMistakes)},
		{"n_tests_shown", float64(agent.NTestsShown)},
	}
	for _, c := range caps {
		if c.value < 0 {
			return fmt.Errorf("config: agents.yaml: agent %q field %q must not be negative, got %v", name, c.field, c.value)
		}
	}
	return nil
}
