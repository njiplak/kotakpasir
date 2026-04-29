package runtime

import (
	"context"
	"errors"
	"io"
	"time"
)

// ErrNoProxy means the sandbox has no associated egress proxy. Returned by
// runtimes that satisfy the optional ProxyAddr capability.
var ErrNoProxy = errors.New("sandbox has no egress proxy")

type Runtime interface {
	Create(ctx context.Context, spec Spec) (Handle, error)
	Exec(ctx context.Context, id string, exec ExecSpec) (ExecResult, error)
	Stop(ctx context.Context, id string) error
	Remove(ctx context.Context, id string) error
	Status(ctx context.Context, id string) (Status, error)
	Close() error
}

type Spec struct {
	Name        string
	Image       string
	Cmd         []string
	Env         map[string]string
	Cpus        float64
	MemoryMB    int64
	PidsLimit   int64
	User        string
	ReadOnly    bool
	NetworkMode string
	RuntimeName string
	Egress      Egress
	Labels      map[string]string
	TTL         time.Duration
}

type Egress struct {
	Mode  string
	Hosts []string
}

type Handle struct {
	ID   string
	Name string
}

type ExecSpec struct {
	Cmd     []string
	Env     map[string]string
	WorkDir string
	Stdin   io.Reader
	// Stdout / Stderr, if non-nil, receive output as it arrives. When nil,
	// the runtime buffers the streams and returns them in ExecResult.
	// Any combination is valid: stream one, buffer the other.
	Stdout io.Writer
	Stderr io.Writer
}

type ExecResult struct {
	ExitCode int
	Stdout   string
	Stderr   string
	Duration time.Duration
}

type Status struct {
	State    string
	ExitCode *int
}
