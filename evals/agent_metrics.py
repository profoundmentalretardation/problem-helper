"""Part 1 metrics over one run's trajectory and outcome. Pure functions, no I/O.

Four metrics, computed from an ordered `list[ToolCall]` and the case's expectations:

| metric | question it answers | shape |
|---|---|---|
| tool selection accuracy | did it call the right tools at all | 0/1 per run |
| tool parameter accuracy | were the arguments usable | ratio over calls |
| trajectory precision / recall | did it do those things and only those, in order | ratio |
| goal completion | did the session end where it was supposed to | 0/1 per run |

**Acceptable alternatives, not one golden path.** A hint agent that answers a two-pointers
question by searching once is not worse than one that lists the topics first and then
searches; both are correct research. A case therefore names a *set* of acceptable
trajectories, and every metric scores against the alternative that fits the actual run best.
Pinning a single expected sequence would measure conformity to one recorded run, which is
the failure mode that makes agent eval suites go stale in a week.

**Order matters, but only within an alternative.** Precision and recall are computed over
the longest common subsequence of tool names, so calling the right two tools in the wrong
order costs recall without zeroing it, and an extra unnecessary call costs precision. Set
comparison would miss both.

**Goal completion is structural, deliberately.** It checks the outcome code the pipeline
reached, that the hint names the concept the case is about, and that any material it cites
was actually opened. It does not ask a judge whether the hint is *good* — that is what the
HW2 generation scorers do over the same trace (`evals/trace_scorers.py`), and mixing a
sampled judgement into a reliability metric would make pass^3 measure the judge's variance
as much as the agent's.
"""

from __future__ import annotations

import re
from dataclasses import dataclass, field
from typing import Any

from .trajectory import ToolCall


@dataclass(slots=True)
class Expectation:
    """What one scenario considers a correct run."""

    alternatives: list[list[str]]
    """Acceptable tool sequences, by name. An empty inner list means "no tool call"."""

    parameters: dict[str, dict] = field(default_factory=dict)
    """Per-tool argument constraints; see `check_parameters`."""

    outcome: str = "hint_ready"
    error_code: str | None = None
    hint_must_mention: list[list[str]] = field(default_factory=list)
    """Groups of synonyms; the hint has to match at least one term from every group."""

    materials: list[str] = field(default_factory=list)
    """Material ids that would be reasonable to cite. Empty means any, including none."""

    @classmethod
    def from_case(cls, case: dict) -> Expectation:
        return cls(
            alternatives=[list(alt) for alt in case["expected_tools"]],
            parameters=case.get("expected_parameters", {}),
            outcome=case["expected_outcome"].get("outcome", "hint_ready"),
            error_code=case["expected_outcome"].get("error_code"),
            hint_must_mention=[list(g) for g in case["expected_outcome"].get("mentions", [])],
            materials=list(case["expected_outcome"].get("materials", [])),
        )


# --------------------------------------------------------------------------- #
# Trajectory metrics
# --------------------------------------------------------------------------- #


def tool_selection_accuracy(actual: list[ToolCall], expectation: Expectation) -> float:
    """1.0 when the tools used are exactly one of the acceptable sets, ignoring order.

    Multiset, not set: an alternative that allows two searches is not satisfied by one, and
    a run that searches four times when two were expected has not selected the same tools.
    """
    used = sorted(call.tool for call in actual)
    return 1.0 if any(sorted(alt) == used for alt in expectation.alternatives) else 0.0


def check_parameters(call: ToolCall, constraints: dict) -> tuple[bool, str]:
    """Whether one call's arguments are usable. Returns the verdict and why not.

    The constraints are about *usability*, never about exact wording: an eval that demands
    a specific query string measures paraphrase, not competence.

    - `mentions`: groups of synonyms; the joined string arguments must hit one term per group
    - `k_between`: inclusive bounds on the `k` argument
    - `one_of`: the argument must be one of these literal values
    - `min_words`: the query has to be a question, not a keyword
    """
    text = " ".join(str(v) for v in call.arguments.values() if isinstance(v, str)).lower()

    for group in constraints.get("mentions", []):
        if not any(term.lower() in text for term in group):
            return False, f"none of {group} appears in {text[:60]!r}"

    if "min_words" in constraints and len(text.split()) < constraints["min_words"]:
        return False, f"query is {len(text.split())} word(s), expected at least {constraints['min_words']}"

    for name, allowed in constraints.get("one_of", {}).items():
        if name in call.arguments and call.arguments[name] not in allowed:
            return False, f"{name}={call.arguments[name]!r} is not one of {allowed}"

    if "k_between" in constraints and "k" in call.arguments:
        low, high = constraints["k_between"]
        if not low <= call.arguments["k"] <= high:
            return False, f"k={call.arguments['k']} outside [{low}, {high}]"

    return True, ""


def tool_parameter_accuracy(
    actual: list[ToolCall], expectation: Expectation
) -> tuple[float | None, list[str]]:
    """Fraction of calls whose arguments pass their tool's constraints.

    `None`, not 0.0, when the run made no call the case constrains — a scenario where not
    searching is acceptable would otherwise be punished for taking that option. The
    aggregate reports how many runs were skipped, exactly as the retrieval harness does for
    cases with an empty golden set.
    """
    judged = [call for call in actual if call.tool in expectation.parameters]
    if not judged:
        return None, []
    failures = []
    for call in judged:
        ok, why = check_parameters(call, expectation.parameters[call.tool])
        if not ok:
            failures.append(f"{call.tool}: {why}")
    return (len(judged) - len(failures)) / len(judged), failures


