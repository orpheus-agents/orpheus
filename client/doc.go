// Package client provides the HTTP client and wire types generated from the
// Orpheus OpenAPI specification. Its version matches the repository's release tag.
//
// NewClientWithResponses parses JSON responses. A nil error only means that the
// HTTP exchange and parsing succeeded: callers must also check StatusCode and the
// appropriate JSON field. Retries, pagination and durable delivery are owned by
// the integration, not the transport client.
//
// Use the raw StreamEvents method for SSE and close its response body when done.
// StreamEventsWithResponse buffers the entire response and must not be used for
// a long-lived stream. Set request deadlines or supply a custom HTTP client;
// the generated client's default transport has no overall timeout.
package client
