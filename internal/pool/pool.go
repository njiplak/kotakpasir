// Package pool maintains per-image queues of pre-started sandbox containers
// so common Create requests can claim a warm container instead of paying the
// docker-run cold-start cost. Pool entries are one-shot: claimed containers
// leave the pool and are not returned to it.
package pool

import (
	"context"
	"errors"
	"log/slog"
	"sync"

	rt "nexteam.id/kotakpasir/internal/runtime"
)

// LabelRole and LabelPoolImage identify pool entries to the Docker daemon.
// Recovery on kpd startup looks for these labels to clean up orphans.
const (
	LabelRole      = "kotakpasir.role"
	LabelPoolImage = "kotakpasir.pool-image"
	RolePoolWarm   = "pool-warm"
)

type Config struct {
	Image  string
	Target int
}

// Pool keeps Config.Target containers warm. Get pops one and triggers
// async refill. Shutdown drains and removes everything.
type Pool struct {
	cfg  Config
	rt   rt.Runtime
	spec rt.Spec

	mu       sync.Mutex
	avail    []string
	closed   bool
	refillCh chan struct{}
}

// New constructs a pool. spec is the template applied to every warm container;
// the pool decorates it with pool-tracking labels.
func New(runtime rt.Runtime, cfg Config, spec rt.Spec) *Pool {
	if spec.Labels == nil {
		spec.Labels = make(map[string]string, 3)
	}
	spec.Labels[LabelRole] = RolePoolWarm
	spec.Labels[LabelPoolImage] = cfg.Image
	spec.Labels["kotakpasir.managed"] = "true"

	return &Pool{
		cfg:      cfg,
		rt:       runtime,
		spec:     spec,
		refillCh: make(chan struct{}, 1),
	}
}

// Start eagerly fills the pool to Target and launches the background refiller.
// Returns the first error encountered if the initial fill cannot complete.
func (p *Pool) Start(ctx context.Context) error {
	for range p.cfg.Target {
		if err := p.spawn(ctx); err != nil {
			return err
		}
	}
	go p.refillLoop(ctx)
	return nil
}

// Get pops a warm container ID, or returns ok=false if the pool is empty.
// On success, signals the refiller to spawn a replacement asynchronously.
func (p *Pool) Get(_ context.Context) (string, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed || len(p.avail) == 0 {
		return "", false
	}
	id := p.avail[len(p.avail)-1]
	p.avail = p.avail[:len(p.avail)-1]

	select {
	case p.refillCh <- struct{}{}:
	default:
	}
	return id, true
}

// Available returns the number of warm containers currently ready.
func (p *Pool) Available() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.avail)
}

// Target returns the configured warm-pool size.
func (p *Pool) Target() int { return p.cfg.Target }

// Shutdown removes every remaining warm container and prevents further refills.
func (p *Pool) Shutdown(ctx context.Context) error {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return nil
	}
	p.closed = true
	ids := p.avail
	p.avail = nil
	p.mu.Unlock()

	var errs []error
	for _, id := range ids {
		if err := p.rt.Remove(ctx, id); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func (p *Pool) Image() string {
	return p.cfg.Image
}

func (p *Pool) spawn(ctx context.Context) error {
	handle, err := p.rt.Create(ctx, p.spec)
	if err != nil {
		return err
	}

	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		_ = p.rt.Remove(ctx, handle.ID)
		return errors.New("pool closed during spawn")
	}
	p.avail = append(p.avail, handle.ID)
	p.mu.Unlock()
	return nil
}

func (p *Pool) refillLoop(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-p.refillCh:
		}

		for {
			p.mu.Lock()
			closed := p.closed
			need := p.cfg.Target - len(p.avail)
			p.mu.Unlock()

			if closed || need <= 0 {
				break
			}
			if err := p.spawn(ctx); err != nil {
				slog.Warn("pool refill failed", "image", p.cfg.Image, "err", err)
				break
			}
		}
	}
}
