"""Guardrails around the pipeline, arranged as four layers.

The threat here is specific and worth naming before the defences. The service takes a
problem statement and a file of code, both written by whoever is holding the browser, and
feeds them to three models; one of those models then reads passages out of a corpus and
writes text that goes back to a student. So there are four ways in:

| attack | where it enters | what stops it |
|---|---|---|
| direct prompt injection | the task statement or the code | layers 1 and 2 |
| indirect prompt injection | a corpus passage the agent retrieves | layer 2, then layer 3 |
| tool abuse | the model's own tool calls | layer 4 |
| data exfiltration | the hint, or code the fixer writes | layers 3 and 4 |

**Layer 1 — input filtering** (`inputs.scan`). Pattern matching over the untrusted text for
instruction-override and exfiltration phrasing. High-confidence matches refuse the session;
weaker signals are recorded on the trace and nothing else. This layer is cheap and catches
the unsubtle majority, and it is the only layer that can say no before any tokens are paid
for.

**Layer 2 — structural separation** (`channels.fence`). Every piece of untrusted text
reaches a prompt inside a delimited block that says what it is, and the system prompts
carry a standing rule that text inside such a block is data. The delimiter is stripped out
of the body first, so a payload cannot close its own fence and start talking in the
instruction channel. This is what makes layer 1 survivable: layer 1 will miss a phrasing
sooner or later, and layer 2 does not depend on recognising the phrasing at all.

**Layer 3 — output filtering** (`outputs.scan`). Applied to the hint, after the model has
written it and before the student sees it. It verifies every cited material id against what
the tools actually returned, looks for exfiltration channels (URLs, keys, base64 blobs), and
refuses a hint that is the solution in disguise. A rejection here re-enters the existing
hint retry loop, so the model gets told what was wrong exactly as if the validator had said
it.

**Layer 4 — capability constraints.** Not a module: the property that the dangerous thing is
not reachable. `codeshield` refuses code that reaches past stdin/stdout, the container the
code runs in has no network and a read-only rootfs, and the three tools take a query string
and a material id and can do nothing else — there is no tool that writes, deletes, fetches a
URL or shells out, so there is no tool call to abuse into one. `tools.read_materials`
already rebuilds the reading list from tool results rather than from what the model claimed.

Every layer records what it did on the trace as a `GUARDRAIL` span, which is what lets
`evals.safety_scorer` run over stored traces as a pure function and count both catches and
false positives.
"""

from __future__ import annotations

from .channels import FENCE_RULE, fence
from .inputs import InjectionFinding, InjectionVerdict, Severity
from .inputs import scan as scan_input
from .outputs import OutputFinding, OutputVerdict
from .outputs import scan as scan_output

__all__ = [
    "FENCE_RULE",
    "InjectionFinding",
    "InjectionVerdict",
    "OutputFinding",
    "OutputVerdict",
    "Severity",
    "fence",
    "scan_input",
    "scan_output",
]
