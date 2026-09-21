import json
from pathlib import Path
from unittest.mock import Mock

import pytest
from click.testing import CliRunner

from orpheus.cli import cli


def test_export_does_not_require_settings_or_database(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    monkeypatch.chdir(tmp_path)
    monkeypatch.delenv("DATABASE_URL", raising=False)
    engine = Mock(
        side_effect=AssertionError("Export must not create a database engine")
    )
    monkeypatch.setattr("orpheus.app.create_async_engine", engine)
    runner = CliRunner()

    result = runner.invoke(cli, ["openapi"])
    assert result.exit_code == 0, result.output
    first = Path("openapi.json").read_bytes()
    assert {"/health", "/ready", "/api/v1/sessions"} <= set(json.loads(first)["paths"])

    assert runner.invoke(cli, ["openapi"]).exit_code == 0
    assert Path("openapi.json").read_bytes() == first
    engine.assert_not_called()


def test_serve_options(monkeypatch: pytest.MonkeyPatch) -> None:
    run = Mock()
    monkeypatch.setattr("orpheus.cli.uvicorn.run", run)
    result = CliRunner().invoke(
        cli, ["serve", "--host", "127.0.0.1", "--port", "9000", "--reload"]
    )
    assert result.exit_code == 0, result.output
    run.assert_called_once_with(
        "orpheus.app:create_app", factory=True, host="127.0.0.1", port=9000, reload=True
    )


def test_export_reports_write_failure(tmp_path: Path) -> None:
    result = CliRunner().invoke(
        cli, ["openapi", "--output", str(tmp_path / "missing" / "schema.json")]
    )
    assert result.exit_code == 1
    assert "Error:" in result.output
