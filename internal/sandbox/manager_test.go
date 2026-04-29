package sandbox_test

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"nexteam.id/kotakpasir/internal/policy"
	rt "nexteam.id/kotakpasir/internal/runtime"
	"nexteam.id/kotakpasir/internal/sandbox"
)

// fakeRuntime is a minimal in-memory rt.Runtime used by manager tests. It
// records lifecycle calls without touching Docker so tests stay hermetic.
type fakeRuntime struct {
	mu       sync.Mutex
	created  []rt.Spec
	removed  []string
	createN  atomic.Int64
	createCh chan struct{} // optional, signaled after each Create
	createDelay time.Duration
}

func newFakeRuntime() *fakeRuntime { return &fakeRuntime{} }

func (f *fakeRuntime) Create(ctx context.Context, spec rt.Spec) (rt.Handle, error) {
	if f.createDelay > 0 {
		select {
		case <-time.After(f.createDelay):
		case <-ctx.Done():
			return rt.Handle{}, ctx.Err()
		}
	}
	id := "rt-" + uuid.NewString()
	f.mu.Lock()
	f.created = append(f.created, spec)
	f.mu.Unlock()
	f.createN.Add(1)
	if f.createCh != nil {
		select {
		case f.createCh <- struct{}{}:
		default:
		}
	}
	return rt.Handle{ID: id, Name: spec.Name}, nil
}

func (f *fakeRuntime) Exec(ctx context.Context, id string, _ rt.ExecSpec) (rt.ExecResult, error) {
	return rt.ExecResult{ExitCode: 0}, nil
}

func (f *fakeRuntime) Stop(ctx context.Context, id string) error  { return nil }
func (f *fakeRuntime) Status(ctx context.Context, id string) (rt.Status, error) {
	return rt.Status{State: "running"}, nil
}
func (f *fakeRuntime) Close() error { return nil }

func (f *fakeRuntime) Remove(ctx context.Context, id string) error {
	f.mu.Lock()
	f.removed = append(f.removed, id)
	f.mu.Unlock()
	return nil
}

// memStore is an in-memory sandbox.Store.
type memStore struct {
	mu      sync.Mutex
	entries map[string]sandbox.Sandbox
}

func newMemStore() *memStore { return &memStore{entries: make(map[string]sandbox.Sandbox)} }

func (s *memStore) Put(ctx context.Context, sb sandbox.Sandbox) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.entries[sb.ID] = sb
	return nil
}

func (s *memStore) Get(ctx context.Context, id string) (sandbox.Sandbox, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sb, ok := s.entries[id]
	if !ok {
		return sandbox.Sandbox{}, sandbox.ErrNotFound
	}
	return sb, nil
}

func (s *memStore) List(ctx context.Context) ([]sandbox.Sandbox, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]sandbox.Sandbox, 0, len(s.entries))
	for _, v := range s.entries {
		out = append(out, v)
	}
	return out, nil
}

func (s *memStore) Delete(ctx context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.entries[id]; !ok {
		return sandbox.ErrNotFound
	}
	delete(s.entries, id)
	return nil
}

func (s *memStore) ExpiredBefore(ctx context.Context, t time.Time) ([]sandbox.Sandbox, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []sandbox.Sandbox
	for _, sb := range s.entries {
		if sb.ExpiresAt != nil && sb.ExpiresAt.Before(t) {
			out = append(out, sb)
		}
	}
	return out, nil
}

func (s *memStore) Close() error { return nil }

func defaultsPolicy(images []policy.Image) *policy.Policy {
	return &policy.Policy{
		Version: 1,
		Defaults: policy.Defaults{
			Cpus: 1.0, MemoryMB: 256, PidsLimit: 256,
			User: "1000:1000", ReadOnly: true, NetworkMode: "none",
		},
		Images: images,
		Egress: policy.GlobalEgress{Default: policy.Egress{Mode: policy.EgressNone}},
	}
}

func newTestManager(t *testing.T, fr *fakeRuntime, pol *policy.Policy) *sandbox.Manager {
	t.Helper()
	mgr, err := sandbox.NewManager(sandbox.Options{
		Runtime: fr,
		Store:   newMemStore(),
		Policy:  pol,
	})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	return mgr
}

