// Package runtimetest is a contract test suite that any Runtime backend
// (docker, runsc, kata, firecracker) must pass. Use it from a backend-specific
// _test.go file so each backend stays honest about the interface.
package runtimetest

import (
	"context"
	"strings"
	"testing"
	"time"

	rt "nexteam.id/kotakpasir/internal/runtime"
)

// TestImage is the OCI image used by the suite. Contributors must have it
// pulled locally (or have network access at test time).
const TestImage = "alpine:latest"

// Suite runs the conformance test suite against any Runtime.
// makeRuntime is called fresh for each subtest.
func Suite(t *testing.T, makeRuntime func(t *testing.T) rt.Runtime) {
	t.Helper()

	t.Run("Lifecycle", func(t *testing.T) { testLifecycle(t, makeRuntime(t)) })
	t.Run("ExecStreams", func(t *testing.T) { testExecStreams(t, makeRuntime(t)) })
	t.Run("Hardening", func(t *testing.T) { testHardening(t, makeRuntime(t)) })
	t.Run("EgressAllowlist", func(t *testing.T) { testEgress(t, makeRuntime(t)) })
}

func basicSpec() rt.Spec {
	return rt.Spec{
		Image:       TestImage,
		User:        "1000:1000",
		ReadOnly:    true,
		NetworkMode: "none",
		PidsLimit:   256,
		Cpus:        1.0,
		MemoryMB:    256,
	}
}

func sandboxIDSpec(id string) rt.Spec {
	s := basicSpec()
	s.Name = id
	s.Labels = map[string]string{"kotakpasir.sandbox-id": id}
	return s
}

func testLifecycle(t *testing.T, r rt.Runtime) {
	ctx := t.Context()
	t.Cleanup(func() { _ = r.Close() })

	h, err := r.Create(ctx, basicSpec())
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	t.Cleanup(func() { _ = r.Remove(ctx, h.ID) })

	if h.ID == "" {
		t.Fatal("Create returned empty ID")
	}

	res, err := r.Exec(ctx, h.ID, rt.ExecSpec{
		Cmd: []string{"sh", "-c", "echo hi"},
	})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if res.ExitCode != 0 {
		t.Errorf("ExitCode=%d, want 0", res.ExitCode)
	}
	if res.Stdout != "hi\n" {
		t.Errorf("Stdout=%q, want %q", res.Stdout, "hi\n")
	}
	if res.Duration <= 0 {
		t.Errorf("Duration=%v, want >0", res.Duration)
	}

	if err := r.Stop(ctx, h.ID); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	st, err := r.Status(ctx, h.ID)
	if err != nil {
		t.Fatalf("Status after Stop: %v", err)
	}
	if st.State == "running" {
		t.Errorf("State after Stop = %q, want non-running", st.State)
	}

	if err := r.Remove(ctx, h.ID); err != nil {
		t.Fatalf("Remove: %v", err)
	}
}

func testExecStreams(t *testing.T, r rt.Runtime) {
	ctx := t.Context()
	t.Cleanup(func() { _ = r.Close() })

	h, err := r.Create(ctx, basicSpec())
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	t.Cleanup(func() { _ = r.Remove(ctx, h.ID) })

	res, err := r.Exec(ctx, h.ID, rt.ExecSpec{
		Cmd: []string{"sh", "-c", "echo out; echo err >&2; exit 7"},
	})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if res.ExitCode != 7 {
		t.Errorf("ExitCode=%d, want 7", res.ExitCode)
	}
	if !strings.Contains(res.Stdout, "out") {
		t.Errorf("Stdout=%q, want to contain 'out'", res.Stdout)
	}
	if !strings.Contains(res.Stderr, "err") {
		t.Errorf("Stderr=%q, want to contain 'err'", res.Stderr)
	}
	if strings.Contains(res.Stdout, "err") {
		t.Errorf("Stderr leaked into Stdout: %q", res.Stdout)
	}
}

