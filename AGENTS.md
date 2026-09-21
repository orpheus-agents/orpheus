# AGENTS.md

Read [README.md](README.md).

# Environment
* Python 3.14.7, `uv`
* Lint and Type Checking – `ruff`, `ty`
* CLI configuration - `click`
* CLI output - `rich`

# General rules
* Do not preserve backward compatibility.
* Choose the simplest implementation that fully meets the current requirements.
* Prefer established, well-maintained libraries over custom implementations.
* Fix the cause, not the symptom.
* Suggest best practices, even if they may require refactoring.

# Development workflow
* Always write a comprehensive test suite covering the implementation alongside the implementation itself.
* Run `make openapi` to update `openapi.json` whenever the API changes.
* Run `make lint test` after completing the implementation.
