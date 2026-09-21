"""Durable state. Every mutation is serialized on its parent session."""

from datetime import UTC, datetime
from typing import Any
from uuid import UUID, uuid4

from sqlalchemy import (
    BigInteger,
    Boolean,
    CheckConstraint,
    DateTime,
    ForeignKey,
    ForeignKeyConstraint,
    Index,
    Integer,
    String,
    Text,
    UniqueConstraint,
    text,
)
from sqlalchemy.dialects.postgresql import JSONB
from sqlalchemy.orm import Mapped, mapped_column

from orpheus.database import Base

ACTIVE = ("accepted", "starting", "running", "cancelling")
TERMINAL = ("completed", "failed", "cancelled")


def now() -> datetime:
    return datetime.now(UTC)


class SessionRecord(Base):
    __tablename__ = "sessions"
    id: Mapped[UUID] = mapped_column(primary_key=True, default=uuid4)
    created_at: Mapped[datetime] = mapped_column(DateTime(timezone=True), default=now)
    configuration: Mapped[dict[str, Any]] = mapped_column(JSONB)
    env_ciphertext: Mapped[str | None] = mapped_column(Text)
    sandbox_state: Mapped[str] = mapped_column(default="not_created")
    sandbox_last_known_state: Mapped[str | None]
    sandbox_error: Mapped[dict[str, Any] | None] = mapped_column(JSONB)
    sandbox_id: Mapped[str | None]
    process_id: Mapped[int | None]
    launch_id: Mapped[UUID | None]
    thread_id: Mapped[str | None]
    history_path: Mapped[str | None]
    history_offset: Mapped[int] = mapped_column(BigInteger, default=0)
    workspace: Mapped[str | None]
    harness_home: Mapped[str | None]
    slot_reserved: Mapped[bool] = mapped_column(Boolean, default=True)
    next_run_number: Mapped[int] = mapped_column(default=1)
    next_event_sequence: Mapped[int] = mapped_column(BigInteger, default=1)
    __table_args__ = (
        CheckConstraint(
            "sandbox_state IN ('not_created','provisioning','ready','pausing','paused','resuming','unavailable')",
            name="session_sandbox_state",
        ),
        Index("sessions_reserved", "id", postgresql_where=text("slot_reserved")),
    )


class RunRecord(Base):
    __tablename__ = "runs"
    id: Mapped[UUID] = mapped_column(primary_key=True, default=uuid4)
    session_id: Mapped[UUID] = mapped_column(
        ForeignKey("sessions.id", ondelete="CASCADE")
    )
    number: Mapped[int]
    status: Mapped[str] = mapped_column(default="accepted")
    observation: Mapped[str | None]
    created_at: Mapped[datetime] = mapped_column(DateTime(timezone=True), default=now)
    execution_started_at: Mapped[datetime | None] = mapped_column(
        DateTime(timezone=True)
    )
    deadline_at: Mapped[datetime | None] = mapped_column(DateTime(timezone=True))
    finished_at: Mapped[datetime | None] = mapped_column(DateTime(timezone=True))
    cancel_requested_at: Mapped[datetime | None] = mapped_column(
        DateTime(timezone=True)
    )
    cancel_attempted_at: Mapped[datetime | None] = mapped_column(
        DateTime(timezone=True)
    )
    stop_reason: Mapped[str | None]
    stop_method: Mapped[str | None]
    error: Mapped[dict[str, Any] | None] = mapped_column(JSONB)
    native_turn_id: Mapped[str | None]
    next_delivery_number: Mapped[int] = mapped_column(default=1)
    final_message_id: Mapped[UUID | None]
    __table_args__ = (
        UniqueConstraint("session_id", "number"),
        UniqueConstraint("session_id", "id"),
        Index(
            "runs_one_unfinished_per_session",
            "session_id",
            unique=True,
            postgresql_where=text(
                "status IN ('accepted','starting','running','cancelling')"
            ),
        ),
        CheckConstraint(
            "status IN ('accepted','starting','running','cancelling','completed','failed','cancelled')",
            name="run_status",
        ),
        CheckConstraint(
            "observation IS NULL OR observation IN ('attached','reconnecting','uncertain')",
            name="run_observation",
        ),
        CheckConstraint(
            "stop_reason IS NULL OR stop_reason IN ('user_request','run_timeout')",
            name="run_stop_reason",
        ),
        CheckConstraint(
            "stop_method IS NULL OR stop_method IN ('graceful','forced')",
            name="run_stop_method",
        ),
        ForeignKeyConstraint(
            ["id", "final_message_id"],
            ["messages.run_id", "messages.id"],
            name="run_final_message",
            use_alter=True,
        ),
    )


