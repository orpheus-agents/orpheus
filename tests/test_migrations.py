import asyncio
import os
import subprocess

from orpheus.database import Base


async def test_migration_upgrade_downgrade_and_model_consistency(database, settings):
    engine = database.kw["bind"]
    async with engine.begin() as connection:
        await connection.run_sync(Base.metadata.drop_all)
    env = {**os.environ, "DATABASE_URL": settings.database_url}
    for args in (
        ("upgrade", "head"),
        ("check",),
        ("downgrade", "base"),
        ("upgrade", "head"),
        ("check",),
    ):
        result = await asyncio.to_thread(
            subprocess.run,
            ["alembic", *args],
            env=env,
            capture_output=True,
            text=True,
            check=False,
        )
        assert result.returncode == 0, str(result.stdout) + str(result.stderr)