func TestReplacePolicy_AddsAndRemovesPools(t *testing.T) {
	fr := newFakeRuntime()
	pol := defaultsPolicy([]policy.Image{{Name: "alpine:latest", Pool: 2}})
	mgr := newTestManager(t, fr, pol)

	ctx := t.Context()
	if err := mgr.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if got := fr.createN.Load(); got != 2 {
		t.Fatalf("after Start: createN=%d, want 2", got)
	}

	// Reload: drop alpine pool, add busybox pool of size 1.
	newPol := defaultsPolicy([]policy.Image{
		{Name: "alpine:latest"},
		{Name: "busybox:1", Pool: 1},
	})
	if err := mgr.ReplacePolicy(ctx, newPol); err != nil {
		t.Fatalf("ReplacePolicy: %v", err)
	}
	if got := fr.createN.Load(); got != 3 {
		t.Errorf("after Replace: createN=%d, want 3 (2 alpine warm + 1 busybox warm)", got)
	}

	// Old alpine pool entries should be shut down asynchronously. Wait briefly.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		fr.mu.Lock()
		n := len(fr.removed)
		fr.mu.Unlock()
		if n >= 2 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	fr.mu.Lock()
	got := len(fr.removed)
	fr.mu.Unlock()
	if got < 2 {
		t.Errorf("retired alpine pool not drained: removed=%d, want >=2", got)
	}
}

func TestReplacePolicy_KeepsUnchangedPool(t *testing.T) {
	fr := newFakeRuntime()
	pol := defaultsPolicy([]policy.Image{{Name: "alpine:latest", Pool: 2}})
	mgr := newTestManager(t, fr, pol)
	ctx := t.Context()
	if err := mgr.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	beforeRemove := func() int {
		fr.mu.Lock()
		defer fr.mu.Unlock()
		return len(fr.removed)
	}()
	createdBefore := fr.createN.Load()

	// Reload with an identical alpine entry; pool should be preserved.
	if err := mgr.ReplacePolicy(ctx, defaultsPolicy([]policy.Image{{Name: "alpine:latest", Pool: 2}})); err != nil {
		t.Fatalf("ReplacePolicy: %v", err)
	}
	// Give any (incorrect) async shutdown a moment to land.
	time.Sleep(50 * time.Millisecond)

	if got := fr.createN.Load(); got != createdBefore {
		t.Errorf("createN=%d, want unchanged %d (pool should be preserved)", got, createdBefore)
	}
	fr.mu.Lock()
	got := len(fr.removed)
	fr.mu.Unlock()
	if got != beforeRemove {
		t.Errorf("removed=%d, want unchanged %d", got, beforeRemove)
	}
}

func TestReplacePolicy_InFlightCreateUsesOldPolicy(t *testing.T) {
	fr := newFakeRuntime()
	// Slow Create to give us a window where a request is mid-flight.
	fr.createDelay = 200 * time.Millisecond

	pol := defaultsPolicy([]policy.Image{{Name: "alpine:latest"}})
	mgr := newTestManager(t, fr, pol)
	ctx := t.Context()
	if err := mgr.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}

	// Kick off a Create on the old policy. It should succeed even though the
	// policy gets replaced with one that disallows alpine while it's in flight.
	type result struct {
		sb  sandbox.Sandbox
		err error
	}
	done := make(chan result, 1)
	go func() {
		sb, err := mgr.Create(ctx, sandbox.CreateOptions{Image: "alpine:latest"})
		done <- result{sb, err}
	}()

	// Make sure the goroutine has entered Create before we swap.
	time.Sleep(20 * time.Millisecond)

	// Replace with a policy that doesn't allow alpine.
	newPol := defaultsPolicy([]policy.Image{{Name: "busybox:1"}})
	if err := mgr.ReplacePolicy(ctx, newPol); err != nil {
		t.Fatalf("ReplacePolicy: %v", err)
	}

	res := <-done
	if res.err != nil {
		t.Fatalf("in-flight Create failed under reload: %v", res.err)
	}
	if res.sb.Image != "alpine:latest" {
		t.Errorf("sandbox image=%q, want alpine:latest", res.sb.Image)
	}

	// New requests should now be denied.
	_, err := mgr.Create(ctx, sandbox.CreateOptions{Image: "alpine:latest"})
	if err == nil {
		t.Fatal("post-reload Create for alpine: want policy violation, got nil")
	}
	if !errors.Is(err, policy.ErrPolicyViolation) {
		t.Errorf("err=%v, want ErrPolicyViolation", err)
	}
}

