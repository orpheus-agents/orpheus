from datetime import datetime
from typing import Annotated, Any, Literal
from uuid import UUID

from pydantic import BaseModel, Field, field_validator

from orpheus.configuration import Configuration, ConfigurationInput, StrictModel
from orpheus.errors import Error

RunStatus = Literal[
    "accepted", "starting", "running", "cancelling", "completed", "failed", "cancelled"
]


class TextMessage(StrictModel):
    text: str = Field(min_length=1)

    @field_validator("text")
    @classmethod
    def nonblank(cls, value: str) -> str:
        if not value.strip() or "\0" in value:
            raise ValueError("A nonblank text without NUL is required")
        return value


class CreateSession(StrictModel):
    configuration: ConfigurationInput
    message: TextMessage


class SendMessage(StrictModel):
    message: TextMessage


class Accepted(BaseModel):
    session_id: UUID
    run_id: UUID
    message_id: UUID


class Position(BaseModel):
    run_number: int
    item_index: int


class Message(BaseModel):
    id: UUID
    session_id: UUID
    run_id: UUID
    role: Literal["user", "assistant"]
    kind: Literal["progress", "answer"] | None
    text: str
    delivery_status: (
        Literal["pending", "sending", "delivered", "uncertain", "rejected"] | None
    )
    error: Error | None
    registered_sequence: str
    position: Position | None
    created_at: datetime


class TextResult(BaseModel):
    type: Literal["text"] = "text"
    text: str
    exit_code: int | None = None
    original_bytes: int | None


class JSONResult(BaseModel):
    type: Literal["json"] = "json"
    value: Any
    original_bytes: int | None


class TruncatedResult(BaseModel):
    type: Literal["truncated_text"] = "truncated_text"
    head: str
    tail: str
    source_type: Literal["text", "json"] = "text"
    exit_code: int | None = None
    original_bytes: int | None


class ToolCall(BaseModel):
    id: UUID
    session_id: UUID
    run_id: UUID
    name: str
    input: Any
    status: Literal["running", "completed", "failed", "cancelled", "unknown"]
    result: (
        Annotated[
            TextResult | JSONResult | TruncatedResult, Field(discriminator="type")
        ]
        | None
    )
    output_completeness: Literal["complete", "truncated", "unavailable", "unknown"]
    truncation_reason: Literal["orpheus_limit", "harness_limit"] | None
    registered_sequence: str
    position: Position | None
    created_at: datetime


class Run(BaseModel):
    id: UUID
    session_id: UUID
    number: int
    status: RunStatus
    observation: Literal["attached", "reconnecting", "uncertain"] | None
    created_at: datetime
    execution_started_at: datetime | None
    deadline_at: datetime | None
    finished_at: datetime | None
    cancel_requested_at: datetime | None
    stop_reason: Literal["user_request", "run_timeout"] | None
    stop_method: Literal["graceful", "forced"] | None
    final_message: Message | None
    error: Error | None


class SandboxState(BaseModel):
    state: Literal[
        "not_created",
        "provisioning",
        "ready",
        "pausing",
        "paused",
        "resuming",
        "unavailable",
    ]
    last_known_state: str | None
    error: Error | None


class Session(BaseModel):
    id: UUID
    created_at: datetime
    configuration: Configuration
    sandbox: SandboxState
    active_run_id: UUID | None
    last_run_id: UUID
    status: RunStatus
    final_message: Message | None
    error: Error | None


class Cancelled(BaseModel):
    run_id: UUID
    status: RunStatus


class Page[T](BaseModel):
    items: list[T]
    next_cursor: str | None


class MessageItem(BaseModel):
    type: Literal["message"] = "message"
    message: Message


class ToolItem(BaseModel):
    type: Literal["tool_call"] = "tool_call"
    tool_call: ToolCall


class HistoryPage(Page[Annotated[MessageItem | ToolItem, Field(discriminator="type")]]):
    event_cursor: str


class Event(BaseModel):
    id: str
    session_id: UUID
    type: Literal[
        "run.updated", "message.updated", "tool_call.updated", "sandbox.updated"
    ]
    created_at: datetime
    data: Run | Message | ToolCall | SandboxState


class EventPage(BaseModel):
    items: list[Event]
    next_cursor: str
    has_more: bool
