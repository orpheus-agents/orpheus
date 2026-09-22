package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/skillum-ai/orpheus/internal/agentbox"
	"github.com/skillum-ai/orpheus/internal/config"
	"github.com/skillum-ai/orpheus/internal/diagnostic"
	"github.com/skillum-ai/orpheus/internal/httpserver"
	"github.com/skillum-ai/orpheus/internal/migrate"
	"github.com/skillum-ai/orpheus/internal/store"
	"github.com/skillum-ai/orpheus/internal/worker"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	err := run(ctx, os.Args[1:], os.Stdout)
	stop()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
func run(ctx context.Context, args []string, out io.Writer) error {
	if len(args) > 0 && args[0] == "--version" {
		_, err := fmt.Fprintln(out, "orpheus 0.1.0")
		return err
	}
	if len(args) == 0 || args[0] == "--help" || args[0] == "help" {
		_, err := fmt.Fprintln(out, "Usage: orpheus <serve|worker|migrate|healthcheck>\nAll application configuration is supplied through environment variables.")
		return err
	}
	if args[0] == "healthcheck" {
		flags := flag.NewFlagSet("healthcheck", flag.ContinueOnError)
		flags.SetOutput(out)
		port := flags.String("port", envDefault("ORPHEUS_PORT", "8000"), "HTTP port")
		if err := flags.Parse(args[1:]); err != nil {
			return err
		}
		if err := validatePort(*port); err != nil {
			return err
		}
		ctx, cancel := context.WithTimeout(ctx, 4*time.Second)
		defer cancel()
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+net.JoinHostPort("127.0.0.1", *port)+"/ready", nil)
		if err != nil {
			return err
		}
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			return errors.New("readiness probe failed")
		}
		defer func() { _ = res.Body.Close() }()
		if res.StatusCode != 200 {
			return errors.New("service is not ready")
		}
		return nil
	}
	if args[0] != "serve" && args[0] != "worker" && args[0] != "migrate" {
		return errors.New("unknown command")
	}
	flags := flag.NewFlagSet(args[0], flag.ContinueOnError)
	flags.SetOutput(out)
	host := flags.String("host", envDefault("ORPHEUS_HOST", "0.0.0.0"), "HTTP bind host")
	port := flags.String("port", envDefault("ORPHEUS_PORT", "8000"), "HTTP port")
	var migrationsDir string
	if args[0] == "migrate" {
		flags.StringVar(&migrationsDir, "dir", envDefault("ORPHEUS_MIGRATIONS_DIR", "migrations"), "SQL migrations directory")
	}
	if err := flags.Parse(args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if args[0] == "serve" {
		if err := validatePort(*port); err != nil {
			return err
		}
	}
	settings, err := config.Load()
	if err != nil {
		return err
	}
	if args[0] == "migrate" {
		if flags.NArg() > 1 {
			return errors.New("unexpected migration arguments")
		}
		command := "up"
		if flags.NArg() > 0 {
			command = flags.Arg(0)
		}
		if command != "up" && command != "down" && command != "reset" {
			return errors.New("migrate accepts up, down, or reset")
		}
		if err := migrate.Run(ctx, settings.DatabaseURL, command, migrationsDir); err != nil {
			return fmt.Errorf("database migration failed (%s)", diagnostic.Describe(err))
		}
		return nil
	}
	if flags.NArg() != 0 {
		return errors.New("unexpected arguments")
	}
	profiles, cipher, err := settings.Runtime(args[0] == "serve")
	if err != nil {
		return err
	}
	poolConfig, err := config.DatabasePoolConfig(settings.DatabaseURL)
	if err != nil {
		return errors.New("invalid database configuration")
	}
	pool, err := pgxpool.NewWithConfig(ctx, poolConfig)
	if err != nil {
		return errors.New("invalid database configuration")
	}
	defer pool.Close()
	storage := &store.Store{Pool: pool, Settings: settings, Profiles: profiles, Cipher: cipher}
	if args[0] == "worker" {
		platform, err := agentbox.New()
		if err != nil {
			return errors.New("invalid AgentBox configuration")
		}
		if err := worker.Run(ctx, storage, platform); err != nil {
			return fmt.Errorf("worker stopped: storage ownership or connectivity failed (%s)", diagnostic.Describe(err))
		}
		return nil
	}
	streams, stopStreams := context.WithCancel(context.Background())
	defer stopStreams()
	handler, err := httpserver.Handler(storage, streams)
	if err != nil {
		return err
	}
	server := &http.Server{Addr: net.JoinHostPort(*host, *port), Handler: handler, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 30 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 1 << 20}
	server.RegisterOnShutdown(stopStreams)
	return serve(ctx, server)
}

func serve(ctx context.Context, server *http.Server) error {
	// SIGTERM stops accepting requests; active requests retain their contexts
	// until the grace period expires. SSE has a separate shutdown context.
	stopped := make(chan error, 1)
	go func() { stopped <- server.ListenAndServe() }()
	select {
	case err := <-stopped:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdown); err != nil {
			_ = server.Close()
			return err
		}
		return nil
	}
}

func envDefault(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}
func validatePort(value string) error {
	port, err := strconv.Atoi(value)
	if err != nil || port < 1 || port > 65535 {
		return errors.New("HTTP port must be between 1 and 65535")
	}
	return nil
}
