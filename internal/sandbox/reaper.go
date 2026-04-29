package sandbox

import (
	"context"
	"errors"
	"log/slog"
	"time"
)

type Reaper struct {
	mgr      *Manager
	interval time.Duration
}

func NewReaper(mgr *Manager, interval time.Duration) *Reaper {
	return &Reaper{mgr: mgr, interval: interval}
}

func (r *Reaper) Run(ctx context.Context) error {
	if r.interval <= 0 {
		slog.Info("reaper disabled")
		<-ctx.Done()
		return nil
	}

	slog.Info("reaper started", "interval", r.interval)
	t := time.NewTicker(r.interval)
	defer t.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil
		case now := <-t.C:
			r.sweep(ctx, now.UTC())
		}
	}
}

func (r *Reaper) sweep(ctx context.Context, now time.Time) {
	expired, err := r.mgr.ExpiredBefore(ctx, now)
	if err != nil {
		slog.Warn("reaper list", "err", err)
		return
	}
	if len(expired) == 0 {
		return
	}
	swept := 0
	for _, sb := range expired {
		if err := r.mgr.Delete(ctx, sb.ID); err != nil && !errors.Is(err, ErrNotFound) {
			slog.Warn("reaper delete", "sandbox_id", sb.ID, "err", err)
			continue
		}
		swept++
		slog.Info("reaped", "sandbox_id", sb.ID, "image", sb.Image, "expires_at", sb.ExpiresAt)
	}
	if swept > 0 {
		r.mgr.metrics.ReaperSwept(swept)
	}
}
