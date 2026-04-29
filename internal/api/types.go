package api

type CreateSandboxRequest struct {
	Name     string            `json:"name,omitempty"`
	Profile  string            `json:"profile,omitempty"`
	Image    string            `json:"image,omitempty"`
	Cmd      []string          `json:"cmd,omitempty"`
	Env      map[string]string `json:"env,omitempty"`
	Cpus     float64           `json:"cpus,omitempty"`
	MemoryMB int64             `json:"memory_mb,omitempty"`
	TTLSec   int64             `json:"ttl_seconds,omitempty"`
}

type Sandbox struct {
	ID    string `json:"id"`
	Name  string `json:"name,omitempty"`
	Image string `json:"image"`
	State string `json:"state"`
}

type ExecRequest struct {
	Cmd     []string          `json:"cmd"`
	Env     map[string]string `json:"env,omitempty"`
	WorkDir string            `json:"workdir,omitempty"`
	Stdin   string            `json:"stdin,omitempty"`
}

type ExecResponse struct {
	ExitCode   int    `json:"exit_code"`
	Stdout     string `json:"stdout"`
	Stderr     string `json:"stderr"`
	DurationMs int64  `json:"duration_ms"`
}

type ErrorResponse struct {
	Error string `json:"error"`
}

type ProxyResponse struct {
	Address string `json:"address"`
}
