# research/ — frozen Python prototypes

Read-only reference. These are the course prototypes the Go service (see `../AGENTS.md` and
`../docs/plans/completed/20260729-mvp-service.md`) is built from. Where the plan and a notebook disagree,
the plan is the priority.

## Notebook → Go package map

| Notebook | Prototyped | Go successor |
|---|---|---|
| `01_repair_loop.ipynb` | find and fix the bug in a student's submission | `internal/agent/repair/` |
| `02_hint_loop.ipynb` (+ `02_hint_loop.md`) | turn the fix into a non-obvious hint, reject bad ones | `internal/agent/hint/` |
| `03_prompt_shield.ipynb` | clean student code before any model sees it | `internal/shield/` |
| `04_curator_loop.ipynb` | decide what, out of a finished run, is worth remembering | `internal/agent/curator/` |
| `05_monitor.ipynb` / `monitor.py` | judge on its own clock, over a hash-chained event log | **no MVP successor** — deferred to Post-MVP (see the plan's Post-MVP ideas list) |
| `idea.ipynb` | early scratch notes | none — superseded by the plan itself |

Supporting modules:

- `memory/` — domain model, context assembly, and shared/curator storage prototyped in
  `01_repair_loop.ipynb` and `04_curator_loop.ipynb`; superseded by `internal/store/` and
  `internal/prompt/`.
- `rules/` — prompt fragments (`tutor_rules.md`, `curator_brief.md`) referenced by the repair
  and curator notebooks; superseded by `prompts/*.md`.
- `ask_tutor.py` — CLI harness used to drive the notebooks manually; no Go successor, the HTTP
  API (`internal/api/`) replaces it.
- `scratch/` — `.py` sources the notebooks were generated from (percent-format cells) plus
  trace-capture scripts; not maintained, see `scratch/README.md`.
- `traces/`, `slides/`, `state/` — captured run traces, course presentation slides, and local
  run artifacts (event logs, sqlite state) from developing the prototypes. `state/*.db` files
  are gitignored (see `../.gitignore`).

## Why frozen

`05_monitor.ipynb` explicitly has **no MVP successor**: the hash-chained log + monitor agent is
deferred to Post-MVP in the plan. Everything else here informed a real Go package and stays
purely as a reference for behavior/prompts/test-corpus porting (e.g. the shield's injection
corpus and the hint loop's deterministic pre-checks are ported into Go tests, not reimplemented
by reading this code at runtime).
