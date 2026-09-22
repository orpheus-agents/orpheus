# AGENTS.md

Read [README.md](README.md).

# Environment
* Go 1.27

# General rules
* Do not preserve backward compatibility.
* Choose the simplest implementation that fully meets the current requirements.
* Prefer established, well-maintained libraries over custom implementations.
* Fix the cause, not the symptom.
* Suggest best practices, even if they may require refactoring.

# Development workflow
* Always write a comprehensive test suite covering the implementation alongside the implementation itself.
* API-first workflow: spec, `make generate`, implementation, tests.
* Run `make fix gofix check` after completing the implementation.

# Database and migrations
* Use sqlc for database queries and Goose for migrations.
* Edit SQL in `internal/store/queries/`, then run `make generate`; do not edit `internal/store/db/` by hand.
* Keep data changes and DDL operations in separate migrations.
* Test new migrations in both directions against the test database.

# Docs
* Follow the principles of Maxim Ilyakhov's "Write, Cut": be concise without losing substance.
* Prefer clear structure, diagrams, and lists over long blocks of text.
