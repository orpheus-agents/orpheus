"""Agent sessions and execution journal

Revision ID: 48915795c0e0
Revises:
Create Date: 2026-09-21 17:31:12.903110
"""

from collections.abc import Sequence

import sqlalchemy as sa
from sqlalchemy.dialects import postgresql

from alembic import op

revision: str = "48915795c0e0"
down_revision: str | Sequence[str] | None = None
branch_labels: str | Sequence[str] | None = None
depends_on: str | Sequence[str] | None = None


def upgrade() -> None:
    op.create_table(
        "sessions",
        sa.Column("id", sa.Uuid(), nullable=False),
        sa.Column("created_at", sa.DateTime(timezone=True), nullable=False),
        sa.Column(
            "configuration", postgresql.JSONB(astext_type=sa.Text()), nullable=False
        ),
        sa.Column("env_ciphertext", sa.Text(), nullable=True),
        sa.Column("sandbox_state", sa.String(), nullable=False),
        sa.Column("sandbox_last_known_state", sa.String(), nullable=True),
        sa.Column(
            "sandbox_error", postgresql.JSONB(astext_type=sa.Text()), nullable=True
        ),
        sa.Column("sandbox_id", sa.String(), nullable=True),
        sa.Column("process_id", sa.Integer(), nullable=True),
        sa.Column("launch_id", sa.Uuid(), nullable=True),
        sa.Column("thread_id", sa.String(), nullable=True),
        sa.Column("history_path", sa.String(), nullable=True),
        sa.Column("history_offset", sa.BigInteger(), nullable=False),
        sa.Column("workspace", sa.String(), nullable=True),
        sa.Column("harness_home", sa.String(), nullable=True),
        sa.Column("slot_reserved", sa.Boolean(), nullable=False),
        sa.Column("next_run_number", sa.Integer(), nullable=False),
        sa.Column("next_event_sequence", sa.BigInteger(), nullable=False),
        sa.CheckConstraint(
            "sandbox_state IN ('not_created','provisioning','ready','pausing','paused','resuming','unavailable')",
            name="session_sandbox_state",
        ),
        sa.PrimaryKeyConstraint("id"),
    )
    op.create_index(
        "sessions_reserved",
        "sessions",
        ["id"],
        unique=False,
        postgresql_where=sa.text("slot_reserved"),
    )
    op.create_table(
        "runs",
        sa.Column("id", sa.Uuid(), nullable=False),
        sa.Column("session_id", sa.Uuid(), nullable=False),
        sa.Column("number", sa.Integer(), nullable=False),
        sa.Column("status", sa.String(), nullable=False),
        sa.Column("observation", sa.String(), nullable=True),
        sa.Column("created_at", sa.DateTime(timezone=True), nullable=False),
        sa.Column("execution_started_at", sa.DateTime(timezone=True), nullable=True),
        sa.Column("deadline_at", sa.DateTime(timezone=True), nullable=True),
        sa.Column("finished_at", sa.DateTime(timezone=True), nullable=True),
        sa.Column("cancel_requested_at", sa.DateTime(timezone=True), nullable=True),
        sa.Column("cancel_attempted_at", sa.DateTime(timezone=True), nullable=True),
        sa.Column("stop_reason", sa.String(), nullable=True),
        sa.Column("stop_method", sa.String(), nullable=True),
        sa.Column("error", postgresql.JSONB(astext_type=sa.Text()), nullable=True),
        sa.Column("native_turn_id", sa.String(), nullable=True),
        sa.Column("next_delivery_number", sa.Integer(), nullable=False),
        sa.Column("final_message_id", sa.Uuid(), nullable=True),
        sa.CheckConstraint(
            "observation IS NULL OR observation IN ('attached','reconnecting','uncertain')",
            name="run_observation",
        ),
        sa.CheckConstraint(
            "status IN ('accepted','starting','running','cancelling','completed','failed','cancelled')",
            name="run_status",
        ),
        sa.CheckConstraint(
            "stop_method IS NULL OR stop_method IN ('graceful','forced')",
            name="run_stop_method",
        ),
        sa.CheckConstraint(
            "stop_reason IS NULL OR stop_reason IN ('user_request','run_timeout')",
            name="run_stop_reason",
        ),
        sa.ForeignKeyConstraint(["session_id"], ["sessions.id"], ondelete="CASCADE"),
        sa.PrimaryKeyConstraint("id"),
        sa.UniqueConstraint("session_id", "id"),
        sa.UniqueConstraint("session_id", "number"),
    )
    op.create_index(
        "runs_one_unfinished_per_session",
        "runs",
        ["session_id"],
        unique=True,
        postgresql_where=sa.text(
            "status IN ('accepted','starting','running','cancelling')"
        ),
    )
    op.create_table(
        "session_events",
        sa.Column("session_id", sa.Uuid(), nullable=False),
        sa.Column("sequence", sa.BigInteger(), nullable=False),
        sa.Column("type", sa.String(), nullable=False),
        sa.Column("created_at", sa.DateTime(timezone=True), nullable=False),
        sa.Column("data", postgresql.JSONB(astext_type=sa.Text()), nullable=False),
        sa.ForeignKeyConstraint(["session_id"], ["sessions.id"], ondelete="CASCADE"),
        sa.PrimaryKeyConstraint("session_id", "sequence"),
    )
    op.create_table(
        "messages",
        sa.Column("id", sa.Uuid(), nullable=False),
        sa.Column("session_id", sa.Uuid(), nullable=False),
        sa.Column("run_id", sa.Uuid(), nullable=False),
        sa.Column("role", sa.String(), nullable=False),
        sa.Column("kind", sa.String(), nullable=True),
        sa.Column("text", sa.Text(), nullable=False),
        sa.Column("delivery_status", sa.String(), nullable=True),
        sa.Column("delivery_number", sa.Integer(), nullable=True),
        sa.Column("error", postgresql.JSONB(astext_type=sa.Text()), nullable=True),
        sa.Column("native_key", sa.String(), nullable=True),
        sa.Column("registered_sequence", sa.BigInteger(), nullable=False),
        sa.Column("position", postgresql.JSONB(astext_type=sa.Text()), nullable=True),
        sa.Column("created_at", sa.DateTime(timezone=True), nullable=False),
        sa.CheckConstraint(
            "(role='assistant' AND kind IN ('progress','answer') AND delivery_status IS NULL) OR (role='user' AND kind IS NULL AND delivery_status IN ('pending','sending','delivered','uncertain','rejected'))",
            name="message_role_delivery",
        ),
        sa.ForeignKeyConstraint(
            ["session_id", "run_id"], ["runs.session_id", "runs.id"], ondelete="CASCADE"
        ),
        sa.PrimaryKeyConstraint("id"),
        sa.UniqueConstraint("run_id", "delivery_number"),
        sa.UniqueConstraint("run_id", "id"),
        sa.UniqueConstraint("session_id", "native_key"),
    )
    op.create_table(
        "tool_calls",
        sa.Column("id", sa.Uuid(), nullable=False),
        sa.Column("session_id", sa.Uuid(), nullable=False),
        sa.Column("run_id", sa.Uuid(), nullable=False),
        sa.Column("name", sa.String(), nullable=False),
        sa.Column("input", postgresql.JSONB(astext_type=sa.Text()), nullable=False),
        sa.Column("status", sa.String(), nullable=False),
        sa.Column("result", postgresql.JSONB(astext_type=sa.Text()), nullable=True),
        sa.Column("output_completeness", sa.String(), nullable=False),
        sa.Column("truncation_reason", sa.String(), nullable=True),
        sa.Column("native_key", sa.String(), nullable=True),
        sa.Column("registered_sequence", sa.BigInteger(), nullable=False),
        sa.Column("position", postgresql.JSONB(astext_type=sa.Text()), nullable=True),
        sa.Column("created_at", sa.DateTime(timezone=True), nullable=False),
        sa.CheckConstraint(
            "output_completeness IN ('complete','truncated','unavailable','unknown')",
            name="tool_completeness",
        ),
        sa.CheckConstraint(
            "status IN ('running','completed','failed','cancelled','unknown')",
            name="tool_status",
        ),
        sa.CheckConstraint(
            "truncation_reason IS NULL OR truncation_reason IN ('orpheus_limit','harness_limit')",
            name="tool_truncation",
        ),
        sa.ForeignKeyConstraint(
            ["session_id", "run_id"], ["runs.session_id", "runs.id"], ondelete="CASCADE"
        ),
        sa.PrimaryKeyConstraint("id"),
        sa.UniqueConstraint("session_id", "native_key"),
    )
    op.create_table(
        "idempotency_keys",
        sa.Column("operation", sa.String(length=32), nullable=False),
        sa.Column("resource", sa.String(length=100), nullable=False),
        sa.Column("key", sa.Uuid(), nullable=False),
        sa.Column("fingerprint", sa.String(), nullable=False),
        sa.Column("version", sa.Integer(), nullable=False),
        sa.Column("session_id", sa.Uuid(), nullable=False),
        sa.Column("run_id", sa.Uuid(), nullable=False),
        sa.Column("message_id", sa.Uuid(), nullable=False),
        sa.ForeignKeyConstraint(
            ["message_id"],
            ["messages.id"],
        ),
        sa.ForeignKeyConstraint(
            ["run_id"],
            ["runs.id"],
        ),
        sa.ForeignKeyConstraint(
            ["session_id"],
            ["sessions.id"],
        ),
        sa.PrimaryKeyConstraint("operation", "resource", "key"),
    )
    op.create_table(
        "operations",
        sa.Column("id", sa.Uuid(), nullable=False),
        sa.Column("session_id", sa.Uuid(), nullable=False),
        sa.Column("run_id", sa.Uuid(), nullable=True),
        sa.Column("message_id", sa.Uuid(), nullable=True),
        sa.Column("kind", sa.String(), nullable=False),
        sa.Column("status", sa.String(), nullable=False),
        sa.Column(
            "parameters", postgresql.JSONB(astext_type=sa.Text()), nullable=False
        ),
        sa.Column("result", postgresql.JSONB(astext_type=sa.Text()), nullable=True),
        sa.Column("attempted_at", sa.DateTime(timezone=True), nullable=True),
        sa.Column("created_at", sa.DateTime(timezone=True), nullable=False),
        sa.CheckConstraint(
            "status IN ('pending','sending','confirmed','failed','uncertain')",
            name="operation_status",
        ),
        sa.ForeignKeyConstraint(
            ["message_id"],
            ["messages.id"],
        ),
        sa.ForeignKeyConstraint(
            ["session_id", "run_id"],
            ["runs.session_id", "runs.id"],
        ),
        sa.ForeignKeyConstraint(["session_id"], ["sessions.id"], ondelete="CASCADE"),
        sa.PrimaryKeyConstraint("id"),
    )
    op.create_foreign_key(
        "run_final_message",
        "runs",
        "messages",
        ["id", "final_message_id"],
        ["run_id", "id"],
    )


def downgrade() -> None:
    op.drop_constraint("run_final_message", "runs", type_="foreignkey")
    op.drop_table("operations")
    op.drop_table("idempotency_keys")
    op.drop_table("tool_calls")
    op.drop_table("messages")
    op.drop_table("session_events")
    op.drop_index(
        "runs_one_unfinished_per_session",
        table_name="runs",
        postgresql_where=sa.text(
            "status IN ('accepted','starting','running','cancelling')"
        ),
    )
    op.drop_table("runs")
    op.drop_index(
        "sessions_reserved",
        table_name="sessions",
        postgresql_where=sa.text("slot_reserved"),
    )
    op.drop_table("sessions")
