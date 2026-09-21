import json
from uuid import uuid4

import pytest
from cryptography.fernet import Fernet, InvalidToken
from pydantic import SecretStr, ValidationError

from orpheus.configuration import (
    AgentInput,
    ConfigurationInput,
    EnvironmentCipher,
    Profiles,
    SandboxInput,
    resolve,
)
from orpheus.errors import APIError
from orpheus.harnesses.codex.credentials import validate_auth
from orpheus.settings import Settings


def test_runtime_requires_persistent_secrets(settings: Settings):
    settings.public_api_keys = []
    with pytest.raises(ValueError):
        settings.runtime(api=True)
    settings.env_encryption_key = SecretStr("bad")
    with pytest.raises(ValueError):
        settings.runtime()


def test_encrypted_environment_survives_restart_and_binds_session(settings: Settings):
    sid = uuid4()
    cipher = EnvironmentCipher(settings.env_encryption_key)
    token = cipher.encrypt(sid, {"TOKEN": "secret-value"})
    assert token is not None
    assert "secret-value" not in token
    assert token != cipher.encrypt(sid, {"TOKEN": "secret-value"})
    assert EnvironmentCipher(settings.env_encryption_key).decrypt(sid, token) == {
        "TOKEN": "secret-value"
    }
    with pytest.raises(ValueError):
        cipher.decrypt(uuid4(), token)
    with pytest.raises(InvalidToken):
        EnvironmentCipher(SecretStr(Fernet.generate_key().decode())).decrypt(sid, token)


@pytest.mark.parametrize(
    "body",
    [
        {"workdir": "/tmp"},
        {"env": {"ALL_PROXY": "http://example.com"}},
        {"env_from": ["HTTPS_PROXY"]},
        {"env": {"HOME": "/tmp"}},
        {"env": {"CODEX_HOME": "/tmp"}},
        {"env": {"OPENAI_API_KEY": "key"}},
        {"env": {"1BAD": "a"}},
        {"env": {"A": "\0"}},
        {"env": {"A": "x"}, "env_from": ["A"]},
        {"env_from": ["A", "A"]},
        {"env_from": ["*"]},
        {"env": {"A": 1}},
        {"env": None},
    ],
)
def test_invalid_sandbox_configuration(body):
    with pytest.raises(ValidationError):
        SandboxInput.model_validate({"template": "codex", **body})


def test_profile_resolution_freezes_values(settings: Settings):
    profiles, _ = settings.runtime()
    request = ConfigurationInput(
        agent=AgentInput(profile="default", instructions=""),
        sandbox=SandboxInput(
            template="codex", env={"TOKEN": "secret"}, env_from=["GITHUB_TOKEN"]
        ),
    )
    result = resolve(request, profiles, settings.harness_env_allowlist)
    profiles.profiles["default"].model = "changed"
    assert result.public.agent.model == "fixture-model"
    assert result.public.sandbox.env_names == ["TOKEN"]
    assert "secret" not in result.model_dump_json()
    with pytest.raises(APIError):
        resolve(request, profiles, [])


def test_env_references_read_only_on_process_launch(settings: Settings, monkeypatch):
    profiles, cipher = settings.runtime()
    config = resolve(
        ConfigurationInput(
            agent=AgentInput(profile="default"),
            sandbox=SandboxInput(template="codex", env_from=["GITHUB_TOKEN"]),
        ),
        profiles,
        settings.harness_env_allowlist,
    ).public
    sid = uuid4()
    monkeypatch.setenv("GITHUB_TOKEN", "a")
    first = cipher.environment(sid, None, config, settings.harness_env_allowlist)
    monkeypatch.setenv("GITHUB_TOKEN", "b")
    assert first == {"GITHUB_TOKEN": "a"}
    assert cipher.environment(sid, None, config, settings.harness_env_allowlist) == {
        "GITHUB_TOKEN": "b"
    }


def test_account_profile_resolves_store():
    profiles = Profiles.model_validate(
        {
            "credential_stores": {"s": {"bucket": "b"}},
            "profiles": {
                "account": {
                    "harness": "codex",
                    "model": "m",
                    "auth": {"mode": "account", "store": "s", "key": "auth.json"},
                }
            },
        }
    )
    config = resolve(
        ConfigurationInput(
            agent=AgentInput(profile="account"), sandbox=SandboxInput(template="codex")
        ),
        profiles,
        [],
    )
    assert config.credentials.store is not None
    assert config.credentials.store.bucket == "b"
    assert "credentials" not in config.public.model_dump()
    with pytest.raises(ValidationError):
        Profiles.model_validate(
            {
                "profiles": {
                    "p": {
                        "harness": "codex",
                        "auth": {"mode": "account", "store": "missing", "key": "a"},
                    }
                }
            }
        )


@pytest.mark.parametrize(
    "raw", [b"", b"{", b"{}", b'{"tokens":{}}', b'{"OPENAI_API_KEY":"x"}']
)
def test_invalid_account_file(raw):
    with pytest.raises(ValueError):
        validate_auth(raw)


def test_account_file():
    raw = json.dumps(
        {
            "tokens": {
                "access_token": "fixture",
                "refresh_token": "fixture",
                "id_token": "fixture",
            }
        }
    ).encode()
    assert validate_auth(raw) == raw
