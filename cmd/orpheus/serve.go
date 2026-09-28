package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"sync/atomic"
	"time"
)

func serve(ctx context.Context, api, system *http.Server, ready *atomic.Bool) error {
	ready.Store(false)
	if ctx.Err() != nil {
		return nil
	}
	servers := []struct {
		name     string
		server   *http.Server
		listener net.Listener
	}{{name: "API", server: api}, {name: "system", server: system}}
	// Bind both sockets before either server starts accepting requests.
	var listen net.ListenConfig
	for i := range servers {
		s := &servers[i]
		s.server.ErrorLog = httpErrorLogger(s.name)
		listener, err := listen.Listen(ctx, "tcp", s.server.Addr)
		if err != nil {
			return fmt.Errorf("listen on %s HTTP address: %w", s.name, err)
		}
		defer func() { _ = listener.Close() }()
		s.listener = listener
	}
	if ctx.Err() != nil {
		return nil
	}
	ready.Store(true)
	slog.InfoContext(ctx, "HTTP servers started", "api_address", servers[0].listener.Addr().String(), "system_address", servers[1].listener.Addr().String())
	stopped := make(chan error, len(servers))
	for _, s := range servers {
		go func() { stopped <- fmt.Errorf("%s HTTP server stopped: %w", s.name, s.server.Serve(s.listener)) }()
	}
	var result error
	remaining := len(servers)
	select {
	case result = <-stopped:
		remaining--
	case <-ctx.Done():
	}
	ready.Store(false)
	slog.InfoContext(ctx, "HTTP servers stopping")
	// Keep the system listener up while API requests drain, reporting not ready.
	// SSE has its own cancellation hook; probes need no graceful drain.
	shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := api.Shutdown(shutdown); err != nil {
		_ = api.Close()
		result = errors.Join(result, fmt.Errorf("stop API HTTP server: %w", err))
	}
	if err := system.Close(); err != nil {
		result = errors.Join(result, fmt.Errorf("stop system HTTP server: %w", err))
	}
	for range remaining {
		<-stopped
	}
	return result
}
