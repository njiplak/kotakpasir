// Quickstart for the kotakpasir Go SDK. Run kpd separately, then:
//
//	go run ./examples/quickstart
//
// Demonstrates: create, exec (buffered), exec (streaming), delete.
package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"time"

	"nexteam.id/kotakpasir/pkg/kotakpasir"
)

func main() {
	addr := envOr("KPD_ADDR", "http://127.0.0.1:8080")
	c := kotakpasir.New(addr, kotakpasir.WithToken(os.Getenv("KPD_TOKEN")))

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	if err := c.Health(ctx); err != nil {
		log.Fatalf("kpd unreachable at %s: %v", addr, err)
	}

	fmt.Println("--- create ---")
	sb, err := c.Create(ctx, kotakpasir.CreateOptions{Image: "alpine:latest"})
	if err != nil {
		log.Fatalf("create: %v", err)
	}
	fmt.Printf("sandbox %s state=%s\n", sb.ID, sb.State)
	defer func() {
		_ = c.Delete(context.Background(), sb.ID)
		fmt.Printf("--- deleted %s ---\n", sb.ID)
	}()

	fmt.Println("\n--- buffered exec ---")
	res, err := c.Exec(ctx, sb.ID, kotakpasir.ExecOptions{
		Cmd: []string{"sh", "-c", "echo hello; echo problem >&2; exit 0"},
	})
	if err != nil {
		log.Fatalf("exec: %v", err)
	}
	fmt.Printf("exit=%d duration=%v\n", res.ExitCode, res.Duration)
	fmt.Printf("stdout: %q\nstderr: %q\n", res.Stdout, res.Stderr)

	fmt.Println("\n--- streaming exec (3-second loop) ---")
	streamRes, err := c.ExecStream(ctx, sb.ID, kotakpasir.ExecOptions{
		Cmd: []string{"sh", "-c", "for i in 1 2 3; do echo step $i; sleep 1; done"},
	}, &prefixWriter{prefix: "  out| "}, &prefixWriter{prefix: "  err| "})
	if err != nil {
		log.Fatalf("exec stream: %v", err)
	}
	fmt.Printf("exit=%d duration=%v\n", streamRes.ExitCode, streamRes.Duration)
}

// prefixWriter prepends a prefix to every chunk to make stdout vs stderr
// visually distinguishable in the example output.
type prefixWriter struct{ prefix string }

func (p *prefixWriter) Write(b []byte) (int, error) {
	fmt.Printf("%s%s", p.prefix, string(b))
	return len(b), nil
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
