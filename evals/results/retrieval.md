# Retrieval metrics

27 cases (3 out-of-corpus, excluded from every average below), 182 chunks.
Fusion `k=60`, 20 candidates per retriever, reranking the top 20.

## By stage and k

**k = 1**

| run | hit@k | precision@k | recall@k | MRR | nDCG@k |
|---|---|---|---|---|---|
| dense | 0.583 | 0.583 | 0.342 | 0.583 | 0.583 |
| bm25 | 0.583 | 0.583 | 0.358 | 0.583 | 0.583 |
| fused | 0.792 | 0.792 | 0.449 | 0.792 | 0.792 |
| rerank | 0.667 | 0.667 | 0.401 | 0.667 | 0.667 |

**k = 3**

| run | hit@k | precision@k | recall@k | MRR | nDCG@k |
|---|---|---|---|---|---|
| dense | 0.875 | 0.389 | 0.584 | 0.715 | 0.591 |
| bm25 | 0.792 | 0.347 | 0.537 | 0.674 | 0.555 |
| fused | 0.875 | 0.389 | 0.590 | 0.833 | 0.649 |
| rerank | 0.833 | 0.389 | 0.594 | 0.743 | 0.618 |

**k = 5**

| run | hit@k | precision@k | recall@k | MRR | nDCG@k |
|---|---|---|---|---|---|
| dense | 0.958 | 0.283 | 0.690 | 0.734 | 0.628 |
| bm25 | 0.833 | 0.233 | 0.590 | 0.684 | 0.571 |
| fused | 0.917 | 0.292 | 0.699 | 0.844 | 0.695 |
| rerank | 0.917 | 0.283 | 0.719 | 0.760 | 0.662 |

**k = 10**

| run | hit@k | precision@k | recall@k | MRR | nDCG@k |
|---|---|---|---|---|---|
| dense | 1.000 | 0.175 | 0.823 | 0.741 | 0.687 |
| bm25 | 0.917 | 0.167 | 0.757 | 0.693 | 0.645 |
| fused | 1.000 | 0.179 | 0.830 | 0.855 | 0.750 |
| rerank | 0.958 | 0.179 | 0.840 | 0.766 | 0.719 |

## Reranking on vs. off, per failure category (k = 5)

| category (n) | run | hit@5 | recall@5 | MRR | nDCG@5 |
|---|---|---|---|---|---|
| acronym (5) | fused | 1.000 | 0.783 | 1.000 | 0.814 |
|  | +rerank | 1.000 | 0.733 | 1.000 | 0.774 |
| ambiguous (1) | fused | 0.000 | 0.000 | 0.000 | 0.000 |
|  | +rerank | 0.000 | 0.000 | 0.000 | 0.000 |
| exact_term (4) | fused | 1.000 | 0.833 | 1.000 | 0.806 |
|  | +rerank | 1.000 | 0.708 | 0.875 | 0.677 |
| long_tail_detail (4) | fused | 1.000 | 0.775 | 1.000 | 0.813 |
|  | +rerank | 1.000 | 0.775 | 1.000 | 0.814 |
| multi_hop (2) | fused | 1.000 | 0.750 | 0.625 | 0.632 |
|  | +rerank | 1.000 | 1.000 | 0.667 | 0.772 |
| near_duplicate (2) | fused | 1.000 | 0.500 | 0.750 | 0.473 |
|  | +rerank | 1.000 | 0.667 | 0.350 | 0.457 |
| negation (2) | fused | 1.000 | 1.000 | 1.000 | 1.000 |
|  | +rerank | 1.000 | 1.000 | 1.000 | 1.000 |
| out_of_corpus (3) | fused | — | — | — | — |
|  | +rerank | — | — | — | — |
| paraphrase (4) | fused | 0.750 | 0.479 | 0.625 | 0.479 |
|  | +rerank | 0.750 | 0.583 | 0.425 | 0.401 |

## The packing trap

The identical reranked chunks, scored in retriever order and after `pack_for_lim`. Hit rate, precision and recall cannot move — they do not read position — so a harness that scores the packed order looks fine in three metrics out of five.

**k = 5**

| run | hit@k | precision@k | recall@k | MRR | nDCG@k |
|---|---|---|---|---|---|
| retriever order | 0.917 | 0.283 | 0.719 | 0.760 | 0.662 |
| LIM-packed order | 0.917 | 0.283 | 0.719 | 0.753 | 0.660 |

**k = 10**

| run | hit@k | precision@k | recall@k | MRR | nDCG@k |
|---|---|---|---|---|---|
| retriever order | 0.958 | 0.179 | 0.840 | 0.766 | 0.719 |
| LIM-packed order | 0.958 | 0.179 | 0.840 | 0.758 | 0.700 |

## First relevant chunk per case, by stage

| case | category | dense | bm25 | fused | +rerank |
|---|---|---|---|---|---|
| bisect-left-vs-right | exact_term | 1 | 1 | 1 | 1 |
| list-pop-zero-queue | exact_term | 1 | 1 | 1 | 1 |
| heapq-max-heap | exact_term | 1 | 1 | 1 | 1 |
| counter-most-common | exact_term | 2 | 3 | 1 | 2 |
| bfs-visits-node-twice | acronym | 2 | 1 | 1 | 1 |
| dp-knapsack-loop-direction | acronym | 1 | 1 | 1 | 1 |
| gcd-to-lcm | acronym | 1 | 1 | 1 | 1 |
| lifo-vs-fifo | acronym | 1 | 1 | 1 | 1 |
| lis-efficient | acronym | 1 | 1 | 1 | 1 |
| even-sum-adds-wrong-thing | paraphrase | 1 | 2 | 1 | 1 |
| judge-says-too-slow | paraphrase | 3 | 9 | 2 | 7 |
| grid-rows-change-together | paraphrase | 5 | — | 8 | 5 |
| negative-floor-division | long_tail_detail | 2 | 1 | 1 | 1 |
| two-pointers-or-sliding-window | near_duplicate | 2 | 1 | 1 | 2 |
| set-or-counter-for-duplicates | near_duplicate | 3 | 3 | 2 | 5 |
| dijkstra-negative-weights | negation | 1 | 1 | 1 | 1 |
| greedy-coin-change-valid | negation | 1 | 1 | 1 | 1 |
| binary-search-hangs | long_tail_detail | 1 | 2 | 1 | 1 |
| recursion-error-deep-input | multi_hop | 6 | 9 | 4 | 3 |
| sort-desc-score-asc-name | multi_hop | 1 | 1 | 1 | 1 |
| output-has-brackets | long_tail_detail | 2 | 4 | 1 | 1 |
| string-building-slow | paraphrase | 1 | 1 | 1 | 2 |
| prefix-sum-leading-zero | long_tail_detail | 1 | 2 | 1 | 1 |
| make-it-faster | ambiguous | 4 | — | 7 | — |
| segment-tree-lazy | out_of_corpus | — | — | — | — |
| red-black-tree | out_of_corpus | — | — | — | — |
| which-web-framework | out_of_corpus | — | — | — | — |

`—` means no golden chunk in the top 10; out-of-corpus cases have no golden chunk at all and show `—` everywhere by construction.
