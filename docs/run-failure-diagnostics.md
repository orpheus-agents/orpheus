# Run failure diagnostics

Worker failures emit a flat JSON `execution.failure` event to the existing log
output. A UUID `diagnostic_id` links the event to the public error returned by
the API. The optional field is stored in existing error JSON and API events;
older errors and errors outside the worker handlers may omit it.

Confirmed failures use ERROR; interrupted observation or uncertain delivery uses
WARN. An event records an observed failure, not a successful database commit.
If saving the public error fails, the log can still exist. Log delivery is best
effort and uses the existing collection and retention policy.

## Reading an event

Read the run using `GET /api/v1/sessions/{session_id}/runs/{run_id}` and search
the logs for its `error.diagnostic_id`. Compare the event with the persisted
outcome. Web and Mattermost keep their existing error display; the ID is
available through the API. The ID grants no access.

Events include known session/run, namespace, phase and public error code, plus
the failed RPC's method, request ID, numerical code and duration. `error_kind`
distinguishes rejection, transport error, timeout, cancellation, closed stream,
invalid response and oversized response. Unclassified failures use `unknown`.
Operation, sandbox, launch and process IDs are included when already available.
Kubernetes metadata comes from the log collector.

`native_message` and `stderr_tail` contain redacted text only. Stderr describes
the process during the current connection; adjacent text is not proof that a
particular RPC caused it. Stream closure does not establish an exit code, OOM
or signal. Expected replies such as “already initialized” are not logged as
run failures. Stopping the worker does not emit a failure for cancellation.

## Text limits and redaction

The worker masks known connection credentials, their JSON/URL representations,
credential assignments, authorization values, JWTs, PEM blocks and complete
URLs. Known values remain protected across credential rotation until disconnect.
Masks cannot identify every unknown secret or personal detail: diagnostic text
belongs in internal logs, never in public error messages.

| Data | Limit |
| --- | --- |
| Native message or individual stderr input line | 64 KiB; oversized input is omitted whole |
| Redacted native message | 4 KiB |
| Redacted stderr tail | 32 KiB of complete lines |
| Encoded log event | 64 KiB; omit tail, then message if needed |
| Known secrets and encoded variants | 1 MiB per connection |

Stderr is assembled across transport chunks before masking. On attachment the
first fragment is discarded because it may start inside a secret. Both output
streams are continuously consumed; stdout protocol payloads are not logged.
RPC params, results, `error.data`, arbitrary SDK errors, full environments and
auth files are also excluded.

`text_omitted` explains unavailable or discarded text; truncation flags mark
bounded messages and discarded stderr records. If the secret registry fills,
all free text is suppressed until disconnect with `redaction_limit`; structured
fields and execution behavior are unchanged.

## Reading native history

Full history is read only after a turn was accepted or a start operation could
have been dispatched to the current thread. The worker uses existing start
operations and native turn IDs; run number and execution timestamps alone are
not evidence of dispatch. Invalid thread bindings fail explicitly.

A lost start acknowledgement still requires snapshot/reconciliation and never
permits automatic resend. Initialization failures before dispatch therefore do
not trigger an unnecessary full-history read on the next manually created run.
