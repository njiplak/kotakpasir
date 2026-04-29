package mcp

import (
	"context"
	"errors"
	"fmt"
	"time"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"nexteam.id/kotakpasir/internal/sandbox"
)

const (
	serverName    = "kotakpasir"
	serverVersion = "0.1.0"
)

func NewServer(mgr *sandbox.Manager) *mcpsdk.Server {
	srv := mcpsdk.NewServer(&mcpsdk.Implementation{
		Name:    serverName,
		Version: serverVersion,
	}, nil)

	registerTools(srv, mgr)
	return srv
}

type CreateInput struct {
	Image    string            `json:"image,omitempty" jsonschema:"OCI image to run, e.g. alpine, python:3.12 (optional if profile is set)"`
	Profile  string            `json:"profile,omitempty" jsonschema:"named template from policy.profiles; image+limits+egress preset"`
	Name     string            `json:"name,omitempty" jsonschema:"optional human-readable name"`
	Cmd      []string          `json:"cmd,omitempty" jsonschema:"override default command"`
	Env      map[string]string `json:"env,omitempty" jsonschema:"environment variables"`
	Cpus     float64           `json:"cpus,omitempty" jsonschema:"CPU limit (1.0 = 1 core)"`
	MemoryMB int64             `json:"memory_mb,omitempty" jsonschema:"memory limit in MB"`
	TTLSec   int64             `json:"ttl_seconds,omitempty" jsonschema:"auto-delete after N seconds"`
}

type SandboxOutput struct {
	ID    string `json:"id"`
	Name  string `json:"name,omitempty"`
	Image string `json:"image"`
	State string `json:"state"`
}

type IDInput struct {
	ID string `json:"id" jsonschema:"sandbox id"`
}

type ExecInput struct {
	ID      string            `json:"id" jsonschema:"sandbox id"`
	Cmd     []string          `json:"cmd" jsonschema:"command and arguments"`
	Env     map[string]string `json:"env,omitempty"`
	WorkDir string            `json:"workdir,omitempty"`
	Stdin   string            `json:"stdin,omitempty"`
}

type ExecOutput struct {
	ExitCode   int    `json:"exit_code"`
	Stdout     string `json:"stdout"`
	Stderr     string `json:"stderr"`
	DurationMs int64  `json:"duration_ms"`
}

type ListOutput struct {
	Sandboxes []SandboxOutput `json:"sandboxes"`
}

type EmptyInput struct{}
type StatusOutput struct {
	OK bool `json:"ok"`
}

func registerTools(srv *mcpsdk.Server, mgr *sandbox.Manager) {
	mcpsdk.AddTool(srv, &mcpsdk.Tool{
		Name:        "sandbox_create",
		Description: "Create and start a new sandbox from an OCI image.",
	}, func(ctx context.Context, _ *mcpsdk.CallToolRequest, in CreateInput) (*mcpsdk.CallToolResult, SandboxOutput, error) {
		sb, err := mgr.Create(ctx, sandbox.CreateOptions{
			Name:     in.Name,
			Profile:  in.Profile,
			Image:    in.Image,
			Cmd:      in.Cmd,
			Env:      in.Env,
			Cpus:     in.Cpus,
			MemoryMB: in.MemoryMB,
			TTL:      time.Duration(in.TTLSec) * time.Second,
		})
		if err != nil {
			return nil, SandboxOutput{}, err
		}
		return textResult(fmt.Sprintf("created sandbox %s", sb.ID)), toMCP(sb), nil
	})

	mcpsdk.AddTool(srv, &mcpsdk.Tool{
		Name:        "sandbox_list",
		Description: "List all known sandboxes.",
	}, func(ctx context.Context, _ *mcpsdk.CallToolRequest, _ EmptyInput) (*mcpsdk.CallToolResult, ListOutput, error) {
		items, err := mgr.List(ctx)
		if err != nil {
			return nil, ListOutput{}, err
		}
		out := ListOutput{Sandboxes: make([]SandboxOutput, 0, len(items))}
		for _, sb := range items {
			out.Sandboxes = append(out.Sandboxes, toMCP(sb))
		}
		return textResult(fmt.Sprintf("%d sandbox(es)", len(out.Sandboxes))), out, nil
	})

	mcpsdk.AddTool(srv, &mcpsdk.Tool{
		Name:        "sandbox_get",
		Description: "Get the current state of a sandbox.",
	}, func(ctx context.Context, _ *mcpsdk.CallToolRequest, in IDInput) (*mcpsdk.CallToolResult, SandboxOutput, error) {
		sb, err := mgr.Get(ctx, in.ID)
		if errors.Is(err, sandbox.ErrNotFound) {
			return nil, SandboxOutput{}, fmt.Errorf("sandbox %s not found", in.ID)
		}
		if err != nil {
			return nil, SandboxOutput{}, err
		}
		return nil, toMCP(sb), nil
	})

	mcpsdk.AddTool(srv, &mcpsdk.Tool{
		Name:        "sandbox_exec",
		Description: "Run a command inside a running sandbox and return its output.",
	}, func(ctx context.Context, _ *mcpsdk.CallToolRequest, in ExecInput) (*mcpsdk.CallToolResult, ExecOutput, error) {
		res, err := mgr.Exec(ctx, in.ID, sandbox.ExecOptions{
			Cmd:     in.Cmd,
			Env:     in.Env,
			WorkDir: in.WorkDir,
			Stdin:   in.Stdin,
		})
		if err != nil {
			return nil, ExecOutput{}, err
		}
		return textResult(res.Stdout), ExecOutput{
			ExitCode:   res.ExitCode,
			Stdout:     res.Stdout,
			Stderr:     res.Stderr,
			DurationMs: res.Duration.Milliseconds(),
		}, nil
	})

	mcpsdk.AddTool(srv, &mcpsdk.Tool{
		Name:        "sandbox_stop",
		Description: "Stop a running sandbox without deleting it.",
	}, func(ctx context.Context, _ *mcpsdk.CallToolRequest, in IDInput) (*mcpsdk.CallToolResult, StatusOutput, error) {
		if err := mgr.Stop(ctx, in.ID); err != nil {
			return nil, StatusOutput{}, err
		}
		return textResult("stopped " + in.ID), StatusOutput{OK: true}, nil
	})

	mcpsdk.AddTool(srv, &mcpsdk.Tool{
		Name:        "sandbox_delete",
		Description: "Stop and permanently delete a sandbox.",
	}, func(ctx context.Context, _ *mcpsdk.CallToolRequest, in IDInput) (*mcpsdk.CallToolResult, StatusOutput, error) {
		if err := mgr.Delete(ctx, in.ID); err != nil {
			return nil, StatusOutput{}, err
		}
		return textResult("deleted " + in.ID), StatusOutput{OK: true}, nil
	})
}

func toMCP(sb sandbox.Sandbox) SandboxOutput {
	return SandboxOutput{
		ID:    sb.ID,
		Name:  sb.Name,
		Image: sb.Image,
		State: string(sb.State),
	}
}

func textResult(msg string) *mcpsdk.CallToolResult {
	return &mcpsdk.CallToolResult{
		Content: []mcpsdk.Content{&mcpsdk.TextContent{Text: msg}},
	}
}
