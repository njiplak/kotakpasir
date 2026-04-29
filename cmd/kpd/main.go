package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"nexteam.id/kotakpasir/internal/api"
	"nexteam.id/kotakpasir/internal/config"
	"nexteam.id/kotakpasir/internal/metrics"
	"nexteam.id/kotakpasir/internal/policy"
	dockerrt "nexteam.id/kotakpasir/internal/runtime/docker"
	"nexteam.id/kotakpasir/internal/sandbox"
	sqlitestore "nexteam.id/kotakpasir/internal/sandbox/store/sqlite"
)

func main() {
	cfg := config.Load()
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: config.ParseLevel(cfg.Server.LogLevel)})))

	root := &cobra.Command{
		Use:           "kpd",
		Short:         "kotakpasir HTTP server for AI agents",
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return serve(cmd, cfg)
		},
	}

	root.Flags().String("addr", cfg.Server.Addr, "listen address (env KPD_ADDR)")
	root.Flags().String("token", cfg.Server.Token, "bearer token for auth (env KPD_TOKEN)")
	root.Flags().String("db", cfg.Storage.DBPath, "sqlite path (env KPD_DB)")
	root.Flags().String("policy", cfg.Policy.File, "policy YAML path (env KP_POLICY_FILE)")
	root.Flags().Bool("require-policy", cfg.Policy.Required, "refuse to start without a readable policy file (env KP_REQUIRE_POLICY)")
	root.Flags().Duration("shutdown-timeout", cfg.Server.ShutdownTimeout, "graceful shutdown timeout (env KPD_SHUTDOWN_TIMEOUT)")

	if err := root.Execute(); err != nil {
		slog.Error("kpd failed", "err", err)
		os.Exit(1)
	}
}

func serve(cmd *cobra.Command, cfg config.Config) error {
	cfg.Server.Addr, _ = cmd.Flags().GetString("addr")
	cfg.Server.Token, _ = cmd.Flags().GetString("token")
	cfg.Storage.DBPath, _ = cmd.Flags().GetString("db")
	cfg.Policy.File, _ = cmd.Flags().GetString("policy")
	cfg.Policy.Required, _ = cmd.Flags().GetBool("require-policy")
	cfg.Server.ShutdownTimeout, _ = cmd.Flags().GetDuration("shutdown-timeout")

	if cfg.Policy.Required {
		if cfg.Policy.File == "" {
			return errors.New("require-policy is set but no policy file is configured (set KP_POLICY_FILE or --policy)")
		}
		if _, err := os.Stat(cfg.Policy.File); err != nil {
			return fmt.Errorf("require-policy is set but policy file is unreadable: %w", err)
		}
	}

	pol, err := policy.Load(cfg.Policy.File)
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	rt, err := dockerrt.New()
	if err != nil {
		return err
	}
	defer rt.Close()

	store, err := sqlitestore.Open(ctx, cfg.Storage.DBPath)
	if err != nil {
		return err
	}
	defer store.Close()

	rec := metrics.New()

	mgr, err := sandbox.NewManager(sandbox.Options{
		Runtime:        rt,
		Store:          store,
		Policy:         pol,
		LogBufferBytes: cfg.Logs.BufferBytes,
		Metrics:        rec,
	})
	if err != nil {
		return err
	}

	if err := mgr.Start(ctx); err != nil {
		return err
	}
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := mgr.Shutdown(shutdownCtx); err != nil {
			slog.Warn("pool shutdown", "err", err)
		}
	}()

	reaper := sandbox.NewReaper(mgr, cfg.Reaper.Interval)
	go func() {
		if err := reaper.Run(ctx); err != nil {
			slog.Error("reaper exited", "err", err)
		}
	}()

	app := api.NewServer(api.Options{
		Manager: mgr,
		Token:   cfg.Server.Token,
		Metrics: rec.Handler(),
	}).App()

	listenErr := make(chan error, 1)
	go func() {
		slog.Info("kpd listening",
			"addr", cfg.Server.Addr,
			"auth", cfg.Server.Token != "",
			"db", cfg.Storage.DBPath,
			"policy", policySource(cfg.Policy.File),
			"images", len(pol.Images),
			"profiles", len(pol.Profiles),
		)
		listenErr <- app.Listen(cfg.Server.Addr)
	}()

	select {
	case err := <-listenErr:
		return err
	case <-ctx.Done():
		slog.Info("shutdown signal received")
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.Server.ShutdownTimeout)
	defer cancel()

	return app.ShutdownWithContext(shutdownCtx)
}

func policySource(path string) string {
	if path == "" {
		return "(defaults)"
	}
	return path
}
