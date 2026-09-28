package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func restoreLogging(t *testing.T) {
	t.Helper()
	logger, writer, flags, prefix := slog.Default(), log.Writer(), log.Flags(), log.Prefix()
	t.Cleanup(func() {
		slog.SetDefault(logger)
		log.SetOutput(writer)
		log.SetFlags(flags)
		log.SetPrefix(prefix)
	})
}

func jsonLogRecords(t *testing.T, output []byte) []map[string]any {
	t.Helper()
	var records []map[string]any
	for line := range bytes.SplitSeq(bytes.TrimSpace(output), []byte("\n")) {
		var record map[string]any
		if err := json.Unmarshal(line, &record); err != nil {
			t.Fatalf("non-JSON log line %q: %v", line, err)
		}
		stamp, ok := record["time"].(string)
		if !ok {
			t.Fatal("missing timestamp", record)
		}
		if _, err := time.Parse(time.RFC3339Nano, stamp); err != nil {
			t.Fatal("invalid timestamp", record)
		}
		if record["level"] == nil || record["msg"] == nil {
			t.Fatal("missing log fields", record)
		}
		records = append(records, record)
	}
	return records
}

func TestExecuteFailureIsJSON(t *testing.T) {
	for _, args := range [][]string{{"unknown-private-token"}, {"serve", "--private-token"}, {"healthcheck", "--port", "private-token"}, {"migrate"}, {"worker"}} {
		t.Run(args[0], func(t *testing.T) {
			restoreLogging(t)
			t.Setenv("DATABASE_URL", "")
			var output bytes.Buffer
			if code := execute(t.Context(), args, &output); code != 1 {
				t.Fatal("failure exited successfully", code)
			}
			records := jsonLogRecords(t, output.Bytes())
			if len(records) != 1 || records[0]["level"] != "ERROR" || records[0]["error"] == nil {
				t.Fatal("invalid failure log", records)
			}
			if strings.Contains(output.String(), "private-token") {
				t.Fatal("raw argument leaked", output.String())
			}
		})
	}
}

func TestRuntimeConfigFailureDoesNotLeakSource(t *testing.T) {
	restoreLogging(t)
	path := filepath.Join(t.TempDir(), "private-token.toml")
	if err := os.WriteFile(path, []byte("[profiles.private-token]\nunknown-secret = [\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DATABASE_URL", "postgres://unused")
	t.Setenv("PUBLIC_API_KEYS", `["key"]`)
	t.Setenv("ORPHEUS_CONFIG_FILE", path)
	var output bytes.Buffer
	if code := execute(t.Context(), []string{"serve"}, &output); code != 1 {
		t.Fatal(code)
	}
	jsonLogRecords(t, output.Bytes())
	if strings.Contains(output.String(), "private-token") || strings.Contains(output.String(), "unknown-secret") || !strings.Contains(output.String(), "invalid runtime configuration") {
		t.Fatal(output.String())
	}
}

func TestHelpAndVersionRemainCLIOutput(t *testing.T) {
	for _, args := range [][]string{{"--help"}, {"--version"}, {"serve", "--help"}, {"worker", "--help"}, {"migrate", "--help"}, {"healthcheck", "--help"}} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			restoreLogging(t)
			var output bytes.Buffer
			if code := execute(t.Context(), args, &output); code != 0 || output.Len() == 0 || strings.Contains(output.String(), `"level"`) {
				t.Fatal(code, output.String())
			}
		})
	}
}

func TestDefaultAndStandardLoggersUseJSON(t *testing.T) {
	restoreLogging(t)
	var output bytes.Buffer
	if execute(t.Context(), []string{"--version"}, &output) != 0 {
		t.Fatal("logger setup failed")
	}
	output.Reset()
	slog.WarnContext(t.Context(), "A warning", "reason", "fixture")
	log.Print("A standard log")
	records := jsonLogRecords(t, output.Bytes())
	if len(records) != 2 || records[0]["level"] != "WARN" || records[0]["reason"] != "fixture" || records[1]["msg"] != "A standard log" {
		t.Fatal(records)
	}
}