func testHardening(t *testing.T, r rt.Runtime) {
	ctx := t.Context()
	t.Cleanup(func() { _ = r.Close() })

	h, err := r.Create(ctx, basicSpec())
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	t.Cleanup(func() { _ = r.Remove(ctx, h.ID) })

	res, err := r.Exec(ctx, h.ID, rt.ExecSpec{
		Cmd: []string{"sh", "-c", "touch /opt/x 2>&1; echo done"},
	})
	if err != nil {
		t.Fatalf("Exec ro: %v", err)
	}
	if !strings.Contains(res.Stdout, "Read-only") && !strings.Contains(res.Stdout, "Permission denied") {
		t.Errorf("Read-only rootfs not enforced: stdout=%q", res.Stdout)
	}

	res, err = r.Exec(ctx, h.ID, rt.ExecSpec{
		Cmd: []string{"sh", "-c", "touch /tmp/y && echo tmp-ok"},
	})
	if err != nil {
		t.Fatalf("Exec tmpfs: %v", err)
	}
	if !strings.Contains(res.Stdout, "tmp-ok") {
		t.Errorf("/tmp not writable: stdout=%q", res.Stdout)
	}

	res, err = r.Exec(ctx, h.ID, rt.ExecSpec{Cmd: []string{"id", "-u"}})
	if err != nil {
		t.Fatalf("Exec id -u: %v", err)
	}
	if strings.TrimSpace(res.Stdout) != "1000" {
		t.Errorf("UID=%q, want 1000", strings.TrimSpace(res.Stdout))
	}

	netCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	res, err = r.Exec(netCtx, h.ID, rt.ExecSpec{
		Cmd: []string{"sh", "-c", "wget -qO- --timeout=2 http://1.1.1.1 2>&1; echo done"},
	})
	if err != nil {
		t.Fatalf("Exec net: %v", err)
	}
	if !strings.Contains(res.Stdout, "unreachable") &&
		!strings.Contains(res.Stdout, "bad address") &&
		!strings.Contains(res.Stdout, "timed out") {
		t.Errorf("network=none not enforced: stdout=%q", res.Stdout)
	}
}

func testEgress(t *testing.T, r rt.Runtime) {
	ctx := t.Context()
	t.Cleanup(func() { _ = r.Close() })

	id := "egress-test-" + randHex(8)
	spec := sandboxIDSpec(id)
	spec.Egress = rt.Egress{Mode: "allowlist", Hosts: []string{"example.com"}}

	h, err := r.Create(ctx, spec)
	if err != nil {
		t.Skipf("egress proxy not available (build with: docker build -f Dockerfile.kpproxy -t kotakpasir/proxy:dev .): %v", err)
	}
	t.Cleanup(func() { _ = r.Remove(ctx, h.ID) })

	netCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	// 1. Sandbox can reach the proxy (intra-network OK)
	res, err := r.Exec(netCtx, h.ID, rt.ExecSpec{
		Cmd: []string{"sh", "-c", "nc -w 2 proxy 8080 </dev/null >/dev/null 2>&1; echo exit=$?"},
	})
	if err != nil {
		t.Fatalf("Exec proxy-reach: %v", err)
	}
	if !strings.Contains(res.Stdout, "exit=0") {
		t.Errorf("sandbox cannot reach proxy: stdout=%q", res.Stdout)
	}

	// 2. Sandbox cannot reach the internet directly — internal network has no route
	res, err = r.Exec(netCtx, h.ID, rt.ExecSpec{
		Cmd: []string{"sh", "-c", "nc -w 2 1.1.1.1 443 </dev/null >/dev/null 2>&1; echo exit=$?"},
	})
	if err != nil {
		t.Fatalf("Exec direct-egress: %v", err)
	}
	if strings.Contains(res.Stdout, "exit=0") {
		t.Errorf("sandbox reached internet directly without proxy: stdout=%q", res.Stdout)
	}

	// 3. Proxy allows CONNECT to allowlisted host
	res, err = r.Exec(netCtx, h.ID, rt.ExecSpec{
		Cmd: []string{"sh", "-c", "printf 'CONNECT example.com:443 HTTP/1.1\\r\\nHost: example.com:443\\r\\n\\r\\n' | nc -w 3 proxy 8080 | head -1"},
	})
	if err != nil {
		t.Fatalf("Exec connect-allowed: %v", err)
	}
	if !strings.Contains(res.Stdout, "200") {
		t.Errorf("proxy did not allow allowlisted host (want 200): stdout=%q", res.Stdout)
	}

	// 4. Proxy rejects CONNECT to non-allowlisted host
	res, err = r.Exec(netCtx, h.ID, rt.ExecSpec{
		Cmd: []string{"sh", "-c", "printf 'CONNECT 1.1.1.1:443 HTTP/1.1\\r\\nHost: 1.1.1.1:443\\r\\n\\r\\n' | nc -w 3 proxy 8080 | head -1"},
	})
	if err != nil {
		t.Fatalf("Exec connect-denied: %v", err)
	}
	if strings.Contains(res.Stdout, "200") {
		t.Errorf("proxy allowed non-allowlisted host: stdout=%q", res.Stdout)
	}
}

func randHex(n int) string {
	const chars = "0123456789abcdef"
	b := make([]byte, n)
	for i := range b {
		b[i] = chars[time.Now().UnixNano()%int64(len(chars))]
		time.Sleep(time.Microsecond)
	}
	return string(b)
}