def _lcs_length(left: list[str], right: list[str]) -> int:
    """Longest common subsequence — order-sensitive overlap without demanding adjacency."""
    if not left or not right:
        return 0
    row = [0] * (len(right) + 1)
    for item in left:
        previous = 0
        for j, other in enumerate(right, start=1):
            previous, row[j] = row[j], (previous + 1 if item == other else max(row[j], row[j - 1]))
    return row[-1]


def trajectory_scores(
    actual: list[ToolCall], expectation: Expectation
) -> tuple[float, float, list[str]]:
    """Precision, recall and the alternative they were scored against.

    Scored against the alternative with the best F1, so an agent is compared to the plan it
    came closest to following rather than to an arbitrary first entry.
    """
    names = [call.tool for call in actual]
    best = (0.0, 0.0, expectation.alternatives[0] if expectation.alternatives else [])
    best_f1 = -1.0
    for alternative in expectation.alternatives or [[]]:
        matched = _lcs_length(names, alternative)
        precision = matched / len(names) if names else (1.0 if not alternative else 0.0)
        recall = matched / len(alternative) if alternative else (1.0 if not names else 0.0)
        f1 = 0.0 if precision + recall == 0 else 2 * precision * recall / (precision + recall)
        if f1 > best_f1:
            best_f1, best = f1, (precision, recall, alternative)
    return best


# --------------------------------------------------------------------------- #
# Goal completion
# --------------------------------------------------------------------------- #


def goal_completion(
    outcome: dict, expectation: Expectation, *, cited_materials: list[str]
) -> tuple[float, list[str]]:
    """1.0 when the run ended where the case says it should, with the reasons it did not."""
    reasons: list[str] = []

    reached = outcome.get("outcome") or ""
    error = outcome.get("error_code") or None
    if expectation.error_code:
        if error != expectation.error_code:
            reasons.append(f"expected error {expectation.error_code}, got {error or reached!r}")
    elif reached != expectation.outcome:
        reasons.append(f"expected outcome {expectation.outcome}, got {reached or error!r}")

    hint = (outcome.get("hint") or "").lower()
    for group in expectation.hint_must_mention:
        if not any(_mentions(hint, term) for term in group):
            reasons.append(f"the hint mentions none of {group}")

    if expectation.materials:
        stray = [mid for mid in cited_materials if mid not in expectation.materials]
        if stray:
            reasons.append(f"cited unexpected material(s) {stray}")

    return (1.0 if not reasons else 0.0), reasons


def _mentions(hint: str, term: str) -> bool:
    """A single term matches as a **stem** anchored at a word boundary; a phrase matches literally.

    Stems are the point: the case file writes `boundar`, `descend`, `balanc`, so that
    "boundaries", "descending" and "unbalanced" all count and the metric is not a test of
    which inflection the model happened to pick. The cost is that a stem also matches an
    unrelated longer word — `sum` matches "summary" — so terms in the case file are chosen
    long enough that the collision does not arise, and the anchor at least keeps a stem from
    matching in the middle of a word.
    """
    term = term.lower()
    if " " in term or not term.isalpha():
        return term in hint
    return re.search(rf"\b{re.escape(term)}\w*", hint) is not None


# --------------------------------------------------------------------------- #
# Aggregation across the three runs of a scenario
# --------------------------------------------------------------------------- #


def pass_rates(passed: list[bool]) -> dict[str, float]:
    """pass@1, pass@k and pass^k over the runs of one scenario.

    All three are reported because at k=3 each says something the others cannot:

    - `pass@1` is the mean — the chance a single user gets a good answer.
    - `pass@3` collapses to "did it ever pass", which at three runs is nearly free to
      satisfy and says almost nothing on its own. It is here so the gap to pass^3 is visible.
    - `pass^3` — passed *every* time — is the number that describes a reliable agent, and it
      is the one that goes in the headline of the report table.
    """
    if not passed:
        return {"pass@1": 0.0, "pass@k": 0.0, "pass^k": 0.0}
    return {
        "pass@1": sum(passed) / len(passed),
        "pass@k": 1.0 if any(passed) else 0.0,
        "pass^k": 1.0 if all(passed) else 0.0,
    }


def flaky(passed: list[bool]) -> bool:
    """A scenario that neither always passes nor always fails — where the signal is."""
    return any(passed) and not all(passed)


def mean(values: list[float | None]) -> float | None:
    """Average over the runs that produced a number; `None` when every run skipped."""
    present = [v for v in values if v is not None]
    return sum(present) / len(present) if present else None


def summarise(rows: list[dict], metrics: tuple[str, ...]) -> dict[str, Any]:
    """Per-metric means plus how many runs each one skipped."""
    out: dict[str, Any] = {}
    for metric in metrics:
        values = [row.get(metric) for row in rows]
        out[metric] = mean(values)
        skipped = sum(1 for v in values if v is None)
        if skipped:
            out[f"{metric}_skipped"] = skipped
    return out