class MessageRecord(Base):
    __tablename__ = "messages"
    id: Mapped[UUID] = mapped_column(primary_key=True, default=uuid4)
    session_id: Mapped[UUID]
    run_id: Mapped[UUID]
    role: Mapped[str]
    kind: Mapped[str | None]
    text: Mapped[str] = mapped_column(Text)
    delivery_status: Mapped[str | None]
    delivery_number: Mapped[int | None]
    error: Mapped[dict[str, Any] | None] = mapped_column(JSONB)
    native_key: Mapped[str | None]
    registered_sequence: Mapped[int] = mapped_column(BigInteger)
    position: Mapped[dict[str, int] | None] = mapped_column(JSONB)
    created_at: Mapped[datetime] = mapped_column(DateTime(timezone=True), default=now)
    __table_args__ = (
        ForeignKeyConstraint(
            ["session_id", "run_id"], ["runs.session_id", "runs.id"], ondelete="CASCADE"
        ),
        UniqueConstraint("session_id", "native_key"),
        UniqueConstraint("run_id", "delivery_number"),
        UniqueConstraint("run_id", "id"),
        CheckConstraint(
            "(role='assistant' AND kind IN ('progress','answer') AND delivery_status IS NULL) OR (role='user' AND kind IS NULL AND delivery_status IN ('pending','sending','delivered','uncertain','rejected'))",
            name="message_role_delivery",
        ),
    )


class ToolCallRecord(Base):
    __tablename__ = "tool_calls"
    id: Mapped[UUID] = mapped_column(primary_key=True, default=uuid4)
    session_id: Mapped[UUID]
    run_id: Mapped[UUID]
    name: Mapped[str]
    input: Mapped[Any] = mapped_column(JSONB)
    status: Mapped[str]
    result: Mapped[dict[str, Any] | None] = mapped_column(JSONB)
    output_completeness: Mapped[str]
    truncation_reason: Mapped[str | None]
    native_key: Mapped[str | None]
    registered_sequence: Mapped[int] = mapped_column(BigInteger)
    position: Mapped[dict[str, int] | None] = mapped_column(JSONB)
    created_at: Mapped[datetime] = mapped_column(DateTime(timezone=True), default=now)
    __table_args__ = (
        ForeignKeyConstraint(
            ["session_id", "run_id"], ["runs.session_id", "runs.id"], ondelete="CASCADE"
        ),
        UniqueConstraint("session_id", "native_key"),
        CheckConstraint(
            "status IN ('running','completed','failed','cancelled','unknown')",
            name="tool_status",
        ),
        CheckConstraint(
            "output_completeness IN ('complete','truncated','unavailable','unknown')",
            name="tool_completeness",
        ),
        CheckConstraint(
            "truncation_reason IS NULL OR truncation_reason IN ('orpheus_limit','harness_limit')",
            name="tool_truncation",
        ),
    )


class EventRecord(Base):
    __tablename__ = "session_events"
    session_id: Mapped[UUID] = mapped_column(
        ForeignKey("sessions.id", ondelete="CASCADE"), primary_key=True
    )
    sequence: Mapped[int] = mapped_column(BigInteger, primary_key=True)
    type: Mapped[str]
    created_at: Mapped[datetime] = mapped_column(DateTime(timezone=True), default=now)
    data: Mapped[dict[str, Any]] = mapped_column(JSONB)


class OperationRecord(Base):
    __tablename__ = "operations"
    id: Mapped[UUID] = mapped_column(primary_key=True, default=uuid4)
    session_id: Mapped[UUID] = mapped_column(
        ForeignKey("sessions.id", ondelete="CASCADE")
    )
    run_id: Mapped[UUID | None]
    message_id: Mapped[UUID | None] = mapped_column(ForeignKey("messages.id"))
    kind: Mapped[str]
    status: Mapped[str] = mapped_column(default="pending")
    parameters: Mapped[dict[str, Any]] = mapped_column(JSONB, default=dict)
    result: Mapped[dict[str, Any] | None] = mapped_column(JSONB)
    attempted_at: Mapped[datetime | None] = mapped_column(DateTime(timezone=True))
    created_at: Mapped[datetime] = mapped_column(DateTime(timezone=True), default=now)
    __table_args__ = (
        ForeignKeyConstraint(["session_id", "run_id"], ["runs.session_id", "runs.id"]),
        CheckConstraint(
            "status IN ('pending','sending','confirmed','failed','uncertain')",
            name="operation_status",
        ),
    )


class IdempotencyRecord(Base):
    __tablename__ = "idempotency_keys"
    operation: Mapped[str] = mapped_column(String(32), primary_key=True)
    resource: Mapped[str] = mapped_column(String(100), primary_key=True)
    key: Mapped[UUID] = mapped_column(primary_key=True)
    fingerprint: Mapped[str]
    version: Mapped[int] = mapped_column(Integer, default=1)
    session_id: Mapped[UUID] = mapped_column(ForeignKey("sessions.id"))
    run_id: Mapped[UUID] = mapped_column(ForeignKey("runs.id"))
    message_id: Mapped[UUID] = mapped_column(ForeignKey("messages.id"))
