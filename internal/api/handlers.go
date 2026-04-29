package api

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gofiber/fiber/v3"

	"nexteam.id/kotakpasir/internal/policy"
	rt "nexteam.id/kotakpasir/internal/runtime"
	"nexteam.id/kotakpasir/internal/sandbox"
	"nexteam.id/kotakpasir/internal/sandbox/logbuf"
)

// handleHealth returns the manager's aggregate readiness. Status 200 when
// every check passes; 503 when any subsystem (store, runtime, pool) is
// degraded — load balancers can use the status code, humans can use the
// per-check detail in the body.
func (s *Server) handleHealth(c fiber.Ctx) error {
	report := s.mgr.Health(c.Context())
	if report.Status != "ok" {
		return c.Status(fiber.StatusServiceUnavailable).JSON(report)
	}
	return c.JSON(report)
}

func (s *Server) handleCreate(c fiber.Ctx) error {
	var req CreateSandboxRequest
	if err := c.Bind().Body(&req); err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(ErrorResponse{Error: err.Error()})
	}
	if req.Image == "" && req.Profile == "" {
		return c.Status(fiber.StatusBadRequest).JSON(ErrorResponse{Error: "image or profile is required"})
	}

	sb, err := s.mgr.Create(c.Context(), sandbox.CreateOptions{
		Name:     req.Name,
		Profile:  req.Profile,
		Image:    req.Image,
		Cmd:      req.Cmd,
		Env:      req.Env,
		Cpus:     req.Cpus,
		MemoryMB: req.MemoryMB,
		TTL:      time.Duration(req.TTLSec) * time.Second,
	})
	if err != nil {
		// Policy violations are user input errors → 400. Anything else (runtime,
		// daemon, store) is a server fault → 500.
		if errors.Is(err, policy.ErrPolicyViolation) {
			return c.Status(fiber.StatusBadRequest).JSON(ErrorResponse{Error: err.Error()})
		}
		return c.Status(fiber.StatusInternalServerError).JSON(ErrorResponse{Error: err.Error()})
	}
	return c.Status(fiber.StatusCreated).JSON(toAPI(sb))
}

func (s *Server) handleList(c fiber.Ctx) error {
	items, err := s.mgr.List(c.Context())
	if err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(ErrorResponse{Error: err.Error()})
	}
	out := make([]Sandbox, 0, len(items))
	for _, sb := range items {
		out = append(out, toAPI(sb))
	}
	return c.JSON(out)
}

func (s *Server) handleGet(c fiber.Ctx) error {
	id := c.Params("id")
	sb, err := s.mgr.Get(c.Context(), id)
	if errors.Is(err, sandbox.ErrNotFound) {
		return c.Status(fiber.StatusNotFound).JSON(ErrorResponse{Error: "not found"})
	}
	if err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(ErrorResponse{Error: err.Error()})
	}
	return c.JSON(toAPI(sb))
}

func (s *Server) handleDelete(c fiber.Ctx) error {
	id := c.Params("id")
	if err := s.mgr.Delete(c.Context(), id); err != nil {
		if errors.Is(err, sandbox.ErrNotFound) {
			return c.Status(fiber.StatusNotFound).JSON(ErrorResponse{Error: "not found"})
		}
		return c.Status(fiber.StatusInternalServerError).JSON(ErrorResponse{Error: err.Error()})
	}
	return c.SendStatus(fiber.StatusNoContent)
}

// handleLogs serves captured exec output for a sandbox.
//
//   - GET /v1/sandboxes/:id/logs                 → text/plain merged dump
//   - GET /v1/sandboxes/:id/logs?tail=N          → trim to last N lines
//   - GET /v1/sandboxes/:id/logs?follow=true     → SSE; snapshot first, then live
func (s *Server) handleLogs(c fiber.Ctx) error {
	id := c.Params("id")

	follow := strings.EqualFold(c.Query("follow"), "true") || c.Query("follow") == "1"
	tail := 0
	if v := c.Query("tail"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			return c.Status(fiber.StatusBadRequest).JSON(ErrorResponse{Error: "tail must be a non-negative integer"})
		}
		tail = n
	}

	res, err := s.mgr.Logs(c.Context(), id, sandbox.LogsOptions{TailLines: tail, Follow: follow})
	if errors.Is(err, sandbox.ErrNotFound) {
		return c.Status(fiber.StatusNotFound).JSON(ErrorResponse{Error: "not found"})
	}
	if err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(ErrorResponse{Error: err.Error()})
	}

	if !follow {
		defer res.Cancel()
		c.Set(fiber.HeaderContentType, "text/plain; charset=utf-8")
		var buf strings.Builder
		for _, e := range res.Snapshot {
			buf.Write(e.Data)
		}
		return c.SendString(buf.String())
	}

	c.Set(fiber.HeaderContentType, "text/event-stream")
	c.Set(fiber.HeaderCacheControl, "no-cache")
	c.Set(fiber.HeaderConnection, "keep-alive")
	c.Set("X-Accel-Buffering", "no")

	ctx := c.Context()
	return c.SendStreamWriter(func(w *bufio.Writer) {
		defer res.Cancel()
		sse := newSSEWriter(w)
		emit := func(e logbuf.Entry) {
			ev := "stdout"
			if e.Stream == logbuf.Stderr {
				ev = "stderr"
			}
			sse.send(ev, string(e.Data))
		}
		for _, e := range res.Snapshot {
			emit(e)
		}
		for {
			select {
			case <-ctx.Done():
				return
			case e, ok := <-res.Subscribe:
				if !ok {
					return
				}
				emit(e)
			}
		}
	})
}

