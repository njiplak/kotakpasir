// streaming-pipeline shows the realistic shape of an agent flow:
//
//	1. Create a sandbox
//	2. Wait for it to be running (handles cold-start cleanly)
//	3. Run a multi-step pipeline that emits progress lines
//	4. Stream stdout/stderr back to the host with prefixes so you can tell them apart
//	5. Use typed errors to handle policy denials gracefully
//	6. Clean up
//
// Run kpd with the example policy first, then:
//
//	go run ./examples/streaming-pipeline
//
// Try with a non-allowlisted image to see ErrPolicyDenied handling:
//
//	IMAGE=ubuntu go run ./examples/streaming-pipeline
package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"time"

	"nexteam.id/kotakpasir/pkg/kotakpasir"
)

const pipelineScript = `
echo "[fetch]    starting"
sleep 1
echo "[fetch]    25%"
sleep 1
echo "[fetch]    50%"
sleep 1
echo "[fetch]    75%"
echo "[transform] this should appear on stderr" >&2
sleep 1
echo "[fetch]    complete"
echo "[report]   writing output"
echo "result: 42 records processed"
`

func main() {
	addr := envOr("KPD_ADDR", "http://127.0.0.1:8080")
	image := envOr("IMAGE", "alpine:latest")

	c := kotakpasir.New(addr, kotakpasir.WithToken(os.Getenv("KPD_TOKEN")))

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	if err := c.Health(ctx); err != nil {
		log.Fatalf("kpd not reachable at %s: %v", addr, err)
	}

	// 1. Create
	fmt.Printf(">>> 1/4 creating sandbox (image=%s)\n", image)
	sb, err := c.Create(ctx, kotakpasir.CreateOptions{Image: image})
	if err != nil {
		switch {
		case errors.Is(err, kotakpasir.ErrPolicyDenied):
			log.Fatalf("policy denied: %v\n  → add %q to images: in kotakpasir.yaml, or use the allowlisted default", err, image)
		case errors.Is(err, kotakpasir.ErrUnauthorized):
			log.Fatalf("unauthorized: %v\n  → set KPD_TOKEN to match the server", err)
		default:
			log.Fatalf("create: %v", err)
		}
	}
	defer func() {
		fmt.Printf(">>> cleanup: deleting %s\n", short(sb.ID))
		_ = c.Delete(context.Background(), sb.ID)
	}()

	// 2. WaitFor — robust against cold-start latency or pool drain
	fmt.Printf(">>> 2/4 waiting for sandbox %s to be running\n", short(sb.ID))
	sb, err = c.WaitFor(ctx, sb.ID, kotakpasir.StateRunning, 0)
	if err != nil {
		log.Fatalf("waitfor: %v", err)
	}
	fmt.Printf("    ready: state=%s\n", sb.State)

	// 3. ExecStream — long-running with line-by-line output
	fmt.Println(">>> 3/4 streaming pipeline")
	res, err := c.ExecStream(ctx, sb.ID, kotakpasir.ExecOptions{
		Cmd: []string{"sh", "-c", pipelineScript},
	}, taggedWriter("OUT"), taggedWriter("ERR"))
	if err != nil {
		log.Fatalf("exec stream: %v", err)
	}

	// 4. Report
	fmt.Printf(">>> 4/4 finished: exit=%d duration=%v\n", res.ExitCode, res.Duration)
}

// taggedWriter prefixes every chunk so you can see whether it came from
// stdout or stderr in the example output.
type taggedWriter string

func (t taggedWriter) Write(p []byte) (int, error) {
	fmt.Printf("    [%s] %s", string(t), string(p))
	return len(p), nil
}

func short(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
