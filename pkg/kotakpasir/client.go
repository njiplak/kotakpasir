// Package kotakpasir is a thin Go client for the kpd HTTP API.
//
// Typical use:
//
//	c := kotakpasir.New("http://127.0.0.1:8080", kotakpasir.WithToken(os.Getenv("KPD_TOKEN")))
//	sb, _ := c.Create(ctx, kotakpasir.CreateOptions{Image: "alpine:latest"})
//	defer c.Delete(ctx, sb.ID)
//
//	res, _ := c.Exec(ctx, sb.ID, kotakpasir.ExecOptions{Cmd: []string{"echo", "hi"}})
//	fmt.Println(res.Stdout)
package kotakpasir

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

const defaultTimeout = 30 * time.Second

type Client struct {
	addr  string
	token string
	hc    *http.Client
}

type Option func(*Client)

// WithToken authenticates every request via Authorization: Bearer <token>.
// kpd ignores it when KPD_TOKEN is unset on the server side.
func WithToken(t string) Option {
	return func(c *Client) { c.token = t }
}

// WithHTTPClient overrides the underlying http.Client. Streaming calls
// disable per-call timeouts internally, so a long-lived client is safe.
func WithHTTPClient(h *http.Client) Option {
	return func(c *Client) { c.hc = h }
}

// New constructs a client. addr is the kpd base URL, e.g. "http://127.0.0.1:8080".
func New(addr string, opts ...Option) *Client {
	c := &Client{
		addr: strings.TrimRight(addr, "/"),
		hc:   &http.Client{Timeout: defaultTimeout},
	}
	for _, opt := range opts {
		opt(c)
	}
	return c
}

func (c *Client) Health(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.addr+"/healthz", nil)
	if err != nil {
		return err
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("healthz: status %d", resp.StatusCode)
	}
	return nil
}

func (c *Client) Create(ctx context.Context, opts CreateOptions) (*Sandbox, error) {
	body := map[string]any{
		"name":     opts.Name,
		"profile":  opts.Profile,
		"image":    opts.Image,
		"cmd":      opts.Cmd,
		"env":      opts.Env,
		"cpus":     opts.Cpus,
		"memory_mb": opts.MemoryMB,
	}
	if opts.TTL > 0 {
		body["ttl_seconds"] = int64(opts.TTL.Seconds())
	}
	var sb Sandbox
	if err := c.do(ctx, http.MethodPost, "/v1/sandboxes", body, &sb); err != nil {
		return nil, err
	}
	return &sb, nil
}

func (c *Client) Get(ctx context.Context, id string) (*Sandbox, error) {
	var sb Sandbox
	if err := c.do(ctx, http.MethodGet, "/v1/sandboxes/"+id, nil, &sb); err != nil {
		return nil, err
	}
	return &sb, nil
}

func (c *Client) List(ctx context.Context) ([]Sandbox, error) {
	var out []Sandbox
	if err := c.do(ctx, http.MethodGet, "/v1/sandboxes", nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

func (c *Client) Stop(ctx context.Context, id string) error {
	return c.do(ctx, http.MethodPost, "/v1/sandboxes/"+id+"/stop", nil, nil)
}

func (c *Client) Delete(ctx context.Context, id string) error {
	return c.do(ctx, http.MethodDelete, "/v1/sandboxes/"+id, nil, nil)
}

// Exec runs a command and returns the buffered result. Use ExecStream for
// long-running or large-output commands.
func (c *Client) Exec(ctx context.Context, id string, opts ExecOptions) (*ExecResult, error) {
	body := map[string]any{
		"cmd":     opts.Cmd,
		"env":     opts.Env,
		"workdir": opts.WorkDir,
		"stdin":   opts.Stdin,
	}
	var raw struct {
		ExitCode   int    `json:"exit_code"`
		Stdout     string `json:"stdout"`
		Stderr     string `json:"stderr"`
		DurationMs int64  `json:"duration_ms"`
	}
	if err := c.do(ctx, http.MethodPost, "/v1/sandboxes/"+id+"/exec", body, &raw); err != nil {
		return nil, err
	}
	return &ExecResult{
		ExitCode: raw.ExitCode,
		Stdout:   raw.Stdout,
		Stderr:   raw.Stderr,
		Duration: time.Duration(raw.DurationMs) * time.Millisecond,
	}, nil
}

func (c *Client) do(ctx context.Context, method, path string, body, out any) error {
	var bodyReader io.Reader
	if body != nil {
		buf, err := json.Marshal(body)
		if err != nil {
			return err
		}
		bodyReader = bytes.NewReader(buf)
	}

	req, err := http.NewRequestWithContext(ctx, method, c.addr+path, bodyReader)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}

	resp, err := c.hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		return readErrorResponse(resp)
	}
	if out == nil || resp.StatusCode == http.StatusNoContent {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil && !errors.Is(err, io.EOF) {
		return fmt.Errorf("decode response: %w", err)
	}
	return nil
}

func readErrorResponse(resp *http.Response) error {
	var apiErr struct {
		Error string `json:"error"`
	}
	body, _ := io.ReadAll(resp.Body)
	_ = json.Unmarshal(body, &apiErr)
	msg := apiErr.Error
	if msg == "" {
		msg = strings.TrimSpace(string(body))
	}
	if msg == "" {
		msg = http.StatusText(resp.StatusCode)
	}
	return &Error{
		StatusCode: resp.StatusCode,
		Message:    msg,
		Kind:       classifyError(resp.StatusCode, msg),
	}
}

// classifyError maps an HTTP status (and message hint for 400s) onto a
// sentinel error so callers can errors.Is(err, ErrXxx) cleanly.
func classifyError(status int, msg string) error {
	switch status {
	case http.StatusNotFound:
		return ErrNotFound
	case http.StatusUnauthorized, http.StatusForbidden:
		return ErrUnauthorized
	case http.StatusBadRequest:
		lower := strings.ToLower(msg)
		if strings.Contains(lower, "allowlist") ||
			strings.Contains(lower, "policy") ||
			strings.Contains(lower, "profile ") {
			return ErrPolicyDenied
		}
		return ErrBadRequest
	}
	return nil
}
