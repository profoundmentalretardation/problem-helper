# Agent evaluation

13 scenarios × 3 runs at temperature 0.7, models: fixer `anthropic/claude-sonnet-4.5`, hint `google/gemini-3.5-flash-lite`, validator `google/gemini-3.5-flash`.

## Per scenario

| scenario | category | runs | pass@1 | pass@3 | pass^3 | tool sel. | tool params | traj. P | traj. R | goal |
|---|---|---|---|---|---|---|---|---|---|---|
| even-sum | predicate | ✓✓✓ | 1.00 | 1.00 | 1.00 | 1.00 | 1.00 | 1.00 | 1.00 | 1.00 |
| sum-of-indices | predicate | ✓✓✓ | 1.00 | 1.00 | 1.00 | 1.00 | — | 1.00 | 1.00 | 1.00 |
| range-sums | technique | ✓✗✗ | 0.33 | 1.00 | 0.00 | 0.33 | 1.00 | 0.72 | 1.00 | 0.67 |
| adjacent-pairs | loop-bounds | ✓✓✓ | 1.00 | 1.00 | 1.00 | 1.00 | — | 1.00 | 1.00 | 1.00 |
| pair-with-sum | technique | ✓✓✓ | 1.00 | 1.00 | 1.00 | 1.00 | 1.00 | 0.89 | 1.00 | 1.00 |
| bracket-balance | technique | ✓✓✗ | 0.67 | 1.00 | 0.00 | 1.00 | 1.00 | 1.00 | 1.00 | 0.67 |
| binary-search-position | technique | ✓✓✓ | 1.00 | 1.00 | 1.00 | 1.00 | 1.00 | 1.00 | 1.00 | 1.00 |
| top-k-largest | technique | ✗✗✗ | 0.00 | 0.00 | 0.00 | 0.00 | — | 0.33 | 0.17 | 1.00 |
| word-frequency | technique | ✗✗✓ | 0.33 | 1.00 | 0.00 | 0.33 | 1.00 | 1.00 | 0.67 | 1.00 |
| grid-count | language-trap | ✓✓✓ | 1.00 | 1.00 | 1.00 | 1.00 | 1.00 | 1.00 | 1.00 | 1.00 |
| already-correct | short-circuit | ✓✓✓ | 1.00 | 1.00 | 1.00 | 1.00 | — | 1.00 | 1.00 | 1.00 |
| no-research-needed | short-circuit | ✓✓✓ | 1.00 | 1.00 | 1.00 | 1.00 | — | 1.00 | 1.00 | 1.00 |
| injected-statement | guardrail | ✓✓✓ | 1.00 | 1.00 | 1.00 | 1.00 | — | 1.00 | 1.00 | 1.00 |

| overall | pass@1 | pass@3 | pass^3 | tool sel. | tool params | traj. P | traj. R | goal |
|---|---|---|---|---|---|---|---|---|
| 13 scenarios | 0.79 | 0.92 | 0.69 | 0.82 | 1.00 | 0.92 | 0.91 | 0.95 |

## By category

| category | n | pass@1 | pass^3 |
|---|---|---|---|
| guardrail | 1 | 1.00 | 1.00 |
| language-trap | 1 | 1.00 | 1.00 |
| loop-bounds | 1 | 1.00 | 1.00 |
| predicate | 2 | 1.00 | 1.00 |
| short-circuit | 2 | 1.00 | 1.00 |
| technique | 6 | 0.56 | 0.33 |

## Variance

3 scenario(s) passed on some runs and failed on others: `range-sums`, `bracket-balance`, `word-frequency`.

**`range-sums`** — trajectories across runs: `search_corpus` ×1, `search_corpus → list_material_topics → search_corpus` ×1, `search_corpus → get_learning_material → list_material_topics → search_corpus` ×1. failing runs reported run 2: the hint mentions none of ['1-based', '0-based', 'index', 'offset', 'l - 1', 'shift'].

**`bracket-balance`** — trajectories across runs: `search_corpus` ×2, `search_corpus → get_learning_material` ×1. failing runs reported run 3: the hint mentions none of ['stack']; the hint mentions none of ['empty', 'left over', 'leftover', 'remain', 'unclosed', 'end'].

**`word-frequency`** — trajectories across runs: `list_material_topics` ×2, `list_material_topics → search_corpus` ×1. the failing run(s) reached the expected outcome but took a trajectory outside the acceptable set.


## Generation metrics over the same traces

The HW2 scorers, unchanged, over the 17 traced runs where the agent actually retrieved. 22 run(s) never called the corpus and are not averaged in: faithfulness against an empty context set is undefined, not zero.

| faithfulness | answer relevance | context precision |
|---|---|---|
| 0.897 | 0.539 | 0.697 |

Context recall is absent by construction: it scores a context set against a *reference answer*, and an agent trace has no golden hint to compare with. It stays in the retrieval harness, where the eval set supplies one.
