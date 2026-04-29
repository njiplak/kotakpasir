package sandbox

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"nexteam.id/kotakpasir/internal/metrics"
	"nexteam.id/kotakpasir/internal/pool"
	"nexteam.id/kotakpasir/internal/policy"
	rt "nexteam.id/kotakpasir/internal/runtime"
	"nexteam.id/kotakpasir/internal/sandbox/logbuf"
)

// DefaultLogBufferBytes is the per-sandbox cap on captured exec output when
// the operator hasn't overridden it. Keeps memory bounded; exec output beyond
// this is evicted FIFO.
const DefaultLogBufferBytes = 256 * 1024

type Manager struct {
	rt     rt.Runtime
	store  Store
	policy *policy.Policy
	pools  map[string]*pool.Pool

	metrics metrics.Recorder

	logCapBytes int
	logsMu      sync.Mutex
	logs        map[string]*logbuf.Buffer
}

type Options struct {
	Runtime rt.Runtime
	Store   Store
	Policy  *policy.Policy
	// LogBufferBytes caps captured exec output per sandbox. Zero uses
	// DefaultLogBufferBytes; negative disables capture.
	LogBufferBytes int
	// Metrics receives lifecycle events. Nil falls back to a Noop recorder.
	Metrics metrics.Recorder
}

func NewManager(opts Options) (*Manager, error) {
	if opts.Runtime == nil {
		return nil, errors.New("runtime is required")
	}
	if opts.Store == nil {
		return nil, errors.New("store is required")
	}
	if opts.Policy == nil {
		opts.Policy = policy.Default()
	}
	logCap := opts.LogBufferBytes
	if logCap == 0 {
		logCap = DefaultLogBufferBytes
	}
	rec := opts.Metrics
	if rec == nil {
		rec = metrics.NewNoop()
	}

	m := &Manager{
		rt:          opts.Runtime,
		store:       opts.Store,
		policy:      opts.Policy,
		pools:       make(map[string]*pool.Pool),
		metrics:     rec,
		logCapBytes: logCap,
		logs:        make(map[string]*logbuf.Buffer),
	}
	m.buildPools()
	return m, nil
}

// logBufferFor returns the buffer for a sandbox, creating it lazily on first
// use. Returns nil when capture is disabled (logCapBytes <= 0).
func (m *Manager) logBufferFor(id string) *logbuf.Buffer {
	if m.logCapBytes <= 0 {
		return nil
	}
	m.logsMu.Lock()
	defer m.logsMu.Unlock()
	b, ok := m.logs[id]
	if !ok {
		b = logbuf.New(m.logCapBytes)
		m.logs[id] = b
	}
	return b
}

// dropLogBuffer removes the buffer for a sandbox; called on Delete.
func (m *Manager) dropLogBuffer(id string) {
	m.logsMu.Lock()
	defer m.logsMu.Unlock()
	delete(m.logs, id)
}

// buildPools constructs a Pool for each image in the policy that has
// pool > 0 and a compatible egress mode.
func (m *Manager) buildPools() {
	for _, img := range m.policy.Images {
		if img.Pool <= 0 {
			continue
		}
		if img.Egress != nil && img.Egress.Mode != "" && img.Egress.Mode != policy.EgressNone {
			continue
		}
		spec := m.poolSpecFor(img)
		m.pools[img.Name] = pool.New(m.rt, pool.Config{
			Image:  img.Name,
			Target: img.Pool,
		}, spec)
	}
}

// poolSpecFor builds the runtime.Spec used to warm pool entries for an image.
// Resource limits come from the image entry if set, otherwise policy defaults.
// Pool entries are created without per-sandbox identifiers — those are assigned
// only when the entry is claimed.
func (m *Manager) poolSpecFor(img policy.Image) rt.Spec {
	cpus := cmp.Or(img.Cpus, m.policy.Defaults.Cpus)
	memMB := cmp.Or(img.MemoryMB, m.policy.Defaults.MemoryMB)
	return rt.Spec{
		Image:       img.Name,
		Cpus:        cpus,
		MemoryMB:    memMB,
		PidsLimit:   m.policy.Defaults.PidsLimit,
		User:        m.policy.Defaults.User,
		ReadOnly:    m.policy.Defaults.ReadOnly,
		NetworkMode: m.policy.Defaults.NetworkMode,
		RuntimeName: m.policy.Defaults.Runtime,
	}
}

