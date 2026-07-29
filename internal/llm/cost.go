package llm

import (
	"math/big"

	"github.com/profoundmentalretardation/problem-helper/internal/config"
)

// Usage is one call's token usage, as extracted from an OpenAI-compatible
// response's "usage" object. CachedInputTokens is a SUBSET of InputTokens
// (the API reports it nested under prompt_tokens_details.cached_tokens, not
// as an addition) — billing it on top of InputTokens double-counts and
// inflates cost.
type Usage struct {
	InputTokens       int
	CachedInputTokens int
	OutputTokens      int
}

// Cost computes the exact decimal cost of one call, per the plan's formula:
//
//	cost = (input_tokens - cached_tokens)*p_in + cached_tokens*p_cached + output_tokens*p_out
//
// pricing rates are per 1M tokens. The arithmetic runs in exact rational
// form (big.Rat) rather than float64, so the result is fit to store as
// `numeric` without accumulating rounding error beyond what the configured
// per-1M rates already carry, and is formatted as a fixed 6-decimal string.
func Cost(u Usage, pricing config.PricingConfig) string {
	million := big.NewRat(1_000_000, 1)
	uncachedInput := u.InputTokens - u.CachedInputTokens

	total := new(big.Rat)
	total.Add(total, tokenCost(uncachedInput, pricing.Input, million))
	total.Add(total, tokenCost(u.CachedInputTokens, pricing.CachedInput, million))
	total.Add(total, tokenCost(u.OutputTokens, pricing.Output, million))

	return total.FloatString(6)
}

func tokenCost(tokens int, perMillion float64, million *big.Rat) *big.Rat {
	price := new(big.Rat)
	if price.SetFloat64(perMillion) == nil {
		price = new(big.Rat) // NaN/Inf pricing treated as zero rather than corrupting the total
	}
	n := new(big.Rat).SetInt64(int64(tokens))
	return new(big.Rat).Quo(new(big.Rat).Mul(n, price), million)
}
