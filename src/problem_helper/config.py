"""Service settings. Everything is overridable via .env / environment variables."""

from functools import lru_cache

from pydantic_settings import BaseSettings, SettingsConfigDict


class Settings(BaseSettings):
    model_config = SettingsConfigDict(env_file=".env", extra="ignore")

    # --- LLM provider (OpenAI-compatible, OpenRouter by default) ---
    llm_base_url: str = "https://openrouter.ai/api/v1"
    llm_api_key: str = ""
    llm_timeout_sec: float = 180.0
    llm_max_retries: int = 2

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
    sandbox_timeout_sec: float = 5.0
    sandbox_memory_mb: int = 256
    sandbox_max_output_bytes: int = 8_000

    # --- Storage ---
    db_path: str = "problem_helper.db"
    checkpoint_db_path: str = "problem_helper_checkpoints.db"

    # --- HTTP ---
    host: str = "127.0.0.1"
    port: int = 8000


@lru_cache
def get_settings() -> Settings:
    return Settings()