// orphanCleaner is implemented by runtimes that can clean up stale pool
// containers from a previous process lifetime. Optional capability.
type orphanCleaner interface {
	CleanPoolOrphans(ctx context.Context) error
}

// imageEnsurer is implemented by runtimes that can pre-pull images. Optional
// capability — non-Docker backends (e.g. Firecracker) won't satisfy it.
type imageEnsurer interface {
	EnsureImage(ctx context.Context, ref string) (pulled bool, err error)
}

// proxyAddrer is implemented by runtimes that provision per-sandbox egress
// proxies and can report a routable address for them. Optional capability.
type proxyAddrer interface {
	ProxyAddr(ctx context.Context, sandboxID string) (string, error)
}

// pinger is implemented by runtimes/stores that can confirm reachability.
// Used by /healthz; absent capability is treated as "unknown, assume ok".
type pinger interface {
	Ping(ctx context.Context) error
}

// HealthReport summarizes the readiness of every subsystem the manager
// depends on. status="ok" iff every check has Err==nil; otherwise "degraded".
type HealthReport struct {
	Status string                 `json:"status"`
	Checks map[string]HealthCheck `json:"checks"`
}

type HealthCheck struct {
	OK     bool   `json:"ok"`
	Detail string `json:"detail,omitempty"`
}

// Health runs every subsystem check and returns the aggregate.
func (m *Manager) Health(ctx context.Context) HealthReport {
	checks := map[string]HealthCheck{}
	overallOK := true

	addCheck := func(name string, err error, ok bool) {
		c := HealthCheck{OK: ok}
		if err != nil {
			c.OK = false
			c.Detail = err.Error()
		}
		if !c.OK {
			overallOK = false
		}
		checks[name] = c
	}

	if p, ok := m.store.(pinger); ok {
		addCheck("store", p.Ping(ctx), true)
	} else {
		addCheck("store", nil, true)
	}

	if p, ok := m.rt.(pinger); ok {
		addCheck("runtime", p.Ping(ctx), true)
	} else {
		addCheck("runtime", nil, true)
	}

	for image, p := range m.pools {
		// A pool is healthy if it has at least one warm entry available — the
		// async refill goroutine handles the upper bound. Zero with a target
		// > 0 means everything's claimed AND the refill hasn't caught up.
		ok := p.Available() > 0 || p.Target() == 0
		var err error
		if !ok {
			err = fmt.Errorf("0/%d warm", p.Target())
		}
		addCheck("pool:"+image, err, ok)
	}

	status := "ok"
	if !overallOK {
		status = "degraded"
	}
	return HealthReport{Status: status, Checks: checks}
}

// Start eagerly warms every configured pool. Safe to call once after construction.
// If the runtime supports it, also cleans up stale pool containers from earlier
// kpd runs and pre-pulls every declared image before warming, so a cold registry
// surfaces as explicit log lines instead of a silent pool-warmup stall.
func (m *Manager) Start(ctx context.Context) error {
	if oc, ok := m.rt.(orphanCleaner); ok {
		if err := oc.CleanPoolOrphans(ctx); err != nil {
			slog.Warn("pool orphan cleanup", "err", err)
		}
	}
	if ie, ok := m.rt.(imageEnsurer); ok {
		for _, img := range m.policy.Images {
			start := time.Now()
			pulled, err := ie.EnsureImage(ctx, img.Name)
			if err != nil {
				return fmt.Errorf("ensure image %s: %w", img.Name, err)
			}
			if pulled {
				slog.Info("image pulled", "image", img.Name, "duration", time.Since(start).Round(time.Millisecond))
			} else {
				slog.Info("image present", "image", img.Name)
			}
		}
	}
	for image, p := range m.pools {
		if err := p.Start(ctx); err != nil {
			return fmt.Errorf("pool %s: %w", image, err)
		}
		slog.Info("warm pool ready", "image", image, "target", m.pools[image].Available())
	}
	return nil
}

