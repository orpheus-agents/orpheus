# Go API client

Import `github.com/orpheus-agents/orpheus/client` using the Orpheus release tag.
Go 1.27 or newer is required. This is a package in the root module, with the same
version as the server; there is no separate `client/vX.Y.Z` tag or nested module.
The first release containing this package must be newer than `v0.1.0`.

`client.gen.go` is generated from `../api/openapi.yaml` using
`../api/oapi-client.yaml`. Do not edit it manually. The generator version is pinned
in the root `go.mod`; consumers use the committed code without running a generator.

## Requests and responses

```go
import (
    "context"
    "fmt"
    "net/http"
    "time"

    "github.com/orpheus-agents/orpheus/client"
)

func create(ctx context.Context, baseURL, token, idempotencyKey string) (*client.Accepted, error) {
    api, err := client.NewClientWithResponses(baseURL,
        client.WithHTTPClient(&http.Client{Timeout: 30 * time.Second}),
        client.WithRequestEditorFn(func(_ context.Context, r *http.Request) error {
            r.Header.Set("Authorization", "Bearer "+token)
            return nil
        }),
    )
    if err != nil {
        return nil, err
    }
    response, err := api.CreateSessionWithResponse(ctx,
        &client.CreateSessionParams{IdempotencyKey: &idempotencyKey},
        client.CreateSession{
            Namespace: new("my-connector"),
            ExternalKey: new("source:thread-id"),
            Configuration: client.ConfigurationInput{
                Agent: client.AgentInput{Profile: "default"},
                Sandbox: client.SandboxInput{Template: "my-template"},
            },
            Messages: []client.TextMessage{{Text: "Hello", ExternalKey: new("source:post-id")}},
        },
    )
    if err != nil {
        return nil, err
    }
    if response.StatusCode() != http.StatusAccepted || response.JSON202 == nil {
        return nil, fmt.Errorf("create session: HTTP %d", response.StatusCode())
    }
    return response.JSON202, nil
}
```

Pass the server origin, optionally with a reverse-proxy path prefix, rather than
a URL ending in `/api/v1`. The generated methods append the API paths.

`CreateSession`, `CreateRun`, and `SendMessage` accept a nonempty ordered `Messages`
slice. Admission is atomic, and `Accepted.MessageID` identifies its last element.
Each `TextMessage` may include `Metadata` as a JSON object. Orpheus stores it with
the message and returns it in history and `message.updated` events; only `Text`
is passed to the agent. Use `ExternalKey` to identify a source item, and
`Metadata` for the accepted snapshot needed by the connector after restart.

- `ClientWithResponses` reads and closes JSON response bodies. A nil Go error is
  not proof of API success: inspect `StatusCode()` and `JSON200`/`JSON202` or the
  corresponding error field, such as `JSON409.Error.Code`.
- Unknown statuses and non-JSON bodies remain available in `Body` and `HTTPResponse`.
  Headers such as `Retry-After` are preserved. Accepted responses also expose
  typed `Headers202.Location`.
- The raw methods return `*http.Response`; the caller must close `Body`.
- Retries and pagination are explicit. Reuse the same idempotency key and body
  for a retried mutation; an unclear transport result must not create a new job.
  Follow `NextCursor` (and `HasMore` for events) without interpreting opaque cursors.
- Event, history and hook-output unions expose `Discriminator`/`As…` helpers where
  specified by OpenAPI. Inspect the discriminator before selecting a variant;
  unmarshalling into a different variant is not semantic validation.

## SSE

Use `api.StreamEvents(ctx, sessionID, params)` to get the streaming response;
check the status and content type before reading, and close `Body` when done.
`StreamEventsParams` exposes both `After` and `LastEventID`; the server gives
`Last-Event-ID` precedence. Cancel the request context to stop streaming.

**Do not use `StreamEventsWithResponse` for a live stream.** Like the other
generated response parsers, it reads until EOF. Use a separate HTTP client or
request context with an appropriate lifetime; a short JSON-request timeout would
also terminate an SSE stream. SSE parsing, cursor persistence and reconnection
policy belong to the connector. REST history/events remain available for recovery.

## Development

```sh
make generate-client
make generate-client-check
make test-client build-client
```

The full `make generate` and `make check` include this package. Release tags run
only `make build-client` before image publication; generation checks and tests
remain in branch/PR CI. External consumers
of the generated API are not visible to the server's dead-code analysis; that
check remains restricted to the server and its internal tooling.
