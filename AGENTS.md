# AGENTS.md

Read [README.md](README.md).

# Environment

* Python 3.14.7, `uv`
* Lint and Type Checking – `ruff`, `ty`
* CLI configuration - `click`
* CLI output - `rich`

# Development workflow

* Always write a comprehensive test suite covering the implementation alongside the implementation itself.
* Run `make openapi` to update `openapi.json` whenever the API changes.
* Run `make lint test` after completing the implementation.
