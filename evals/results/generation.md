# Generation metrics

Answering model `google/gemini-3.5-flash-lite`, judge `anthropic/claude-haiku-4.5`, top-5 passages.

| run | cases | faithfulness | answer relevance | context precision | context recall |
|---|---|---|---|---|---|
| reranking on | 27 | 0.914 | 0.778 | 0.789 | 0.872 |
| reranking off | 25 | 0.920 | 0.750 | 0.768 | 0.780 |
| reranking on, answerable only | 24 | 0.944 | 0.875 | 0.888 | 0.918 |
| reranking off, answerable only | 22 | 0.909 | 0.852 | 0.873 | 0.848 |

A correct refusal on an out-of-corpus question scores 1.0 on faithfulness (it invents nothing) and 0.0 on answer relevance (it does not answer), which is why the last two rows exclude those three cases. Both views are shown; neither is the headline on its own.

## By failure category (reranking on)

| category | n | faithfulness | answer relevance | context precision | context recall |
|---|---|---|---|---|---|
| acronym | 5 | 0.933 | 0.800 | 0.891 | 1.000 |
| ambiguous | 1 | 1.000 | 1.000 | 1.000 | 0.500 |
| exact_term | 4 | 0.750 | 0.750 | 0.808 | 0.875 |
| long_tail_detail | 4 | 1.000 | 1.000 | 0.938 | 0.938 |
| multi_hop | 2 | 1.000 | 1.000 | 1.000 | 1.000 |
| near_duplicate | 2 | 1.000 | 0.500 | 0.933 | 1.000 |
| negation | 2 | 1.000 | 1.000 | 1.000 | 1.000 |
| out_of_corpus | 3 | 0.667 | 0.000 | 0.000 | 0.500 |
| paraphrase | 4 | 1.000 | 1.000 | 0.751 | 0.821 |
