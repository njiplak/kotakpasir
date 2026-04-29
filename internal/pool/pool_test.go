package pool_test

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"nexteam.id/kotakpasir/internal/pool"
	rt "nexteam.id/kotakpasir/internal/runtime"
)

// fakeRuntime mirrors runtime behavior with a tunable Create latency so we can
// measure how much of a request's wall-clock the pool actually saves.
type fakeRuntime struct {
	createDelay time.Duration
	created     atomic.Int64
	removed     atomic.Int64
}

func (f *fakeRuntime) Create(ctx context.Context, _ rt.Spec) (rt.Handle, error) {
	if f.createDelay > 0 {
		select {
		case <-time.After(f.createDelay):
		case <-ctx.Done():
			return rt.Handle{}, ctx.Err()
		}
	}
	f.created.Add(1)
	return rt.Handle{ID: "rt-" + uuid.NewString()}, nil
}
func (f *fakeRuntime) Exec(_ context.Context, _ string, _ rt.ExecSpec) (rt.ExecResult, error) {
	return rt.ExecResult{}, nil
}
func (f *fakeRuntime) Stop(_ context.Context, _ string) error                { return nil }
func (f *fakeRuntime) Remove(_ context.Context, _ string) error              { f.removed.Add(1); return nil }
func (f *fakeRuntime) Status(_ context.Context, _ string) (rt.Status, error) { return rt.Status{}, nil }
func (f *fakeRuntime) Close() error                                          { return nil }

func TestPool_GetReturnsWarmEntry(t *testing.T) {
	fr := &fakeRuntime{}
	p := pool.New(fr, pool.Config{Image: "alpine:latest", Target: 2}, rt.Spec{})
	if err := p.Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = p.Shutdown(context.Background()) }()

	if got := p.Available(); got != 2 {
		t.Fatalf("Available=%d, want 2", got)
	}
	id, ok := p.Get(t.Context())
	if !ok || id == "" {
		t.Fatalf("Get failed: id=%q ok=%v", id, ok)
	}
	if got := p.Available(); got != 1 {
		t.Fatalf("after Get: Available=%d, want 1", got)
	}
}

// BenchmarkPool_ClaimVsCold measures the latency Pool.Get gives over the
// equivalent rt.Create. With createDelay=10ms the pool path should clock in
// at sub-microsecond per claim while a cold create costs >=10ms. The number
// is documentation, not an assertion — but a regression that makes Get
// noticeably slower will jump out here.
func BenchmarkPool_ClaimVsCold(b *testing.B) {
	delay := 10 * time.Millisecond

	b.Run("cold", func(b *testing.B) {
		fr := &fakeRuntime{createDelay: delay}
		ctx := b.Context()
		b.ResetTimer()
		for b.Loop() {
			_, err := fr.Create(ctx, rt.Spec{})
			if err != nil {
				b.Fatalf("Create: %v", err)
			}
		}
	})

	b.Run("warm-claim", func(b *testing.B) {
		fr := &fakeRuntime{createDelay: delay}
		// Pre-warm enough entries to absorb the whole loop without refill stalls.
		// b.N is set by the framework before each iteration; we cap it for the
		// initial fill and lean on the refill loop to keep up afterward.
		target := 64
		p := pool.New(fr, pool.Config{Image: "alpine:latest", Target: target}, rt.Spec{})
		if err := p.Start(b.Context()); err != nil {
			b.Fatalf("Start: %v", err)
		}
		defer func() { _ = p.Shutdown(context.Background()) }()

		ctx := b.Context()
		b.ResetTimer()
		for b.Loop() {
			id, ok := p.Get(ctx)
			if !ok {
				// Pool ran dry — the refill loop is tied to docker-Create
				// latency, so under heavy benchmark pressure with delay=10ms
				// this is the expected steady-state. Fall back to the runtime
				// directly so the benchmark still measures something.
				if _, err := fr.Create(ctx, rt.Spec{}); err != nil {
					b.Fatalf("Create fallback: %v", err)
				}
				continue
			}
			_ = id
		}
	})
}
