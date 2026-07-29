package llm_test

import (
	"testing"

	"github.com/profoundmentalretardation/problem-helper/internal/config"
	"github.com/profoundmentalretardation/problem-helper/internal/llm"
)

func TestCost(t *testing.T) {
	tests := []struct {
		name    string
		usage   llm.Usage
		pricing config.PricingConfig
		want    string
	}{
		{
			name:    "no cached tokens",
			usage:   llm.Usage{InputTokens: 1000, CachedInputTokens: 0, OutputTokens: 50},
			pricing: config.PricingConfig{Input: 3.00, CachedInput: 1.50, Output: 15.00},
			want:    "0.003750",
		},
		{
			// The double-counting trap: cached tokens are a SUBSET of input
			// tokens. A wrong implementation that bills input_tokens*p_in
			// PLUS cached_tokens*p_cached would get 0.004050 here instead.
			name:    "cached tokens are a subset of input, not additional",
			usage:   llm.Usage{InputTokens: 1000, CachedInputTokens: 200, OutputTokens: 50},
			pricing: config.PricingConfig{Input: 3.00, CachedInput: 1.50, Output: 15.00},
			want:    "0.003450",
		},
		{
			name:    "entirely cached input",
			usage:   llm.Usage{InputTokens: 500, CachedInputTokens: 500, OutputTokens: 0},
			pricing: config.PricingConfig{Input: 2.00, CachedInput: 0.50, Output: 10.00},
			want:    "0.000250",
		},
		{
			name:    "zero usage",
			usage:   llm.Usage{},
			pricing: config.PricingConfig{Input: 3.00, CachedInput: 1.50, Output: 15.00},
			want:    "0.000000",
		},
		// A negative count must never produce a negative cost: the agents add
		// Cost into their running per-retry and per-loop totals, so one
		// garbled usage block would otherwise subtract from those totals and
		// let a loop run to max_retries with neither cap ever binding.
		{
			name:    "negative input tokens are clamped, not billed as a credit",
			usage:   llm.Usage{InputTokens: -1_000_000, CachedInputTokens: 0, OutputTokens: 50},
			pricing: config.PricingConfig{Input: 3.00, CachedInput: 1.50, Output: 15.00},
			want:    "0.000750",
		},
		{
			name:    "negative output tokens are clamped, not billed as a credit",
			usage:   llm.Usage{InputTokens: 1000, CachedInputTokens: 0, OutputTokens: -1_000_000},
			pricing: config.PricingConfig{Input: 3.00, CachedInput: 1.50, Output: 15.00},
			want:    "0.003000",
		},
		{
			name:    "fractional cents from small pricing, rounds to 6 decimals",
			usage:   llm.Usage{InputTokens: 123, CachedInputTokens: 23, OutputTokens: 7},
			pricing: config.PricingConfig{Input: 0.15, CachedInput: 0.075, Output: 0.60},
			want:    "0.000021",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := llm.Cost(tt.usage, tt.pricing)
			if got != tt.want {
				t.Errorf("Cost(%+v, %+v) = %q, want %q", tt.usage, tt.pricing, got, tt.want)
			}
		})
	}
}
