package sandbox_test

import (
	"context"
	"slices"
	"testing"
	"time"

	"nexteam.id/kotakpasir/internal/policy"
	"nexteam.id/kotakpasir/internal/sandbox"
)

// TestReaper_TTLGC_EndToEnd creates a sandbox with a short TTL, runs the
// reaper, and confirms the sandbox is removed from the store and the runtime.
// Exercises the full Manager → Reaper → Store → Runtime path.
func TestReaper_TTLGC_EndToEnd(t *testing.T) {
	fr := newFakeRuntime()
	pol := defaultsPolicy([]policy.Image{{Name: "alpine:latest"}})

	store := newMemStore()
	mgr, err := sandbox.NewManager(sandbox.Options{Runtime: fr, Store: store, Policy: pol})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}

	sb, err := mgr.Create(t.Context(), sandbox.CreateOptions{
		Image: "alpine:latest",
		TTL:   50 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if sb.ExpiresAt == nil {
		t.Fatalf("ExpiresAt is nil for TTL'd sandbox")
	}

	// Sanity: present in store before TTL elapses.
	if _, err := mgr.Get(t.Context(), sb.ID); err != nil {
		t.Fatalf("Get before reap: %v", err)
	}

	reaper := sandbox.NewReaper(mgr, 20*time.Millisecond)
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()

	go func() { _ = reaper.Run(ctx) }()

	// Wait for the sandbox to disappear from the store.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		_, err := mgr.Get(context.Background(), sb.ID)
		if err == sandbox.ErrNotFound {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	_, err = mgr.Get(context.Background(), sb.ID)
	if err != sandbox.ErrNotFound {
		t.Fatalf("after reap: Get err=%v, want ErrNotFound", err)
	}

	// Runtime should have been told to remove the underlying container.
	fr.mu.Lock()
	removed := append([]string(nil), fr.removed...)
	fr.mu.Unlock()
	if !slices.Contains(removed, sb.RuntimeID) {
		t.Errorf("runtime id %q not removed; removed=%v", sb.RuntimeID, removed)
	}
}

// TestReaper_NoTTL_NotReaped confirms sandboxes without a TTL are left alone.
func TestReaper_NoTTL_NotReaped(t *testing.T) {
	fr := newFakeRuntime()
	mgr, err := sandbox.NewManager(sandbox.Options{
		Runtime: fr, Store: newMemStore(),
		Policy: defaultsPolicy([]policy.Image{{Name: "alpine:latest"}}),
	})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	sb, err := mgr.Create(t.Context(), sandbox.CreateOptions{Image: "alpine:latest"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	reaper := sandbox.NewReaper(mgr, 20*time.Millisecond)
	ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
	defer cancel()
	_ = reaper.Run(ctx)

	if _, err := mgr.Get(context.Background(), sb.ID); err != nil {
		t.Errorf("non-TTL sandbox got reaped: err=%v", err)
	}
}
