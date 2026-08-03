from dataclasses import dataclass, field

from pydantic import BaseModel

from problem_helper.sandbox import TestOutcome, TestReport
from problem_helper.schemas import TestCase


@dataclass
class FakeCall:
    model: str
    system: str
    user: str
    schema: type[BaseModel]


@dataclass
class FakeLLM:
    """Serves canned answers keyed by schema type; the last answer repeats."""

    script: dict[type[BaseModel], list[BaseModel]]
    calls: list[FakeCall] = field(default_factory=list)

    async def structured(self, *, model: str, system: str, user: str, schema):
        self.calls.append(FakeCall(model=model, system=system, user=user, schema=schema))
        queue = self.script[schema]
        return queue.pop(0) if len(queue) > 1 else queue[0]

    def users_for(self, schema: type[BaseModel]) -> list[str]:
        return [c.user for c in self.calls if c.schema is schema]


def report(passed: bool, total: int = 2) -> TestReport:
    """A synthetic report, built without spawning processes."""
    return TestReport(
        outcomes=[
            TestOutcome(
                index=i,
                passed=passed,
                input="",
                expected_output="7",
                actual_output="7" if passed else "-1",
                stderr="",
                exit_code=0,
                timed_out=False,
                duration_ms=1,
            )
            for i in range(total)
        ]
    )


def sample_tests() -> list[TestCase]:
    return [TestCase(input="3 4\n", expected_output="7")]