func TestHTTPErrorCategories(t *testing.T) {
	const caller = "example.handler (handler.go:42)"
	for _, tt := range []struct {
		message, reason string
		caller          bool
	}{
		{"http: panic serving 127.0.0.1:123: private-token\nstack", "handler_panic", false},
		{"http: Accept error: private-token; retrying in 1s", "accept", false},
		{"http: TLS handshake error from private-token", "tls_handshake", false},
		{"http: superfluous response.WriteHeader call from " + caller, "superfluous_write_header", true},
		{"http: response.WriteHeader on hijacked connection from " + caller, "write_header_after_hijack", true},
		{"http: response.Write on hijacked connection from " + caller, "write_after_hijack", true},
		{`http: invalid Content-Length of "private-token"`, "invalid_content_length", false},
		{`http: WriteHeader called with both Transfer-Encoding of "private-token" and a Content-Length of 2`, "conflicting_transfer_headers", false},
		{"unknown private-token", "unknown", false},
	} {
		t.Run(tt.reason, func(t *testing.T) {
			var output bytes.Buffer
			writer := httpLogWriter{logger: slog.New(slog.NewJSONHandler(&output, nil))}
			message := tt.message + "\n"
			if n, err := writer.Write([]byte(message)); err != nil || n != len(message) {
				t.Fatal(n, err)
			}
			records := jsonLogRecords(t, output.Bytes())
			if len(records) != 1 || records[0]["reason"] != tt.reason || records[0]["level"] != "ERROR" {
				t.Fatal(records)
			}
			if tt.caller && records[0]["caller"] != caller || !tt.caller && records[0]["caller"] != nil {
				t.Fatal("unexpected caller metadata", records)
			}
			if strings.Contains(output.String(), "private-token") {
				t.Fatal("HTTP payload leaked", output.String())
			}
		})
	}
}

func TestHTTPPanicUsesSafeJSONLog(t *testing.T) {
	restoreLogging(t)
	var output bytes.Buffer
	if execute(t.Context(), []string{"--version"}, &output) != 0 {
		t.Fatal("logger setup failed")
	}
	output.Reset()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	address := make(chan string, 1)
	api := &http.Server{
		Addr: "127.0.0.1:0", ReadHeaderTimeout: time.Second,
		Handler:     http.HandlerFunc(func(http.ResponseWriter, *http.Request) { panic("private-token") }),
		BaseContext: func(l net.Listener) context.Context { address <- l.Addr().String(); return t.Context() },
	}
	system := &http.Server{Addr: "127.0.0.1:0", ReadHeaderTimeout: time.Second}
	var ready atomic.Bool
	done := make(chan error, 1)
	go func() { done <- serve(ctx, api, system, &ready) }()
	client := &http.Client{Timeout: time.Second}
	apiAddress := <-address
	response, err := client.Get("http://" + apiAddress)
	if err == nil {
		_, _ = io.Copy(io.Discard, response.Body)
		_ = response.Body.Close()
		t.Error("panic request unexpectedly succeeded")
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if strings.Contains(output.String(), "private-token") {
		t.Fatal("panic payload leaked", output.String())
	}
	found := false
	for _, record := range jsonLogRecords(t, output.Bytes()) {
		if record["msg"] == "HTTP servers started" {
			if record["api_address"] != apiAddress {
				t.Fatal("incorrect API address", record)
			}
			systemAddress, _ := record["system_address"].(string)
			host, port, err := net.SplitHostPort(systemAddress)
			if err != nil || host != "127.0.0.1" || port == "0" || port == "" {
				t.Fatal("incorrect system address", record)
			}
		}
		if record["reason"] == "handler_panic" && record["level"] == "ERROR" && record["listener"] == "API" {
			found = true
		}
	}
	if !found {
		t.Fatal("missing HTTP panic event", output.String())
	}
}
