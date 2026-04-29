package kotakpasir

import (
	"errors"
	"time"
)

// Sandbox is the public view of a sandbox returned by the kpd HTTP API.
type Sandbox struct {
	ID    string `json:"id"`
	Name  string `json:"name,omitempty"`
	Image string `json:"image"`
	State string `json:"state"`
}

// Sandbox states. Use the constants when comparing instead of bare strings.
const (
	StateCreated = "created"
	StateRunning = "running"
	StateStopped = "stopped"
	StateRemoved = "removed"
	StateError   = "error"
)

// CreateOptions is what callers send to kpd to create a sandbox. Either Image
// or Profile must be set; both are allowed (the request's Image overrides the
// profile's). Resource fields are honored only when they match what the
// (optional) warm pool was started with — otherwise kpd cold-starts.
type CreateOptions struct {
	Name     string            `json:"name,omitempty"`
	Profile  string            `json:"profile,omitempty"`
	Image    string            `json:"image,omitempty"`
	Cmd      []string          `json:"cmd,omitempty"`
	Env      map[string]string `json:"env,omitempty"`
	Cpus     float64           `json:"cpus,omitempty"`
	MemoryMB int64             `json:"memory_mb,omitempty"`
	TTL      time.Duration     `json:"-"`
}

// ExecOptions describes a single command to run inside an existing sandbox.
type ExecOptions struct {
	Cmd     []string
	Env     map[string]string
	WorkDir string
	Stdin   string
}

// ExecResult is returned by Client.Exec (the buffered call). For long-running
// or large-output commands prefer Client.ExecStream, which returns the same
// result fields minus Stdout/Stderr (those are written to the caller's writers
// as they arrive).
type ExecResult struct {
	ExitCode int
	Stdout   string
	Stderr   string
	Duration time.Duration
}

// Sentinel errors. Use errors.Is to check, not == — the SDK wraps these.
//
//	if errors.Is(err, kotakpasir.ErrNotFound) { ... }
var (
	ErrNotFound     = errors.New("kotakpasir: not found")
	ErrUnauthorized = errors.New("kotakpasir: unauthorized")
	ErrPolicyDenied = errors.New("kotakpasir: policy denied")
	ErrBadRequest   = errors.New("kotakpasir: bad request")
)

// Error wraps a non-2xx response from kpd. Kind, when non-nil, is one of the
// sentinel errors above so errors.Is works. Message is the server-provided
// description (or HTTP status text if the body was empty).
type Error struct {
	StatusCode int
	Message    string
	Kind       error
}

func (e *Error) Error() string {
	if e.Kind != nil {
		return e.Kind.Error() + " (" + e.Message + ")"
	}
	return e.Message
}

// Unwrap exposes Kind so errors.Is(err, ErrNotFound) etc. work.
func (e *Error) Unwrap() error { return e.Kind }