// Shutdown drains every pool. Use a background context — typically called on
// kpd shutdown when the parent context is already canceled.
func (m *Manager) Shutdown(ctx context.Context) error {
	var errs []error
	for image, p := range m.pools {
		if err := p.Shutdown(ctx); err != nil {
			errs = append(errs, fmt.Errorf("pool %s: %w", image, err))
		}
	}
	return errors.Join(errs...)
}

func (m *Manager) Create(ctx context.Context, opts CreateOptions) (Sandbox, error) {
	resolved, err := m.policy.Resolve(policy.Request{
		Profile:  opts.Profile,
		Image:    opts.Image,
		Cmd:      opts.Cmd,
		Env:      opts.Env,
		Cpus:     opts.Cpus,
		MemoryMB: opts.MemoryMB,
		TTL:      opts.TTL,
		Name:     opts.Name,
		Labels:   opts.Labels,
	})
	if err != nil {
		m.metrics.SandboxCreated(opts.Image, opts.Profile, "policy_denied")
		return Sandbox{}, err
	}

	now := time.Now().UTC()
	sb := Sandbox{
		ID:        uuid.NewString(),
		Name:      resolved.Name,
		Image:     resolved.Image,
		State:     StateCreated,
		Cpus:      resolved.Cpus,
		MemoryMB:  resolved.MemoryMB,
		Env:       resolved.Env,
		Labels:    resolved.Labels,
		CreatedAt: now,
	}
	if resolved.TTL > 0 {
		exp := now.Add(resolved.TTL)
		sb.ExpiresAt = &exp
	}

	spec := rt.Spec{
		Name:        sb.ID,
		Image:       resolved.Image,
		Cmd:         resolved.Cmd,
		Env:         resolved.Env,
		Cpus:        resolved.Cpus,
		MemoryMB:    resolved.MemoryMB,
		PidsLimit:   resolved.PidsLimit,
		User:        resolved.User,
		ReadOnly:    resolved.ReadOnly,
		NetworkMode: resolved.NetworkMode,
		RuntimeName: resolved.RuntimeName,
		Egress:      rt.Egress{Mode: resolved.Egress.Mode, Hosts: resolved.Egress.Hosts},
		Labels:      mergeLabels(resolved.Labels, sb.ID),
		TTL:         resolved.TTL,
	}

	log := slog.With("sandbox_id", sb.ID, "image", resolved.Image)

	if id, ok := m.tryClaim(ctx, resolved); ok {
		sb.RuntimeID = id
		sb.State = StateRunning
		if err := m.store.Put(ctx, sb); err != nil {
			return sb, fmt.Errorf("store put: %w", err)
		}
		m.metrics.PoolHit(resolved.Image)
		m.metrics.SandboxCreated(resolved.Image, opts.Profile, "success")
		m.refreshActive(ctx)
		log.Info("sandbox created", "source", "pool", "profile", opts.Profile)
		return sb, nil
	}

	handle, err := m.rt.Create(ctx, spec)
	if err != nil {
		sb.State = StateError
		_ = m.store.Put(ctx, sb)
		m.metrics.SandboxCreated(resolved.Image, opts.Profile, "runtime_error")
		log.Error("sandbox create failed", "err", err)
		return sb, fmt.Errorf("runtime create: %w", err)
	}
	sb.RuntimeID = handle.ID
	sb.State = StateRunning

	if err := m.store.Put(ctx, sb); err != nil {
		return sb, fmt.Errorf("store put: %w", err)
	}
	m.metrics.PoolMiss(resolved.Image)
	m.metrics.SandboxCreated(resolved.Image, opts.Profile, "success")
	m.refreshActive(ctx)
	log.Info("sandbox created", "source", "cold", "profile", opts.Profile)
	return sb, nil
}

