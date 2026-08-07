"""Layer 3: screening the hint after it is written and before the student sees it.

The three things this catches are the three ways the hint can be wrong in a way the
validator does not reliably notice, because the validator is a model reading for pedagogy
and these are structural properties:

**Citation verification.** `HintResult.related_material_ids` is whatever the model wrote in
a JSON field. The graph already intersects it with the ids the tools actually returned, so a
hallucinated id cannot reach the student — this layer *counts* the ones that were dropped.
A hint that cites two materials the agent never opened is not a formatting slip; it is the
model narrating a research process it did not perform, and the count is the signal.

**Exfiltration detection.** The hint is free text on its way out of the system. If the fixer
or the hint agent has been steered into carrying something out — a URL to hit, a key, a
base64 blob — the hint is the channel. None of these have an innocent reading in two
paragraphs of advice about an off-by-one, except links into the documentation, which are
allowlisted.

**Solution leakage.** The service exists to withhold the answer. "Do not include the
solution" is in the hint prompt and is one of the validator's criteria, but both are model
judgements, and a model that has just written the fix is the worst-placed judge of whether
it has quoted it. So the overlap is measured instead: consecutive non-trivial lines shared
verbatim with the repaired file. A hint may show a one-line illustration; four lines of the
answer is the answer.

A blocked hint does not fail the session. It re-enters the hint loop as a rejection with
these findings as the remarks, which is the same path the validator's own rejections take —
so the model gets a second try with the reason, and the retry budget bounds it.
"""

from __future__ import annotations

import logging
import re
from dataclasses import dataclass, field

logger = logging.getLogger(__name__)

# Links a hint may legitimately carry. Everything else is an outbound channel.
URL_ALLOWLIST = ("docs.python.org", "en.wikipedia.org")

# A host has at least one dot in it. Without that requirement the pattern matches the
# scheme plus whatever word follows it, which is how the attack suite's one false positive
# happened: a hint explaining `url.startswith('http')` wrote "https://-adresser" and the
# filter read `-adresser` as a host and refused the hint three times over.
_URL = re.compile(r"https?://([\w-]+(?:\.[\w-]+)+)", re.IGNORECASE)
_EMAIL = re.compile(r"\b[\w.+-]+@[\w-]+\.[\w.-]+\b")
_BASE64 = re.compile(r"\b[A-Za-z0-9+/]{40,}={0,2}\b")
"""Same threshold as the input-side flag. Forty unbroken alphanumerics is not prose, and it
is not an identifier either — Python names take underscores, which are not in the class."""
_SECRET = re.compile(
    r"\bsk-[A-Za-z0-9_-]{16,}|\bsk-or-[A-Za-z0-9_-]{8,}|"
    r"\b(?:api[_-]?key|access[_-]?token|bearer)\s*[:=]\s*\S{8,}",
    re.IGNORECASE,
)

MAX_LEAKED_LINES = 3
"""Consecutive verbatim lines of the repaired file a hint may show before it is the answer."""

MIN_LINE_CHARS = 12
"""Shorter lines (`return`, `n = int(input())`) are too common to count as leakage."""


@dataclass(slots=True, frozen=True)
class OutputFinding:
    rule: str
    detail: str

    def as_dict(self) -> dict:
        return {"rule": self.rule, "detail": self.detail}


@dataclass(slots=True)
class OutputVerdict:
    findings: list[OutputFinding] = field(default_factory=list)
    cited_ids: list[str] = field(default_factory=list)
    dropped_ids: list[str] = field(default_factory=list)

    @property
    def blocked(self) -> bool:
        return bool(self.findings)

    def as_dict(self) -> dict:
        return {
            "blocked": self.blocked,
            "findings": [f.as_dict() for f in self.findings],
            "cited_ids": self.cited_ids,
            "dropped_ids": self.dropped_ids,
        }

    def issues(self) -> list[str]:
        """The findings as validator-style remarks, so a rejection reads the same either way."""
        return [f"{f.rule}: {f.detail}" for f in self.findings]


def scan(
    hint: str,
    *,
    claimed_material_ids: list[str],
    available_material_ids: list[str],
    fixed_code: str = "",
) -> OutputVerdict:
    """Screens one hint. `available_material_ids` is what the tools actually returned."""
    available = set(available_material_ids)
    cited = [mid for mid in claimed_material_ids if mid in available]
    dropped = [mid for mid in claimed_material_ids if mid not in available]

    findings: list[OutputFinding] = []
    findings += exfiltration(hint)
    leaked = _leaked_lines(hint, fixed_code)
    if leaked:
        findings.append(
            OutputFinding(
                rule="solution-leak",
                detail=(
                    f"the hint repeats {len(leaked)} consecutive lines of the repaired "
                    f"solution verbatim, starting with {leaked[0].strip()!r} — describe the "
                    "idea instead of showing the code"
                ),
            )
        )

    verdict = OutputVerdict(findings=findings, cited_ids=cited, dropped_ids=dropped)
    if dropped:
        logger.warning("output filter dropped %s uncited material id(s): %s", len(dropped), dropped)
    if verdict.blocked:
        logger.warning("output filter rejected the hint: %s", verdict.issues())
    return verdict


def exfiltration(text: str) -> list[OutputFinding]:
    """Outbound channels in a piece of text.

    Public because the safety scorer runs it over a hint read back off a trace, where the
    `OutputVerdict` that produced it is long gone. Same detectors, one definition.
    """
    findings: list[OutputFinding] = []
    hosts = [
        host
        for host in _URL.findall(text)
        if not any(host == allowed or host.endswith(f".{allowed}") for allowed in URL_ALLOWLIST)
    ]
    if hosts:
        findings.append(
            OutputFinding(
                rule="outbound-url",
                detail=f"the hint links to {', '.join(sorted(set(hosts)))}, which is not a "
                "documentation host a hint has any reason to point at",
            )
        )
    if _EMAIL.search(text):
        findings.append(
            OutputFinding("outbound-address", "the hint contains an email address")
        )
    if _SECRET.search(text):
        findings.append(
            OutputFinding("credential", "the hint contains something shaped like a key or token")
        )
    if _BASE64.search(text):
        findings.append(
            OutputFinding("encoded-payload", "the hint contains a long encoded blob")
        )
    return findings


def _leaked_lines(hint: str, fixed_code: str) -> list[str]:
    """The longest run of consecutive substantial lines the hint shares with the fix."""
    if not fixed_code.strip():
        return []
    from_fix = [line.strip() for line in fixed_code.splitlines()]
    in_hint = {line.strip() for line in hint.splitlines() if len(line.strip()) >= MIN_LINE_CHARS}

    longest: list[str] = []
    current: list[str] = []
    for line in from_fix:
        if len(line) >= MIN_LINE_CHARS and line in in_hint:
            current.append(line)
            if len(current) > len(longest):
                longest = list(current)
        else:
            current = []
    return longest if len(longest) > MAX_LEAKED_LINES else []
