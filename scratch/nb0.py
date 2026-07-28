# %% [markdown]
# # problem-helper — project outline
#
# A service that helps a student find the bug in their own competitive-programming submission
# without showing them the answer.
#
# These notebooks are the concept stage: we are settling the tool schemas, the loop shapes and
# the error handling here, and the service itself gets written in Go once the design stops
# moving. Nothing here talks to a real judge yet.

# %% [markdown]
# ## Flow
#
# A request comes in with `problem_id` and `user_id`. We ask the platform (Codeforces, ejudge,
# Yandex Contest) for that student's attempts and take the best one — most tests passed, most
# recent on a tie. If it already passes everything we answer and stop. Otherwise:
#
# ```
#   [ shield ]   03_prompt_shield.ipynb   strip comments, scan, release or hold
#       |
#   [ loop 1 ]   01_repair_loop.ipynb     find the defect, fix it, verify against the tests
#       |
#   [ loop 2 ]   02_hint_loop.ipynb       turn the fix into a hint, check it with another model
#       |
#    student
# ```
#
# The shield is not optional — everything after it feeds attacker-controlled text to a model.

# %% [markdown]
# ## Decisions that are the same in all three
#
# **Verification is not the model's decision.** In each loop the model proposes through a tool
# with a schema — `propose_fix`, `propose_hint`, `report_injection_finding` — and the
# orchestrator checks the proposal with code the model cannot reach or skip. The model chooses
# what to try; it never decides whether it worked.
#
# **A tool error is not a result.** Tools return `ok: false` instead of raising or returning
# something that looks like data. A broken test runner does not spend the model's retries, an
# unreadable checker does not become an approval, a crashed lexer does not become "nothing
# found".
#
# **Failure points one way.** No path through any loop ends in code being forwarded, a hint
# being delivered, or a submission released because something went wrong.
#
# **One gate per loop, on the one irreversible thing** — submitting to a judge, sending a hint,
# forwarding code to a third-party gateway. Everything reversible runs unattended, because a
# gate on every step is a gate nobody reads.

# %% [markdown]
# ## Running these
#
# ```
# python -m venv .venv && .venv/bin/pip install -r requirements.txt
# ```
#
# Each notebook runs top to bottom on its own and needs no API key: the tests replace the model
# with a scripted stand-in, while the tools stay real. For the live cell at the bottom, put
# `LLM_BASE_URL` and `LLM_API_KEY` in `.env`. Model ids come from the environment too
# (`MODEL_FIXER`, `MODEL_HINTER`, `MODEL_GUARDRAIL`, `MODEL_TRIAGE`) since they depend on the
# gateway; loop 2 wants `MODEL_GUARDRAIL` to be a different vendor from `MODEL_HINTER`.
#
# `state/` is generated and not committed.
#
# ## Not done yet
#
# Per-user spend caps (only retry caps exist), session summaries in memory, and a real sandbox
# for running student code — right now it is a plain subprocess with a timeout.