// refreshActive updates the kpd_sandbox_active gauge from the store. Cheap
// (one COUNT query) and called only on Create/Delete, not in hot paths.
func (m *Manager) refreshActive(ctx context.Context) {
	list, err := m.store.List(ctx)
	if err != nil {
		return
	}
	n := 0
	for _, sb := range list {
		if sb.State == StateRunning {
			n++
		}
	}
	m.metrics.SetSandboxActive(n)
}

// tryClaim returns a warm container id if the resolved spec exactly matches
// what the pool was warmed for. Mismatched requests cold-start instead.
func (m *Manager) tryClaim(ctx context.Context, resolved policy.Resolved) (string, bool) {
	p, ok := m.pools[resolved.Image]
	if !ok {
		return "", false
	}
	if !m.poolMatchesResolved(resolved) {
		return "", false
	}
	return p.Get(ctx)
}

func (m *Manager) poolMatchesResolved(resolved policy.Resolved) bool {
	if resolved.Egress.Mode != "" && resolved.Egress.Mode != policy.EgressNone {
		return false
	}
	img, ok := m.findImage(resolved.Image)
	if !ok {
		return false
	}
	expectedCpus := cmp.Or(img.Cpus, m.policy.Defaults.Cpus)
	expectedMem := cmp.Or(img.MemoryMB, m.policy.Defaults.MemoryMB)
	return resolved.Cpus == expectedCpus && resolved.MemoryMB == expectedMem
}

func (m *Manager) findImage(name string) (policy.Image, bool) {
	for _, img := range m.policy.Images {
		if img.Name == name {
			return img, true
		}
	}
	return policy.Image{}, false
}

func (m *Manager) Get(ctx context.Context, id string) (Sandbox, error) {
	return m.store.Get(ctx, id)
}

// ProxyAddr returns the egress proxy's routable address for the given sandbox,
// or rt.ErrNoProxy if the sandbox has no proxy (egress=none, or runtime backend
// doesn't support proxies). Returns ErrNotFound if the sandbox itself doesn't exist.
func (m *Manager) ProxyAddr(ctx context.Context, id string) (string, error) {
	sb, err := m.store.Get(ctx, id)
	if err != nil {
		return "", err
	}
	pa, ok := m.rt.(proxyAddrer)
	if !ok {
		return "", rt.ErrNoProxy
	}
	return pa.ProxyAddr(ctx, sb.ID)
}

func (m *Manager) List(ctx context.Context) ([]Sandbox, error) {
	return m.store.List(ctx)
}

func (m *Manager) ExpiredBefore(ctx context.Context, t time.Time) ([]Sandbox, error) {
	return m.store.ExpiredBefore(ctx, t)
}

func (m *Manager) Exec(ctx context.Context, id string, opts ExecOptions) (ExecResult, error) {
	sb, err := m.store.Get(ctx, id)
	if err != nil {
		return ExecResult{}, err
	}
	if sb.State != StateRunning {
		return ExecResult{}, fmt.Errorf("sandbox %s is %s, not running", id, sb.State)
	}

	logs := m.logBufferFor(id)

	stdoutW, stderrW := opts.Stdout, opts.Stderr
	if logs != nil {
		// Tee both streams into the ring buffer. Streaming writers (passed by
		// SSE handlers) get tee'd live; buffered execs (writers nil) get the
		// runtime's res.Stdout/Stderr appended after the call.
		if stdoutW != nil {
			stdoutW = io.MultiWriter(stdoutW, logs.WriteStdout())
		}
		if stderrW != nil {
			stderrW = io.MultiWriter(stderrW, logs.WriteStderr())
		}
	}

	espec := rt.ExecSpec{
		Cmd:     opts.Cmd,
		Env:     opts.Env,
		WorkDir: opts.WorkDir,
		Stdout:  stdoutW,
		Stderr:  stderrW,
	}
	if opts.Stdin != "" {
		espec.Stdin = strings.NewReader(opts.Stdin)
	}
	res, err := m.rt.Exec(ctx, sb.RuntimeID, espec)
	if err != nil {
		return ExecResult{}, err
	}

	if logs != nil {
		if opts.Stdout == nil && res.Stdout != "" {
			logs.AppendStdout([]byte(res.Stdout))
		}
		if opts.Stderr == nil && res.Stderr != "" {
			logs.AppendStderr([]byte(res.Stderr))
		}
	}

	return ExecResult{
		ExitCode: res.ExitCode,
		Stdout:   res.Stdout,
		Stderr:   res.Stderr,
		Duration: res.Duration,
	}, nil
}

