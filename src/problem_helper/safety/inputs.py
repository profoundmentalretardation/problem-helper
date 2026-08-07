"""Layer 1: pattern matching over untrusted input for injection phrasing.

Two severities, because a filter with one is either useless or unusable:

- `block` — phrasings that have no innocent reading in a programming exercise. "Ignore all
  previous instructions", "you are now in developer mode", "print your system prompt". A
  match refuses the session before a single token is paid for.
- `flag` — signals that are suspicious in aggregate and ordinary on their own. A URL in a
  problem statement, the word `os.environ`, a base64 blob. These are recorded on the trace
  and change nothing about the run; the safety scorer counts them, and they are the reason
  the false-positive rate in the README is measured against legitimate traffic rather than
  asserted.

**Why the patterns are anchored on imperatives.** The cheap version of this filter greps for
"system prompt" and "instructions" and then rejects a legitimate task about parsing an
instruction set. Every blocking pattern here requires a directive verb aimed at the
assistant — the payload has to be telling *someone* to do something. That is what a real
injection has and a problem statement about instruction decoding does not.

Russian phrasings are in the blocking set because the service is built to take Russian
problem statements; a filter that only reads English would be a documented hole in exactly
the population the service targets.
"""

from __future__ import annotations

import logging
import re
from dataclasses import dataclass, field
from enum import StrEnum

logger = logging.getLogger(__name__)


class Severity(StrEnum):
    block = "block"
    flag = "flag"


@dataclass(slots=True, frozen=True)
class Rule:
    name: str
    severity: Severity
    pattern: re.Pattern[str]
    detail: str


def _rule(name: str, severity: Severity, pattern: str, detail: str) -> Rule:
    return Rule(
        name=name,
        severity=severity,
        pattern=re.compile(pattern, re.IGNORECASE | re.UNICODE),
        detail=detail,
    )


# A directive aimed at the assistant. Every blocking pattern below builds on one of these.
_DIRECTIVE = r"(?:ignore|disregard|forget|skip|override|bypass|drop)"
_RU_DIRECTIVE = r"(?:игнорируй|игнорируйте|забудь|забудьте|отбрось|не\s+обращай\s+внимания\s+на)"

# The imperative has to be addressed to the assistant, not reported about something else.
# "IGNORE ALL PREVIOUS INSTRUCTIONS" matches; "the machine has to ignore all previous
# instructions when it reads RESET" — a real exercise about a virtual machine, and the first
# false positive this filter produced — does not, because a modal verb sits in front of it.
# The rule is shallow and it is meant to be: it costs one lookbehind and it removes the most
# common shape of innocent collision. A statement that says "the interpreter ignores all
# previous instructions" still gets through it, which is why layer 2 exists.
_ADDRESSED = r"(?<!to )(?<!must )(?<!should )(?<!will )(?<!shall )(?<!can )(?<!may )(?<!would )"