func (s *Server) handleProxy(c fiber.Ctx) error {
	id := c.Params("id")
	addr, err := s.mgr.ProxyAddr(c.Context(), id)
	if errors.Is(err, sandbox.ErrNotFound) {
		return c.Status(fiber.StatusNotFound).JSON(ErrorResponse{Error: "not found"})
	}
	if errors.Is(err, rt.ErrNoProxy) {
		return c.Status(fiber.StatusNotFound).JSON(ErrorResponse{Error: "sandbox has no egress proxy"})
	}
	if err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(ErrorResponse{Error: err.Error()})
	}
	return c.JSON(ProxyResponse{Address: addr})
}

func (s *Server) handleStop(c fiber.Ctx) error {
	id := c.Params("id")
	if err := s.mgr.Stop(c.Context(), id); err != nil {
		if errors.Is(err, sandbox.ErrNotFound) {
			return c.Status(fiber.StatusNotFound).JSON(ErrorResponse{Error: "not found"})
		}
		return c.Status(fiber.StatusInternalServerError).JSON(ErrorResponse{Error: err.Error()})
	}
	return c.SendStatus(fiber.StatusNoContent)
}

func (s *Server) handleExec(c fiber.Ctx) error {
	id := c.Params("id")
	var req ExecRequest
	if err := c.Bind().Body(&req); err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(ErrorResponse{Error: err.Error()})
	}
	if len(req.Cmd) == 0 {
		return c.Status(fiber.StatusBadRequest).JSON(ErrorResponse{Error: "cmd is required"})
	}
	res, err := s.mgr.Exec(c.Context(), id, sandbox.ExecOptions{
		Cmd:     req.Cmd,
		Env:     req.Env,
		WorkDir: req.WorkDir,
		Stdin:   req.Stdin,
	})
	if errors.Is(err, sandbox.ErrNotFound) {
		return c.Status(fiber.StatusNotFound).JSON(ErrorResponse{Error: "not found"})
	}
	if err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(ErrorResponse{Error: err.Error()})
	}
	return c.JSON(ExecResponse{
		ExitCode:   res.ExitCode,
		Stdout:     res.Stdout,
		Stderr:     res.Stderr,
		DurationMs: res.Duration.Milliseconds(),
	})
}

func (s *Server) handleExecStream(c fiber.Ctx) error {
	id := c.Params("id")
	var req ExecRequest
	if err := c.Bind().Body(&req); err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(ErrorResponse{Error: err.Error()})
	}
	if len(req.Cmd) == 0 {
		return c.Status(fiber.StatusBadRequest).JSON(ErrorResponse{Error: "cmd is required"})
	}

	c.Set(fiber.HeaderContentType, "text/event-stream")
	c.Set(fiber.HeaderCacheControl, "no-cache")
	c.Set(fiber.HeaderConnection, "keep-alive")
	c.Set("X-Accel-Buffering", "no")

	// Capture parent context up front; the stream writer doesn't have c.Context() reliably.
	ctx := c.Context()

	return c.SendStreamWriter(func(w *bufio.Writer) {
		sse := newSSEWriter(w)
		stdoutW := sse.tagged("stdout")
		stderrW := sse.tagged("stderr")

		res, err := s.mgr.Exec(ctx, id, sandbox.ExecOptions{
			Cmd:     req.Cmd,
			Env:     req.Env,
			WorkDir: req.WorkDir,
			Stdin:   req.Stdin,
			Stdout:  stdoutW,
			Stderr:  stderrW,
		})
		if err != nil {
			if errors.Is(err, sandbox.ErrNotFound) {
				sse.send("error", `{"error":"not found"}`)
				return
			}
			sse.send("error", fmt.Sprintf(`{"error":%q}`, err.Error()))
			return
		}
		exitJSON, _ := json.Marshal(map[string]any{
			"exit_code":   res.ExitCode,
			"duration_ms": res.Duration.Milliseconds(),
		})
		sse.send("exit", string(exitJSON))
	})
}

// sseWriter is a tiny SSE encoder. Concurrent writes to the underlying
// bufio.Writer are serialized; multi-line data is split into multiple
// "data:" lines per the SSE spec.
type sseWriter struct {
	mu sync.Mutex
	w  *bufio.Writer
}

func newSSEWriter(w *bufio.Writer) *sseWriter {
	return &sseWriter{w: w}
}

func (s *sseWriter) send(event, data string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if event != "" {
		fmt.Fprintf(s.w, "event: %s\n", event)
	}
	for line := range strings.SplitSeq(data, "\n") {
		fmt.Fprintf(s.w, "data: %s\n", line)
	}
	fmt.Fprint(s.w, "\n")
	_ = s.w.Flush()
}

// tagged returns an io.Writer that emits each chunk as an SSE event with
// the given event name. Multi-line writes are split into multiple events
// (one per non-empty line) so consumers see incremental output.
func (s *sseWriter) tagged(event string) *sseTaggedWriter {
	return &sseTaggedWriter{sse: s, event: event}
}

type sseTaggedWriter struct {
	sse   *sseWriter
	event string
}

func (w *sseTaggedWriter) Write(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	w.sse.send(w.event, string(p))
	return len(p), nil
}

func toAPI(sb sandbox.Sandbox) Sandbox {
	return Sandbox{
		ID:    sb.ID,
		Name:  sb.Name,
		Image: sb.Image,
		State: string(sb.State),
	}
}
