// kpproxy is the per-sandbox egress proxy. It runs in its own container,
// listens on KP_PROXY_PORT, and only allows CONNECT to KP_ALLOWED_HOSTS.
package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"nexteam.id/kotakpasir/internal/config"
	"nexteam.id/kotakpasir/internal/proxy"
)

func main() {
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
		Level: config.ParseLevel(envOr("KP_LOG_LEVEL", "info")),
	})))

	cfg := proxy.Config{
		ListenAddr:   ":" + envOr("KP_PROXY_PORT", "8080"),
		AllowedHosts: splitCSV(os.Getenv("KP_ALLOWED_HOSTS")),
		DenyHosts:    splitCSV(os.Getenv("KP_DENY_HOSTS")),
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if err := proxy.New(cfg).Run(ctx); err != nil {
		slog.Error("kpproxy failed", "err", err)
		os.Exit(1)
	}
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func splitCSV(s string) []string {
	if s == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