func TestReplacePolicy_RejectsInvalid(t *testing.T) {
	fr := newFakeRuntime()
	mgr := newTestManager(t, fr, defaultsPolicy(nil))

	bad := &policy.Policy{Version: 999} // unsupported version
	if err := mgr.ReplacePolicy(t.Context(), bad); err == nil {
		t.Fatal("ReplacePolicy(invalid): want error, got nil")
	}
	if err := mgr.ReplacePolicy(t.Context(), nil); err == nil {
		t.Fatal("ReplacePolicy(nil): want error, got nil")
	}
}

func TestReplacePolicy_BeforeStartDoesNotWarm(t *testing.T) {
	fr := newFakeRuntime()
	mgr := newTestManager(t, fr, defaultsPolicy([]policy.Image{{Name: "alpine:latest"}}))

	// Replace before Start — should record the new policy but not spawn pool entries.
	if err := mgr.ReplacePolicy(t.Context(), defaultsPolicy([]policy.Image{{Name: "busybox:1", Pool: 2}})); err != nil {
		t.Fatalf("ReplacePolicy: %v", err)
	}
	if got := fr.createN.Load(); got != 0 {
		t.Errorf("createN=%d before Start, want 0", got)
	}

	if err := mgr.Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if got := fr.createN.Load(); got != 2 {
		t.Errorf("createN=%d after Start, want 2 (busybox pool warmed)", got)
	}
}

// Sanity: Start/Shutdown ordering — pool entries warmed at Start are drained
// at Shutdown.
func TestStartShutdownOrdering(t *testing.T) {
	fr := newFakeRuntime()
	mgr := newTestManager(t, fr, defaultsPolicy([]policy.Image{{Name: "alpine:latest", Pool: 3}}))

	if err := mgr.Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if got := fr.createN.Load(); got != 3 {
		t.Errorf("createN=%d, want 3", got)
	}
	if err := mgr.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	fr.mu.Lock()
	got := len(fr.removed)
	fr.mu.Unlock()
	if got != 3 {
		t.Errorf("removed=%d after Shutdown, want 3", got)
	}
}

// TestStart_DoubleShutdown confirms calling Shutdown twice is a no-op rather
// than a double-remove.
func TestStart_DoubleShutdown(t *testing.T) {
	fr := newFakeRuntime()
	mgr := newTestManager(t, fr, defaultsPolicy([]policy.Image{{Name: "alpine:latest", Pool: 2}}))

	if err := mgr.Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := mgr.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown 1: %v", err)
	}
	if err := mgr.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown 2: %v", err)
	}
	fr.mu.Lock()
	got := len(fr.removed)
	fr.mu.Unlock()
	if got != 2 {
		t.Errorf("removed=%d after double Shutdown, want 2 (no double-remove)", got)
	}
}

// TestShutdownWithoutStart confirms Shutdown is safe before Start — it should
// be a no-op rather than panic on nil pool state.
func TestShutdownWithoutStart(t *testing.T) {
	fr := newFakeRuntime()
	mgr := newTestManager(t, fr, defaultsPolicy([]policy.Image{{Name: "alpine:latest", Pool: 2}}))
	if err := mgr.Shutdown(context.Background()); err != nil {
		t.Errorf("Shutdown without Start: %v", err)
	}
	if got := fr.createN.Load(); got != 0 {
		t.Errorf("createN=%d, want 0", got)
	}
}

// Make sure we don't accidentally drift from the runtime interface.
var _ rt.Runtime = (*fakeRuntime)(nil)
var _ sandbox.Store = (*memStore)(nil)
