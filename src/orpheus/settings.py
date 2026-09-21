from pathlib import Path

from pydantic import Field, SecretStr
from pydantic_settings import BaseSettings, SettingsConfigDict

from orpheus.configuration import EnvironmentCipher, Profiles


class Settings(BaseSettings):
    model_config = SettingsConfigDict(env_file=".env", extra="ignore")

    database_url: str
    readiness_timeout: float = Field(default=2, gt=0)
    public_api_keys: list[SecretStr] = Field(default_factory=list)
    env_encryption_key: SecretStr = SecretStr("")
    orpheus_config_file: Path = Path("orpheus.toml")
    harness_env_allowlist: list[str] = Field(default_factory=list)
    sandbox_proxy_url: SecretStr = SecretStr(
        "socks5h://sandbox-proxy.agentbox.ru:65180"
    )
    max_concurrent_sessions: int = Field(default=50, gt=0)
    max_tool_result_bytes: int = Field(default=524288, ge=2)
    max_request_bytes: int = Field(default=1048576, gt=0)
    cancel_grace_seconds: float = Field(default=30, gt=0)
    worker_poll_seconds: float = Field(default=1, gt=0)
    rpc_timeout_seconds: float = Field(default=30, gt=0)

    def runtime(self, *, api: bool = False) -> tuple[Profiles, EnvironmentCipher]:
        if api and (
            not self.public_api_keys
            or any(not key.get_secret_value() for key in self.public_api_keys)
        ):
            raise ValueError("PUBLIC_API_KEYS must contain nonempty keys")
        return Profiles.load(self.orpheus_config_file), EnvironmentCipher(
            self.env_encryption_key
        )