// LogsOptions controls Manager.Logs reads.
type LogsOptions struct {
	// TailLines, if > 0, returns only the last N lines of the snapshot.
	TailLines int
	// Follow, if true, keeps the channel open with new entries until ctx cancels.
	// The returned subscribe fn is non-nil only when Follow=true.
	Follow bool
}

// LogsResult is the snapshot returned by Manager.Logs. When Follow was set,
// Subscribe yields entries arriving after the snapshot — call Cancel to release
// resources. The snapshot honors TailLines.
type LogsResult struct {
	Snapshot []logbuf.Entry
	// Subscribe channel — nil unless Follow=true.
	Subscribe <-chan logbuf.Entry
	// Cancel releases the subscription. Safe to call when Subscribe is nil.
	Cancel func()
}

// Logs returns captured exec output for a sandbox.
func (m *Manager) Logs(ctx context.Context, id string, opts LogsOptions) (LogsResult, error) {
	if _, err := m.store.Get(ctx, id); err != nil {
		return LogsResult{}, err
	}
	buf := m.logBufferFor(id)
	if buf == nil {
		return LogsResult{Cancel: func() {}}, nil
	}

	res := LogsResult{Cancel: func() {}}
	if opts.Follow {
		// Subscribe BEFORE snapshotting so we don't miss writes that land in
		// the gap. Possible duplication of the boundary entry is acceptable
		// for a tail-style consumer.
		ch, cancel := buf.Subscribe()
		res.Subscribe = ch
		res.Cancel = cancel
	}
	snap := buf.Snapshot()
	if opts.TailLines > 0 {
		snap = tailLines(snap, opts.TailLines)
	}
	res.Snapshot = snap
	return res, nil
}

// tailLines returns the trailing entries of snap that contain at most n line
// breaks. Approximate — counts \n boundaries across entries.
func tailLines(snap []logbuf.Entry, n int) []logbuf.Entry {
	if n <= 0 || len(snap) == 0 {
		return snap
	}
	count := 0
	for i := len(snap) - 1; i >= 0; i-- {
		count += strings.Count(string(snap[i].Data), "\n")
		if count >= n {
			return snap[i:]
		}
	}
	return snap
}

func (m *Manager) Stop(ctx context.Context, id string) error {
	sb, err := m.store.Get(ctx, id)
	if err != nil {
		return err
	}
	if err := m.rt.Stop(ctx, sb.RuntimeID); err != nil {
		return err
	}
	sb.State = StateStopped
	if err := m.store.Put(ctx, sb); err != nil {
		return err
	}
	m.refreshActive(ctx)
	slog.Info("sandbox stopped", "sandbox_id", id)
	return nil
}

func (m *Manager) Delete(ctx context.Context, id string) error {
	sb, err := m.store.Get(ctx, id)
	if err != nil {
		return err
	}
	if sb.RuntimeID != "" {
		_ = m.rt.Remove(ctx, sb.RuntimeID)
	}
	if err := m.store.Delete(ctx, id); err != nil {
		return err
	}
	m.dropLogBuffer(id)
	m.refreshActive(ctx)
	slog.Info("sandbox deleted", "sandbox_id", id, "image", sb.Image)
	return nil
}

func mergeLabels(in map[string]string, sandboxID string) map[string]string {
	out := make(map[string]string, len(in)+2)
	maps.Copy(out, in)
	out["kotakpasir.sandbox-id"] = sandboxID
	out["kotakpasir.managed"] = "true"
	return out
}
