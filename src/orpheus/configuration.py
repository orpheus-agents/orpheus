"""Deployment profiles and immutable, harness-neutral execution configuration."""

import json
import os
import re
import tomllib
from pathlib import Path
from typing import Annotated, Literal
from uuid import UUID

from cryptography.fernet import Fernet
from pydantic import (
    BaseModel,
    ConfigDict,
    Field,
    SecretStr,
    field_validator,
    model_validator,
)

ENV_NAME = re.compile(r"^[A-Za-z_][A-Za-z0-9_]*$")
RESERVED_ENV = frozenset(
    {
        "ALL_PROXY",
        "NO_PROXY",
        "HTTP_PROXY",
        "HTTPS_PROXY",
        "all_proxy",
        "no_proxy",
        "http_proxy",
        "https_proxy",
        "HOME",
        "CODEX_HOME",
        "ORPHEUS_LAUNCH_ID",
        "PUBLIC_API_KEYS",
        "OPENAI_API_KEY",
        "CODEX_API_KEY",
        "OPENAI_BASE_URL",
        "OPENAI_ORG_ID",
        "OPENAI_ORGANIZATION",
        "OPENAI_PROJECT_ID",
        "CHATGPT_BASE_URL",
        "ENV_ENCRYPTION_KEY",
        "AGENTBOX_API_KEY",
    }
)


class StrictModel(BaseModel):
    model_config = ConfigDict(extra="forbid", strict=True)


NonEmpty = Annotated[str, Field(min_length=1)]


class APIKeyAuth(StrictModel):
    mode: Literal["api_key"]
    api_key_env: NonEmpty

    @field_validator("api_key_env")
    @classmethod
    def env_name(cls, value: str) -> str:
        if not ENV_NAME.fullmatch(value):
            raise ValueError("Invalid environment variable name")
        return value


class AccountAuth(StrictModel):
    mode: Literal["account"]
    store: NonEmpty
    key: NonEmpty


class CredentialStore(StrictModel):
    type: Literal["s3"] = "s3"
    bucket: NonEmpty
    region: NonEmpty = "us-east-1"
    endpoint_url: NonEmpty | None = None


class Profile(StrictModel):
    harness: Literal["codex"]
    model: NonEmpty | None = None
    instructions: str = ""
    auth: Annotated[APIKeyAuth | AccountAuth, Field(discriminator="mode")]


class Profiles(StrictModel):
    profiles: dict[str, Profile]
    credential_stores: dict[str, CredentialStore] = Field(default_factory=dict)

    @model_validator(mode="after")
    def references(self) -> Profiles:
        if not self.profiles or any(not name.strip() for name in self.profiles):
            raise ValueError("At least one named profile is required")
        for profile in self.profiles.values():
            if (
                isinstance(profile.auth, AccountAuth)
                and profile.auth.store not in self.credential_stores
            ):
                raise ValueError("Unknown credential store")
        return self

    @classmethod
    def load(cls, path: Path) -> Profiles:
        with path.open("rb") as file:
            return cls.model_validate(tomllib.load(file))


class AgentInput(StrictModel):
    profile: NonEmpty
    model: NonEmpty = Field(default_factory=lambda: "", validate_default=False)
    instructions: str = ""


class SandboxInput(StrictModel):
    template: NonEmpty
    env: dict[str, str] = Field(default_factory=dict)
    env_from: list[str] = Field(default_factory=list)

    @model_validator(mode="after")
    def environment(self) -> SandboxInput:
        names = [*self.env, *self.env_from]
        if len(names) != len(set(names)):
            raise ValueError("Duplicate environment variable")
        if any(not ENV_NAME.fullmatch(name) or name in RESERVED_ENV for name in names):
            raise ValueError("Invalid or reserved environment variable")
        if any("\0" in value for value in self.env.values()):
            raise ValueError("NUL is not allowed in environment variables")
        return self


class Limits(StrictModel):
    run_timeout_seconds: int = Field(default=3600, gt=0, le=2147483647)


class ConfigurationInput(StrictModel):
    agent: AgentInput
    sandbox: SandboxInput
    limits: Limits = Field(default_factory=Limits)


class AgentConfiguration(StrictModel):
    profile: str
    model: str
    instructions: str


class SandboxConfiguration(StrictModel):
    template: str
    env_names: list[str]
    env_from: list[str]


class Configuration(StrictModel):
    agent: AgentConfiguration
    sandbox: SandboxConfiguration
    limits: Limits


class Credentials(StrictModel):
    mode: Literal["api_key", "account"]
    api_key_env: str | None = None
    store: CredentialStore | None = None
    key: str | None = None


class ResolvedConfiguration(StrictModel):
    version: Literal[1] = 1
    harness: Literal["codex"] = "codex"
    public: Configuration
    credentials: Credentials


def resolve(
    request: ConfigurationInput, profiles: Profiles, allowlist: list[str]
) -> ResolvedConfiguration:
    from orpheus.errors import APIError

    profile = profiles.profiles.get(request.agent.profile)
    if profile is None:
        raise APIError(422, "unknown_profile", "Unknown agent profile.")
    if not set(request.sandbox.env_from) <= set(allowlist):
        raise APIError(422, "validation_error", "Environment reference is not allowed.")
    model = (
        request.agent.model
        if "model" in request.agent.model_fields_set
        else profile.model
    )
    if not model or not model.strip():
        raise APIError(422, "validation_error", "An explicit model is required.")
    instructions = (
        request.agent.instructions
        if "instructions" in request.agent.model_fields_set
        else profile.instructions
    )
    if isinstance(profile.auth, APIKeyAuth):
        credentials = Credentials(mode="api_key", api_key_env=profile.auth.api_key_env)
    else:
        credentials = Credentials(
            mode="account",
            store=profiles.credential_stores[profile.auth.store],
            key=profile.auth.key,
        )
    return ResolvedConfiguration(
        public=Configuration(
            agent=AgentConfiguration(
                profile=request.agent.profile, model=model, instructions=instructions
            ),
            sandbox=SandboxConfiguration(
                template=request.sandbox.template,
                env_names=sorted(request.sandbox.env),
                env_from=request.sandbox.env_from,
            ),
            limits=request.limits,
        ),
        credentials=credentials,
    )


class EnvironmentCipher:
    def __init__(self, key: SecretStr):
        self.fernet = Fernet(key.get_secret_value().encode())

    def encrypt(self, session_id: UUID, values: dict[str, str]) -> str | None:
        if not values:
            return None
        data = json.dumps({"session_id": str(session_id), "env": values}).encode()
        return "fernet-v1:" + self.fernet.encrypt(data).decode()

    def decrypt(self, session_id: UUID, ciphertext: str | None) -> dict[str, str]:
        if ciphertext is None:
            return {}
        version, token = ciphertext.split(":", 1)
        if version != "fernet-v1":
            raise ValueError("Unsupported ciphertext version")
        data = json.loads(self.fernet.decrypt(token.encode()))
        if data["session_id"] != str(session_id):
            raise ValueError("Ciphertext belongs to another session")
        return data["env"]

    def environment(
        self,
        session_id: UUID,
        ciphertext: str | None,
        config: Configuration,
        allowlist: list[str],
    ) -> dict[str, str]:
        values = self.decrypt(session_id, ciphertext)
        if not set(config.sandbox.env_from) <= set(allowlist):
            raise ValueError("Environment reference is no longer allowed")
        for name in config.sandbox.env_from:
            values[name] = os.environ[name]
        SandboxInput(template=config.sandbox.template, env=values)
        return values
