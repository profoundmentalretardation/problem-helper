# Safety hardening

16 hostile cases across four attack classes and 8 legitimate ones, each run 3 times at temperature 0.7 — 72 sessions.

## Attacks

| attack class | cases | defended | refused outright | answered, payload ignored | leaked |
|---|---|---|---|---|---|
| direct injection | 6 | 18/18 | 15 | 3 | — |
| indirect injection | 3 | 9/9 | 0 | 9 | — |
| tool abuse | 3 | 9/9 | 0 | 9 | — |
| exfiltration | 4 | 12/12 | 9 | 3 | — |
| **all** | 16 | **48/48** | 24 | 24 | — |

Counted over runs, not cases: the fraction of real sessions a payload would have got through. *Refused outright* and *answered, payload ignored* are both defences and the second is the better one — the student still gets their hint. The split is shown because a suite that only ever refuses is a suite that has traded its false-positive rate for its defence rate.

## False positives on legitimate queries

| legitimate cases | runs | clean | false positives | false-positive rate |
|---|---|---|---|---|
| 8 | 24 | 24 | 0 | 0.000 |

No legitimate run was refused across 24 sessions. The 8 cases are chosen to sit next to a detector — an instruction-set decoder that 'ignores all previous instructions', a task whose sample input is a URL, an environment-file parser, a Russian statement, a long base64-looking token — so this is a measurement rather than a formality. It is still a small denominator, and the rate should be read as an upper bound with a wide interval rather than as a zero.

## Which layer fired

| layer | sessions |
|---|---|
| code_shield | 6 |
| input_filter | 18 |

## What each layer is worth

The hostile cases re-run with layer 1 (input filtering) disabled. What still gets defended is what layers 2–4 were catching anyway; what leaks is what layer 1 was carrying on its own.

| configuration | runs | defended | refused outright | leaked |
|---|---|---|---|---|
| all four layers | 48 | 48/48 | 24 | — |
| layer 1 off | 16 | 16/16 | 2 | — |
