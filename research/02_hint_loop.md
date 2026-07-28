# Hint loop — `02_hint_loop.ipynb`

Stage 2: turn loop 1's verified fix into a hint. The hard part is rejecting hints, not writing
them — a model asked to be helpful drifts towards "on line 7, change `n - k` to `n - k + 1`",
which teaches nothing. Three tools: `propose_hint`, `report_hint_verdict` (verdict enum plus a
0-100 integer), `deliver_hint`.

The checker runs on a different model family from the writer, because a model grading its own
output agrees with itself too often. Deterministic rules run first — quoted repaired code, line
numbers, "change X to Y" — so a hopeless hint costs zero model calls. Stops: approved, round
cap, or the same hint twice. The gate is on `deliver_hint`, since writing hints is free but
delivering one cannot be undone and spends one of the student's allowances. The checker answers
by calling a tool; prose, the wrong tool, a verdict outside the enum or a dead connection are
not approvals — they give `checker_failed` and nothing goes out.

22 checks, no API key. Writer and checker are scripted separately, so a test can hold the writer
steady and break only the checker. I test the rules in both directions — four hints that must be
caught, two that must pass, since a checker rejecting everything is useless — and that found a
real hole: my first version only matched whole repaired lines, so "use `range(1, n - k + 1)`
instead" sailed through. It now pulls call expressions out of the changed lines too. Then five
ways for the checker to break, each ending with no hint delivered. Known gap: the directness
score is uncalibrated, so I only log it.
