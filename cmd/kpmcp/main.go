package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/spf13/cobra"

	"nexteam.id/kotakpasir/internal/config"
	mcpserver "nexteam.id/kotakpasir/internal/mcp"
	"nexteam.id/kotakpasir/internal/policy"
	dockerrt "nexteam.id/kotakpasir/internal/runtime/docker"
	"nexteam.id/kotakpasir/internal/sandbox"
	sqlitestore "nexteam.id/kotakpasir/internal/sandbox/store/sqlite"
)

func main() {
	cfg := config.Load()
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: config.ParseLevel(cfg.Server.LogLevel)})))

	root := &cobra.Command{
		Use:           "kpmcp",
		Short:         "kotakpasir MCP server (stdio) for AI agents",
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return run(cmd, cfg)
		},
	}

	root.Flags().String("db", cfg.Storage.DBPath, "sqlite path (env KPD_DB)")
	root.Flags().String("policy", cfg.Policy.File, "policy YAML path (env KP_POLICY_FILE)")

	if err := root.Execute(); err != nil {
		slog.Error("kpmcp failed", "err", err)
		os.Exit(1)
	}
}

func run(cmd *cobra.Command, cfg config.Config) error {
	cfg.Storage.DBPath, _ = cmd.Flags().GetString("db")
	cfg.Policy.File, _ = cmd.Flags().GetString("policy")

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

	mgr, err := sandbox.NewManager(sandbox.Options{
		Runtime: rt,
		Store:   store,
		Policy:  pol,
	})
	if err != nil {
		return err
	}

	srv := mcpserver.NewServer(mgr)
	slog.Info("kpmcp ready",
		"transport", "stdio",
		"db", cfg.Storage.DBPath,
		"images", len(pol.Images),
		"profiles", len(pol.Profiles),
	)
	return srv.Run(ctx, &mcpsdk.StdioTransport{})
}
