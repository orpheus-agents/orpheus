package main

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/orpheus-agents/orpheus/internal/httpserver"
)

func checkProbe(t *testing.T, client *http.Client, url string, status int) {
	t.Helper()
	res, err := client.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode != status {
		t.Fatalf("%s: want %d, got %d", url, status, res.StatusCode)
	}
}

func TestServeClosesUnusedSystemConnection(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	var ready atomic.Bool
	address := make(chan string, 1)
	accepted := make(chan struct{}, 1)
	api := &http.Server{Addr: "127.0.0.1:0", ReadHeaderTimeout: time.Second}
	system := &http.Server{
		Addr: "127.0.0.1:0", ReadHeaderTimeout: 30 * time.Second,
		Handler: httpserver.SystemHandler(&ready),
		BaseContext: func(l net.Listener) context.Context {
			address <- l.Addr().String()
			return t.Context()
		},
		ConnState: func(_ net.Conn, state http.ConnState) {
			if state == http.StateNew {
				accepted <- struct{}{}
			}
		},
	}
	t.Cleanup(func() { _ = api.Close(); _ = system.Close() })
	stopped := make(chan error, 1)
	go func() { stopped <- serve(ctx, api, system, &ready) }()
	conn, err := net.DialTimeout("tcp", <-address, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	select {
	case <-accepted:
	case <-time.After(2 * time.Second):
		t.Fatal("system connection was not accepted")
	}
	cancel()
	select {
	case err := <-stopped:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("unused system connection delayed shutdown")
	}
	if ready.Load() {
		t.Fatal("stopped process is ready")
	}
	if err := conn.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Read(make([]byte, 1)); !errors.Is(err, io.EOF) {
		t.Fatal("system connection was not closed", err)
	}
}

func TestServeStopsBothServersOnFailure(t *testing.T) {
	for _, failed := range []string{"API", "system"} {
		t.Run(failed, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			var ready atomic.Bool
			apiAddress, systemAddress := make(chan string, 1), make(chan string, 1)
			api := &http.Server{Addr: "127.0.0.1:0", ReadHeaderTimeout: time.Second, Handler: http.NotFoundHandler(), BaseContext: func(l net.Listener) context.Context { apiAddress <- l.Addr().String(); return t.Context() }}
			system := &http.Server{Addr: "127.0.0.1:0", ReadHeaderTimeout: time.Second, Handler: httpserver.SystemHandler(&ready), BaseContext: func(l net.Listener) context.Context { systemAddress <- l.Addr().String(); return t.Context() }}
			stopped := make(chan error, 1)
			go func() { stopped <- serve(ctx, api, system, &ready) }()
			apiURL, systemURL := "http://"+<-apiAddress, "http://"+<-systemAddress
			client := &http.Client{Timeout: time.Second}
			checkProbe(t, client, apiURL+"/", 404)
			checkProbe(t, client, systemURL+"/ready", 200)
			target := api
			if failed == "system" {
				target = system
			}
			if err := target.Close(); err != nil {
				t.Fatal(err)
			}
			select {
			case err := <-stopped:
				if !errors.Is(err, http.ErrServerClosed) || !strings.Contains(err.Error(), failed) {
					t.Fatal("lost server failure", err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("other server did not stop")
			}
			if ready.Load() {
				t.Fatal("stopped process is ready")
			}
			for _, url := range []string{apiURL, systemURL} {
				res, err := client.Get(url)
				if err == nil {
					_ = res.Body.Close()
					t.Fatal("listener remains available", url)
				}
			}
		})
	}
}

func TestServeBindFailureDoesNotStartOtherServer(t *testing.T) {
	for _, failed := range []string{"API", "system"} {
		t.Run(failed, func(t *testing.T) {
			occupied, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = occupied.Close() }()
			spare, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			address := spare.Addr().String()
			_ = spare.Close()
			api := &http.Server{Addr: address, ReadHeaderTimeout: time.Second}
			system := &http.Server{Addr: occupied.Addr().String(), ReadHeaderTimeout: time.Second}
			if failed == "API" {
				api, system = system, api
			}
			var ready atomic.Bool
			if err := serve(t.Context(), api, system, &ready); err == nil || !strings.Contains(err.Error(), failed) {
				t.Fatal("bind failure not reported", err)
			}
			if ready.Load() {
				t.Fatal("partially started process is ready")
			}
			reopened, err := net.Listen("tcp", address)
			if err != nil {
				t.Fatal("listener leaked on failed startup", err)
			}
			_ = reopened.Close()
		})
	}
}

func TestServeCancelledBeforeStartup(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	var ready atomic.Bool
	if err := serve(ctx, nil, nil, &ready); err != nil || ready.Load() {
		t.Fatal(err)
	}
}

func TestHealthcheckFailure(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(503) }))
	defer s.Close()
	host, port, err := net.SplitHostPort(s.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("ORPHEUS_SYSTEM_HOST", host)
	t.Setenv("ORPHEUS_SYSTEM_PORT", port)
	if err := run(t.Context(), []string{"healthcheck"}, io.Discard); err == nil || err.Error() != "service is not ready" {
		t.Fatal(err)
	}
	s.Close()
	if err := run(t.Context(), []string{"healthcheck"}, io.Discard); err == nil || err.Error() != "readiness probe failed" {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"healthcheck", "--port", "invalid"}, {"healthcheck", "extra"}} {
		if err := run(t.Context(), args, io.Discard); err == nil {
			t.Fatal("invalid arguments accepted", args)
		}
	}
}
