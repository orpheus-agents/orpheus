import json
from pathlib import Path

import click
import uvicorn
from rich.console import Console

from orpheus import __version__
from orpheus.app import create_app

console = Console(stderr=True)


@click.group()
@click.version_option(version=__version__)
def cli() -> None:
    """Orpheus application commands."""


@cli.command()
@click.option("--host", default="0.0.0.0", envvar="ORPHEUS_HOST", show_default=True)
@click.option(
    "--port",
    default=8000,
    type=click.IntRange(1, 65535),
    envvar="ORPHEUS_PORT",
    show_default=True,
)
@click.option("--reload/--no-reload", default=False, envvar="ORPHEUS_RELOAD")
def serve(host: str, port: int, reload: bool) -> None:
    """Start the HTTP service."""
    console.print(f"Starting [bold]Orpheus[/bold] on {host}:{port}", markup=True)
    uvicorn.run(
        "orpheus.app:create_app", factory=True, host=host, port=port, reload=reload
    )


@cli.command()
@click.option(
    "--output",
    default="openapi.json",
    type=click.Path(dir_okay=False, path_type=Path),
    show_default=True,
)
def openapi(output: Path) -> None:
    """Export the OpenAPI schema without starting the service or connecting to a DB."""
    schema = create_app().openapi()
    try:
        output.write_text(
            json.dumps(schema, ensure_ascii=False, indent=2, sort_keys=True) + "\n",
            encoding="utf-8",
        )
    except OSError as error:
        raise click.ClickException(str(error)) from error
    console.print("OpenAPI schema written to", str(output), markup=False)
