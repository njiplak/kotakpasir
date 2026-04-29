package kotakpasir

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// ExecStream runs a command in the sandbox and streams stdout/stderr to the
// provided writers as data arrives. Either writer may be nil to discard that
// stream. The returned ExecResult has zero-valued Stdout/Stderr (they were
// written to the writers).
//
// The call blocks until the command completes (or ctx is cancelled). For
// long-running commands, the underlying http.Client's per-request timeout
// is bypassed by using a fresh client without timeout for the SSE response.
func (c *Client) ExecStream(ctx context.Context, id string, opts ExecOptions, stdout, stderr io.Writer) (*ExecResult, error) {
	body := map[string]any{
		"cmd":     opts.Cmd,
		"env":     opts.Env,
		"workdir": opts.WorkDir,
		"stdin":   opts.Stdin,
	}
	buf, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.addr+"/v1/sandboxes/"+id+"/exec/stream", bytes.NewReader(buf))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}

	// Use a client without timeout for streaming. ctx cancellation still works.
	stream := &http.Client{Transport: c.hc.Transport}
	resp, err := stream.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		return nil, readErrorResponse(resp)
	}

	return parseSSE(resp.Body, stdout, stderr)
}

// parseSSE reads server-sent events from r and dispatches each event to the
// matching writer. It returns when the "exit" event arrives or the stream
// closes. Multi-line "data:" lines within one event are joined with "\n".
func parseSSE(r io.Reader, stdout, stderr io.Writer) (*ExecResult, error) {
	br := bufio.NewReader(r)

	var (
		event strings.Builder
		data  strings.Builder
		res   *ExecResult
		errFromStream error
	)

	flush := func() error {
		ev := event.String()
		dt := data.String()
		event.Reset()
		data.Reset()
		if ev == "" && dt == "" {
			return nil
		}
		switch ev {
		case "stdout":
			if stdout != nil {
				if _, err := io.WriteString(stdout, dt); err != nil {
					return err
				}
			}
		case "stderr":
			if stderr != nil {
				if _, err := io.WriteString(stderr, dt); err != nil {
					return err
				}
			}
		case "exit":
			var x struct {
				ExitCode   int   `json:"exit_code"`
				DurationMs int64 `json:"duration_ms"`
			}
			if err := json.Unmarshal([]byte(dt), &x); err != nil {
				return fmt.Errorf("parse exit event: %w", err)
			}
			res = &ExecResult{
				ExitCode: x.ExitCode,
				Duration: time.Duration(x.DurationMs) * time.Millisecond,
			}
		case "error":
			var x struct {
				Error string `json:"error"`
			}
			_ = json.Unmarshal([]byte(dt), &x)
			if x.Error == "" {
				x.Error = dt
			}
			errFromStream = fmt.Errorf("server stream error: %s", x.Error)
		}
		return nil
	}

	for {
		line, err := br.ReadString('\n')
		if len(line) > 0 {
			line = strings.TrimRight(line, "\r\n")
			switch {
			case line == "":
				if ferr := flush(); ferr != nil {
					return nil, ferr
				}
				if res != nil {
					return res, errFromStream
				}
				if errFromStream != nil {
					return nil, errFromStream
				}
			case strings.HasPrefix(line, "event: "):
				event.WriteString(strings.TrimPrefix(line, "event: "))
			case strings.HasPrefix(line, "data: "):
				if data.Len() > 0 {
					data.WriteByte('\n')
				}
				data.WriteString(strings.TrimPrefix(line, "data: "))
			case strings.HasPrefix(line, ":"):
				// SSE comment, ignore
			}
		}
		if err == io.EOF {
			if ferr := flush(); ferr != nil {
				return nil, ferr
			}
			if res != nil {
				return res, errFromStream
			}
			if errFromStream != nil {
				return nil, errFromStream
			}
			return nil, io.ErrUnexpectedEOF
		}
		if err != nil {
			return nil, err
		}
	}
}
