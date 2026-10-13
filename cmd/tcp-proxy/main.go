package main

import (
	"cmp"
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"go-tcp-proxy/internal/admin"
	"go-tcp-proxy/internal/config"
	"go-tcp-proxy/internal/metrics"
	"go-tcp-proxy/internal/proxy"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run() error {
	var (
		configPath = flag.String("config", "", "path to YAML config (env: PROXY_CONFIG)")
		listen     = flag.String("listen", "", "listen address of an ad-hoc route named \"default\"")
		upstream   = flag.String("upstream", "", "upstream address of an ad-hoc route named \"default\"")
	)
	flag.Parse()
	cfg, err := config.Load(cmp.Or(*configPath, os.Getenv(config.EnvPrefix+"CONFIG")), nil)
	if err != nil {
		return err
	}
	if *listen != "" || *upstream != "" {
		cfg.AddRoute("default", *listen, *upstream)
	}
	if err := cfg.Validate(); err != nil {
		return fmt.Errorf("invalid config: %w", err)
	}

	var level slog.Level
	if err := level.UnmarshalText([]byte(cfg.LogLevel)); err != nil {
		return fmt.Errorf("log_level: %w", err)
	}
	log := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: level}))

	m := metrics.New()
	mgr := proxy.NewManager(cfg, log, m)
	if err := mgr.Listen(); err != nil {
		return err
	}

	adm := admin.New(cfg.AdminListen, m.Registry, mgr.Ready)
	admLn, err := adm.Listen()
	if err != nil {
		return fmt.Errorf("admin listen: %w", err)
	}
	go func() {
		if err := adm.Serve(admLn); err != nil {
			log.Error("admin server failed", "err", err)
		}
	}()
	log.Info("admin server started", "listen", admLn.Addr().String())

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	serveErr := make(chan error, 1)
	go func() { serveErr <- mgr.Serve() }()

	var runErr error
	select {
	case <-ctx.Done():
		log.Info("shutdown signal received", "drain_timeout", cfg.ShutdownTimeout.String())
	case runErr = <-serveErr:
		log.Error("proxy stopped unexpectedly", "err", runErr)
	}
	stop()

	shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
	defer cancel()
	if err := mgr.Shutdown(shutdownCtx); err != nil {
		log.Warn("sessions were closed forcibly", "err", err)
	}

	admCtx, admCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer admCancel()
	if err := adm.Shutdown(admCtx); err != nil {
		log.Warn("admin shutdown", "err", err)
	}
	log.Info("shutdown complete")
	return runErr
}
