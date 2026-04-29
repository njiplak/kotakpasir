// kp is the kotakpasir CLI. It's a pure consumer of the public Go SDK
// (pkg/kotakpasir) — every subcommand maps to a Client method.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"

	"nexteam.id/kotakpasir/pkg/kotakpasir"
)

var (
	flagAddr  string
	flagToken string

	// exitCode is set by subcommands that need to propagate a sandbox-side
	// exit status (e.g. `kp exec` returns the in-sandbox command's exit code).
	exitCode int
)

func main() {
	// Quiet by default; CLI users want clean output, not request logs.
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn})))

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	root := &cobra.Command{
		Use:           "kp",
		Short:         "kotakpasir CLI — drive kpd from the terminal",
		Long:          "kp wraps the kotakpasir Go SDK so you can manage sandboxes without writing curl. Set KPD_ADDR / KPD_TOKEN, or pass --addr/--token.",
		SilenceUsage:  true,
		SilenceErrors: true,
	}

	root.PersistentFlags().StringVar(&flagAddr, "addr", envOr("KPD_ADDR", "http://127.0.0.1:8080"),
		"kpd HTTP address (env KPD_ADDR)")
	root.PersistentFlags().StringVar(&flagToken, "token", os.Getenv("KPD_TOKEN"),
		"bearer token for kpd auth (env KPD_TOKEN)")

	root.AddCommand(
		lsCmd(),
		runCmd(),
		execCmd(),
		stopCmd(),
		rmCmd(),
		inspectCmd(),
		logsCmd(),
		watchCmd(),
		completionCmd(root),
	)

	if err := root.ExecuteContext(ctx); err != nil {
		// Cobra prints help on its own. We just print the error message.
		printErr(err)
		os.Exit(1)
	}
	if exitCode != 0 {
		os.Exit(exitCode)
	}
}

// client builds a fresh SDK client from the global flags. Cheap; safe to call per-command.
func client() *kotakpasir.Client {
	return kotakpasir.New(flagAddr, kotakpasir.WithToken(flagToken))
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

// printErr formats SDK errors with a hint when the kind is recognized.
func printErr(err error) {
	switch {
	case errors.Is(err, kotakpasir.ErrUnauthorized):
		fmt.Fprintf(os.Stderr, "kp: %s\n  → set KPD_TOKEN or pass --token\n", err)
	case errors.Is(err, kotakpasir.ErrPolicyDenied):
		fmt.Fprintf(os.Stderr, "kp: %s\n  → check kotakpasir.yaml allowlist or use a defined profile\n", err)
	case errors.Is(err, kotakpasir.ErrNotFound):
		fmt.Fprintf(os.Stderr, "kp: %s\n  → run `kp ls` to see available sandboxes\n", err)
	default:
		fmt.Fprintf(os.Stderr, "kp: %s\n", err)
	}
}
