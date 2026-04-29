package sandbox

import (
	"io"
	"time"
)

type State string

const (
	StateCreated State = "created"
	StateRunning State = "running"
	StateStopped State = "stopped"
	StateRemoved State = "removed"
	StateError   State = "error"
)

type Sandbox struct {
	ID        string            `json:"id"`
	Name      string            `json:"name,omitempty"`
	Image     string            `json:"image"`
	RuntimeID string            `json:"runtime_id,omitempty"`
	State     State             `json:"state"`
	Cpus      float64           `json:"cpus,omitempty"`
	MemoryMB  int64             `json:"memory_mb,omitempty"`
	Env       map[string]string `json:"env,omitempty"`
	Labels    map[string]string `json:"labels,omitempty"`
	CreatedAt time.Time         `json:"created_at"`
	ExpiresAt *time.Time        `json:"expires_at,omitempty"`
}

type CreateOptions struct {
	Name     string
	Profile  string
	Image    string
	Cmd      []string
	Env      map[string]string
	Cpus     float64
	MemoryMB int64
	TTL      time.Duration
	Labels   map[string]string
}

type ExecOptions struct {
	Cmd     []string
	Env     map[string]string
	WorkDir string
	Stdin   string
	// Stdout / Stderr, if non-nil, receive output as it arrives.
	// When both are nil, output is buffered and returned in ExecResult.
	Stdout io.Writer
	Stderr io.Writer
}

type ExecResult struct {
	ExitCode int
	Stdout   string
	Stderr   string
	Duration time.Duration
}
