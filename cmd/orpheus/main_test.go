package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestHelpWithoutRuntimeConfiguration(t *testing.T) {
	for _, args := range [][]string{nil, {"--help"}, {"--version"}, {"serve", "--help"}, {"worker", "--help"}, {"migrate", "--help"}} {
		var output bytes.Buffer
		if err := run(t.Context(), args, &output); err != nil {
			t.Fatalf("%v: %v", args, err)
		}
		if output.Len() == 0 {
			t.Fatal("missing help")
		}
	}
}

func TestHealthcheckUsesConfiguredPort(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/ready" {
			t.Error(r.URL.Path)
		}
		w.WriteHeader(200)
	}))
	defer s.Close()
	_, port, err := net.SplitHostPort(s.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("ORPHEUS_PORT", port)
	if err := run(t.Context(), []string{"healthcheck"}, io.Discard); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ORPHEUS_PORT", "invalid")
	if err := run(t.Context(), []string{"healthcheck", "--port", port}, io.Discard); err != nil {
		t.Fatal(err)
	}
}

func TestShutdownLetsInFlightRequestFinish(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	address := make(chan string, 1)
	started, shutdown, release := make(chan context.Context, 1), make(chan struct{}), make(chan struct{})
	server := &http.Server{Addr: "127.0.0.1:0", ReadHeaderTimeout: time.Second, BaseContext: func(l net.Listener) context.Context { address <- l.Addr().String(); return t.Context() }, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { started <- r.Context(); <-release; w.WriteHeader(204) })}
	server.RegisterOnShutdown(func() { close(shutdown) })
	stopped := make(chan error, 1)
	go func() { stopped <- serve(ctx, server) }()
	client := &http.Client{Timeout: 5 * time.Second}
	response := make(chan error, 1)
	url := "http://" + <-address
	go func() {
		r, err := client.Get(url)
		if err == nil {
			_ = r.Body.Close()
			if r.StatusCode != 204 {
				err = fmt.Errorf("status %d", r.StatusCode)
			}
		}
		response <- err
	}()
	requestCtx := <-started
	cancel()
	<-shutdown
	err := requestCtx.Err()
	close(release)
	if err != nil {
		t.Error("request cancelled before grace period", err)
	}
	if err := <-response; err != nil {
		t.Error(err)
	}
	if err := <-stopped; err != nil {
		t.Error(err)
	}
}

func TestServeEnvironmentAndPortValidation(t *testing.T) {
	t.Setenv("ORPHEUS_HOST", "127.0.0.2")
	t.Setenv("ORPHEUS_PORT", "8123")
	var output bytes.Buffer
	if err := run(t.Context(), []string{"serve", "--help"}, &output); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "127.0.0.2") || !strings.Contains(output.String(), "8123") {
		t.Fatal("environment defaults absent", output.String())
	}
	for _, value := range []string{"0", "65536", "invalid", "-1"} {
		t.Setenv("ORPHEUS_PORT", value)
		if err := run(t.Context(), []string{"serve"}, &output); err == nil || !strings.Contains(err.Error(), "HTTP port") {
			t.Fatal("invalid port accepted", err)
		}
	}
	t.Setenv("DATABASE_URL", "")
	if err := run(t.Context(), []string{"serve", "--port", "9000"}, &output); err == nil || !strings.Contains(err.Error(), "DATABASE_URL") {
		t.Fatal("explicit port failed to override environment", err)
	}
}
func TestInvalidCLI(t *testing.T) {
	var output bytes.Buffer
	if err := run(t.Context(), []string{"unknown"}, &output); err == nil {
		t.Fatal("unknown command accepted")
	}
	t.Setenv("DATABASE_URL", "")
	if err := run(t.Context(), []string{"serve"}, &output); err == nil || !strings.Contains(err.Error(), "DATABASE_URL") {
		t.Fatal(err)
	}
}