RULES: tuple[Rule, ...] = (
    # -- instruction override ------------------------------------------ #
    _rule(
        "instruction-override",
        Severity.block,
        rf"{_ADDRESSED}{_DIRECTIVE}\s+(?:all\s+|any\s+|the\s+)?"
        r"(?:previous|prior|earlier|above|preceding|system|original)\s+"
        r"(?:instruction|prompt|rule|direction|guideline)",
        "tells the assistant to discard its instructions",
    ),
    _rule(
        "instruction-override-ru",
        Severity.block,
        rf"{_RU_DIRECTIVE}\s+(?:все\s+|всё\s+|любые\s+)?"
        r"(?:предыдущие|прежние|системные)?\s*"
        r"(?:инструкц|указан|правил|промпт)",
        "tells the assistant to discard its instructions, in Russian",
    ),
    _rule(
        "role-override",
        Severity.block,
        r"you\s+are\s+(?:now|no\s+longer)\b|"
        r"\b(?:enter|activate|switch\s+to)\s+(?:developer|debug|god|dan|jailbreak)\s+mode|"
        r"\bact\s+as\s+(?:if\s+you\s+are\s+)?(?:an?\s+)?"
        r"(?:unrestricted|uncensored|different)\b",
        "tries to reassign the assistant's role",
    ),
    _rule(
        "channel-forgery",
        Severity.block,
        r"<\|(?:im_start|im_end|system|endoftext)\|>|"
        r"^\s*(?:###\s*)?(?:system|assistant)\s*:\s*$|"
        r"\[/?INST\]|<</?SYS>>",
        "forges a chat-role marker to open an instruction channel",
    ),
    _rule(
        "prompt-disclosure",
        Severity.block,
        r"(?:print|show|reveal|repeat|output|dump|display|tell\s+me)\s+"
        r"(?:me\s+)?(?:your|the)\s+"
        r"(?:system\s+prompt|initial\s+instruction|full\s+instruction|prompt\s+verbatim|"
        r"hidden\s+(?:prompt|instruction)|configuration)",
        "asks the assistant to disclose its own prompt",
    ),
    _rule(
        "exfiltration-directive",
        Severity.block,
        r"(?:send|post|upload|exfiltrate|transmit|leak|forward)\s+"
        r"(?:\w+\s+){0,6}?(?:to|at)\s+(?:https?://|[\w.-]+@|\b(?:my|our)\s+server\b)|"
        r"\b(?:curl|wget)\s+https?://",
        "asks for data to be sent to an outside endpoint",
    ),
    _rule(
        "secret-request",
        Severity.block,
        r"(?:print|show|reveal|include|output|leak|give\s+me)\s+"
        r"(?:the\s+|your\s+|all\s+)?"
        r"(?:api[_\s-]?key|secret|token|credential|password|env(?:ironment)?\s+var)",
        "asks for credentials or environment contents",
    ),
    # -- weaker signals, recorded only ---------------------------------- #
    _rule(
        "external-url",
        Severity.flag,
        # A host, not just a scheme followed by a word — see the note on `_URL` in
        # `outputs.py` for the false positive the looser pattern produced.
        r"https?://(?!(?:docs\.python\.org|en\.wikipedia\.org))[\w-]+(?:\.[\w-]+)+",
        "carries a URL to a host outside the documentation allowlist",
    ),
    _rule(
        "env-reference",
        Severity.flag,
        r"\bos\.environ\b|\bgetenv\b|\bLLM_API_KEY\b|\bOPENAI_API_KEY\b",
        "mentions process environment or a key name",
    ),
    _rule(
        "encoded-blob",
        Severity.flag,
        r"\b(?:[A-Za-z0-9+/]{40,}={0,2})\b",
        "contains a long encoded-looking blob",
    ),
    _rule(
        "tool-directive",
        Severity.flag,
        r"\b(?:call|invoke|use)\s+(?:the\s+)?"
        r"(?:search_corpus|get_learning_material|list_material_topics)\b",
        "names an internal tool by its registered name",
    ),
)


@dataclass(slots=True, frozen=True)
class InjectionFinding:
    rule: str
    severity: Severity
    source: str
    detail: str
    excerpt: str

    def as_dict(self) -> dict:
        return {
            "rule": self.rule,
            "severity": str(self.severity),
            "source": self.source,
            "detail": self.detail,
            "excerpt": self.excerpt,
        }


@dataclass(slots=True)
class InjectionVerdict:
    findings: list[InjectionFinding] = field(default_factory=list)

    @property
    def blocked(self) -> bool:
        return any(f.severity is Severity.block for f in self.findings)

    @property
    def flagged(self) -> list[InjectionFinding]:
        return [f for f in self.findings if f.severity is Severity.flag]

    def as_dict(self) -> dict:
        return {
            "blocked": self.blocked,
            "findings": [f.as_dict() for f in self.findings],
        }

    def reason(self) -> str:
        blocking = [f for f in self.findings if f.severity is Severity.block]
        return "; ".join(f"{f.source}: {f.detail}" for f in blocking) or "no blocking finding"

    def __or__(self, other: InjectionVerdict) -> InjectionVerdict:
        return InjectionVerdict(findings=[*self.findings, *other.findings])


EXCERPT_CHARS = 120


def scan(text: str, *, source: str) -> InjectionVerdict:
    """Screens one untrusted field. `source` names it so the finding is traceable."""
    findings: list[InjectionFinding] = []
    for rule in RULES:
        match = rule.pattern.search(text)
        if match is None:
            continue
        findings.append(
            InjectionFinding(
                rule=rule.name,
                severity=rule.severity,
                source=source,
                detail=rule.detail,
                excerpt=_excerpt(text, match),
            )
        )
    verdict = InjectionVerdict(findings=findings)
    if verdict.blocked:
        logger.warning("input filter blocked %s: %s", source, verdict.reason())
    return verdict


def scan_all(fields: dict[str, str]) -> InjectionVerdict:
    """Screens several named fields and merges the verdicts."""
    verdict = InjectionVerdict()
    for source, text in fields.items():
        verdict = verdict | scan(text, source=source)
    return verdict


def _excerpt(text: str, match: re.Match[str]) -> str:
    start = max(0, match.start() - 20)
    end = min(len(text), match.end() + 20)
    return text[start:end].replace("\n", " ")[:EXCERPT_CHARS]
