from typing import Literal

from pydantic import BaseModel, Field


class ErrorDetail(BaseModel):
    path: list[str | int]
    code: str


class Error(BaseModel):
    code: str
    message: str
    phase: Literal["preparation", "execution", "recovery"] | None = None
    details: list[ErrorDetail] = Field(default_factory=list)


class ErrorResponse(BaseModel):
    error: Error


class APIError(Exception):
    def __init__(self, status: int, code: str, message: str):
        self.status = status
        self.error = Error(code=code, message=message)
        super().__init__(code)


class ExecutionError(Exception):
    """A confirmed failure safe to publish, never a raw provider exception."""

    def __init__(self, code: str, message: str):
        self.code = code
        self.message = message
        super().__init__(code)


class UncertainError(Exception):
    """An external operation may have succeeded; reconcile instead of retrying."""
