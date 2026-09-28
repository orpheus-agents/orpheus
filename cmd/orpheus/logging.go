package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"log/slog"
	"strings"

	"github.com/orpheus-agents/orpheus/internal/diagnostic"
)

func execute(ctx context.Context, args []string, out io.Writer) int {
	slog.SetDefault(slog.New(slog.NewJSONHandler(out, nil)))
	if err := run(ctx, args, out); err != nil {
		// run sanitizes external errors at the command boundary.
		slog.ErrorContext(ctx, "Command failed", "error", err.Error())
		return 1
	}
	return 0
}

func parseFlags(flags *flag.FlagSet, args []string, out io.Writer) error {
	var output bytes.Buffer
	flags.SetOutput(&output)
	err := flags.Parse(args)
	if errors.Is(err, flag.ErrHelp) {
		if _, writeErr := io.Copy(out, &output); writeErr != nil {
			return fmt.Errorf("cannot write help (%s)", diagnostic.Describe(writeErr))
		}
		return flag.ErrHelp
	}
	if err != nil {
		// flag errors and usage can echo arbitrary arguments and environment values.
		return errors.New("invalid command arguments; use --help for usage")
	}
	return nil
}

// net/http diagnostics can contain panic values, request data and stack traces.
// Keep the event category while routing it through the same JSON logger.
type httpLogWriter struct {
	logger *slog.Logger
}

func (w httpLogWriter) Write(p []byte) (int, error) {
	kind, caller := "unknown", ""
	for _, category := range []struct {
		prefix, reason string
		caller         bool
	}{
		{"http: panic serving ", "handler_panic", false},
		{"http: Accept error: ", "accept", false},
		{"http: TLS handshake error from ", "tls_handshake", false},
		{"http: superfluous response.WriteHeader call from ", "superfluous_write_header", true},
		{"http: response.WriteHeader on hijacked connection from ", "write_header_after_hijack", true},
		{"http: response.Write on hijacked connection from ", "write_after_hijack", true},
		{"http: invalid Content-Length of ", "invalid_content_length", false},
		{"http: WriteHeader called with both Transfer-Encoding of ", "conflicting_transfer_headers", false},
	} {
		if rest, ok := strings.CutPrefix(string(p), category.prefix); ok {
			kind = category.reason
			// These three net/http messages contain only runtime caller metadata.
			if category.caller {
				caller = strings.TrimSpace(rest)
			}
			break
		}
	}
	attrs := []any{"reason", kind}
	if caller != "" {
		attrs = append(attrs, "caller", caller)
	}
	w.logger.Error("HTTP server error", attrs...)
	return len(p), nil
}

func httpErrorLogger(name string) *log.Logger {
	return log.New(httpLogWriter{slog.Default().With("listener", name)}, "", 0)
}
