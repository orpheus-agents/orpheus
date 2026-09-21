import asyncio
import json
from types import SimpleNamespace
from typing import cast
from unittest.mock import AsyncMock, Mock
from uuid import uuid4

import pytest
from agentbox import AsyncSandbox
from botocore.exceptions import ClientError

from orpheus.configuration import Credentials, CredentialStore
from orpheus.errors import ExecutionError
from orpheus.harnesses.codex.credentials import AccountCredentials


def auth(value="fixture"):
    return json.dumps(
        {
            "tokens": {
                "access_token": value,
                "refresh_token": "fixture",
                "id_token": "fixture",
            }
        }
    ).encode()


@pytest.fixture
def account(monkeypatch):
    s3 = Mock()
    s3.get_object.return_value = {
        "Body": SimpleNamespace(read=lambda _: auth(), close=lambda: None)
    }
    monkeypatch.setattr(
        "orpheus.harnesses.codex.credentials.boto3.client", lambda *a, **k: s3
    )
    sandbox = SimpleNamespace(
        files=SimpleNamespace(
            read=AsyncMock(return_value=auth()),
            write=AsyncMock(),
            watch_dir=AsyncMock(),
        ),
        commands=SimpleNamespace(run=AsyncMock()),
    )
    return AccountCredentials(
        cast(AsyncSandbox, sandbox),
        "/home/user/.orpheus-codex",
        Credentials(mode="account", store=CredentialStore(bucket="b"), key="auth.json"),
    ), s3


async def test_seed_watch_and_upload_rotated_credentials(account):
    manager, s3 = account
    await manager.seed()
    manager.sandbox.files.write.assert_awaited_once()
    assert "fixture" not in manager.sandbox.commands.run.call_args.args[0]
    await manager.watch()
    manager.sandbox.files.read.return_value = auth("rotated")
    await manager.sync()
    assert s3.put_object.call_args.kwargs["Body"] == auth("rotated")
    await manager.sync(force=True)
    s3.put_object.assert_called_once()


async def test_partial_file_retried_and_failure_does_not_block_pause(account):
    manager, s3 = account
    manager.sandbox.files.read.side_effect = [b"{", auth()]
    await manager.sync()
    assert manager.sandbox.files.read.await_count == 2
    s3.put_object.assert_called_once()
    manager.sandbox.files.read.side_effect = None
    manager.sandbox.files.read.return_value = auth("new")
    s3.put_object.side_effect = ClientError(
        {"Error": {"Code": "Unavailable"}}, "PutObject"
    )
    await manager.sync(force=True)  # Does not raise or create durable sync state.


async def test_reconnect_reads_current_file_without_watch_replay(account):
    manager, s3 = account
    await manager.watch()
    await manager.sync()
    s3.put_object.assert_called_once()


async def test_broken_seed_fails_preparation_and_never_writes(account):
    manager, s3 = account
    s3.get_object.return_value = {
        "Body": SimpleNamespace(read=lambda _: b"{}", close=lambda: None)
    }
    with pytest.raises(ExecutionError):
        await manager.seed()
    manager.sandbox.files.write.assert_not_awaited()


