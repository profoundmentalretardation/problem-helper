"""Service settings. Everything is overridable via .env / environment variables."""

from functools import lru_cache

from pydantic_settings import BaseSettings, SettingsConfigDict

from .schemas import SandboxBackend


class Settings(BaseSettings):
    model_config = SettingsConfigDict(env_file=".env", extra="ignore")

    # --- LLM provider (OpenAI-compatible, OpenRouter by default) ---
    llm_base_url: str = "https://openrouter.ai/api/v1"
    llm_api_key: str = ""
    llm_timeout_sec: float = 180.0
    llm_max_retries: int = 2
    # 0.0 for the service, so a re-run of the same session gives the same advice. The agent
    # eval raises it — three identical runs cannot measure reliability. See evals/run_agent.py.
    llm_temperature: float = 0.0

    # --- Model roles ---
    fixer_model: str = "anthropic/claude-sonnet-4.5"
    hint_model: str = "google/gemini-3.5-flash-lite"
    validator_model: str = "google/gemini-3.5-flash"

    # --- Loop limits ---
    max_fix_attempts: int = 3
    max_hint_attempts: int = 3

    # --- Retrieval (see retrieval/service.py for what the depths buy) ---
    retrieval_embed_model: str = "BAAI/bge-small-en-v1.5"
    retrieval_rerank_model: str = "Xenova/ms-marco-MiniLM-L-6-v2"
    retrieval_top_k: int = 5
    retrieval_candidates: int = 20
    retrieval_rerank_depth: int = 20
    retrieval_rrf_k: int = 60
    retrieval_rerank: bool = True
    retrieval_cache_dir: str = ".rag_cache"

    # --- Sandbox ---
    # `docker` is the default and there is no `auto`: an unreachable daemon fails the
    # session instead of silently downgrading to the weaker isolation. See sandbox/.
    sandbox_backend: SandboxBackend = SandboxBackend.docker
    sandbox_image: str = "python:3.13-alpine"
    sandbox_timeout_sec: float = 5.0
    sandbox_memory_mb: int = 256
    sandbox_max_output_bytes: int = 8_000

    # --- Guardrails ---
    codeshield_enabled: bool = True
    input_filter_enabled: bool = True
    output_filter_enabled: bool = True

    # --- Tracing ---
    tracing_enabled: bool = True
    # A database backend, not `./mlruns`: MLflow 3 refuses the file store outright, and the
    # trace search and feedback APIs the eval harness runs on need it anyway.
    mlflow_tracking_uri: str = "sqlite:///mlflow.db"
    mlflow_experiment: str = "problem-helper"

    # --- Storage ---
    db_path: str = "problem_helper.db"
    checkpoint_db_path: str = "problem_helper_checkpoints.db"

    # --- HTTP ---
    host: str = "127.0.0.1"
    port: int = 8000


@lru_cache
def get_settings() -> Settings:
    return Settings()
