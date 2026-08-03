"""Entry point: `uv run problem-helper`."""

import logging

import uvicorn

from .config import get_settings


def main() -> None:
    logging.basicConfig(
        level=logging.INFO,
        format="%(asctime)s %(levelname)s %(name)s: %(message)s",
    )
    settings = get_settings()
    uvicorn.run(
        "problem_helper.api:app",
        host=settings.host,
        port=settings.port,
        log_level="info",
    )