@pytest.mark.live
async def test_real_s3_and_native_auth_files(monkeypatch, required_env):
    import base64
    import time

    import boto3

    from orpheus.configuration import Credentials, CredentialStore
    from orpheus.harnesses.codex.credentials import AccountCredentials

    required_env("AGENTBOX_API_KEY")
    endpoint = required_env("TEST_S3_ENDPOINT")
    monkeypatch.setenv("AWS_ACCESS_KEY_ID", "orpheus-test")
    monkeypatch.setenv("AWS_SECRET_ACCESS_KEY", "orpheus-test-password")
    client = boto3.client("s3", endpoint_url=endpoint, region_name="us-east-1")
    bucket = "test-" + uuid4().hex
    await asyncio.to_thread(client.create_bucket, Bucket=bucket)

    def fixture(refresh):
        claims = {
            "email": "fixture@example.test",
            "sub": "fixture",
            "exp": int(time.time()) + 3600,
            "https://api.openai.com/auth": {
                "chatgpt_account_id": "fixture-account",
                "chatgpt_plan_type": "plus",
            },
        }
        part = (
            base64.urlsafe_b64encode(json.dumps(claims).encode()).decode().rstrip("=")
        )
        return json.dumps(
            {
                "auth_mode": "chatgpt",
                "OPENAI_API_KEY": None,
                "tokens": {
                    "access_token": "fixture-access",
                    "refresh_token": refresh,
                    "id_token": "eyJhbGciOiJub25lIn0." + part + ".fixture",
                },
                "last_refresh": "2026-09-21T00:00:00Z",
            }
        ).encode()

    first = fixture("first")
    await asyncio.to_thread(
        client.put_object, Bucket=bucket, Key="auth.json", Body=first
    )
    sandbox = await AsyncSandbox.create(
        template="codex",
        timeout=180,
        metadata={"purpose": "orpheus-rfc-0001-auth-acceptance"},
    )
    manager = None
    try:
        home = "/home/user/.orpheus-auth-test"
        await sandbox.commands.run("mkdir -m 700 " + home, user="user")
        credentials = Credentials(
            mode="account",
            store=CredentialStore(bucket=bucket, endpoint_url=endpoint),
            key="auth.json",
        )
        manager = AccountCredentials(sandbox, home, credentials)
        await manager.seed()
        await manager.watch()
        assert (
            bytes(
                await sandbox.files.read(
                    home + "/auth.json", format="bytes", user="user"
                )
            )
            == first
        )
        from orpheus.harnesses.codex.rpc import RPC

        await sandbox.files.write(
            home + "/config.toml",
            'cli_auth_credentials_store="file"\nforced_login_method="chatgpt"\n',
            user="user",
        )
        rpc = RPC(sandbox, 30)
        try:
            await rpc.launch({"CODEX_HOME": home}, "/home/user")
            await rpc.initialize()
            account = await rpc.call("account/read", {"refreshToken": False})
            assert account["account"]["type"] == "chatgpt"
        finally:
            if rpc.handle:
                await sandbox.commands.kill(rpc.handle.pid)
            await rpc.close()
        rotated = fixture("rotated")
        await sandbox.files.write(home + "/auth.json", rotated, user="user")
        async with asyncio.timeout(10):
            while not manager.dirty:
                await asyncio.sleep(0.05)
        await manager.sync()

        def stored():
            result = client.get_object(Bucket=bucket, Key="auth.json")
            try:
                return result["Body"].read()
            finally:
                result["Body"].close()

        assert await asyncio.to_thread(stored) == rotated
        await manager.close()
        manager = None
        # Updates while disconnected are recovered by the next explicit read.
        latest = fixture("disconnected")
        await sandbox.files.write(home + "/auth.json", latest, user="user")
        await sandbox.pause(keep_memory=True)
        sandbox = await AsyncSandbox.connect(sandbox.sandbox_id)
        manager = AccountCredentials(sandbox, home, credentials)
        await manager.watch()
        await manager.sync()
        assert await asyncio.to_thread(stored) == latest
        # Manual repair: fetch the replacement object rather than the broken local file.
        repaired = fixture("repaired")
        await asyncio.to_thread(
            client.put_object, Bucket=bucket, Key="auth.json", Body=repaired
        )
        await manager.seed()
        assert (
            bytes(
                await sandbox.files.read(
                    home + "/auth.json", format="bytes", user="user"
                )
            )
            == repaired
        )
    finally:
        if manager:
            await manager.close()
        await sandbox.kill()
        await asyncio.to_thread(client.delete_object, Bucket=bucket, Key="auth.json")
        await asyncio.to_thread(client.delete_bucket, Bucket=bucket)
        client.close()


async def test_watch_is_reused_and_stale_exit_cannot_clear_replacement(account):
    manager, _ = account
    await manager.watch()
    first = manager.watcher
    exited = manager.sandbox.files.watch_dir.call_args.kwargs["on_exit"]
    await manager.watch()
    manager.sandbox.files.watch_dir.assert_awaited_once()
    await manager.close()
    assert manager.watcher is None
    first.stop.assert_awaited_once()
    second = SimpleNamespace(stop=AsyncMock())
    manager.sandbox.files.watch_dir.return_value = second
    await manager.watch()
    await exited(None)
    assert manager.watcher is second
    await manager.close()
    second.stop.assert_awaited_once()


async def test_watch_that_exits_before_connect_returns_is_not_retained(account):
    manager, _ = account
    watcher = SimpleNamespace(stop=AsyncMock())

    async def connect(*args, **kwargs):
        await kwargs["on_exit"](None)
        return watcher

    manager.sandbox.files.watch_dir.side_effect = connect
    await manager.watch()
    assert manager.watcher is None
    watcher.stop.assert_awaited_once()
