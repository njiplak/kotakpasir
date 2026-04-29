package kotakpasir

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
)

// LogsOptions controls Client.Logs / Client.LogsStream.
type LogsOptions struct {
	// TailLines, if > 0, returns only the last N lines.
	TailLines int
}

// Logs fetches the buffered exec output for a sandbox as a single string.
// Empty when capture is disabled or nothing has been exec'd.
func (c *Client) Logs(ctx context.Context, id string, opts LogsOptions) (string, error) {
	q := url.Values{}
	if opts.TailLines > 0 {
		q.Set("tail", strconv.Itoa(opts.TailLines))
	}
	path := "/v1/sandboxes/" + id + "/logs"
	if encoded := q.Encode(); encoded != "" {
		path += "?" + encoded
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.addr+path, nil)
	if err != nil {
		return "", err
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}

	resp, err := c.hc.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		return "", readErrorResponse(resp)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("read logs: %w", err)
	}
	return string(body), nil
}

// LogsStream streams captured output: the snapshot first, then any new
// entries until ctx cancels or the server closes. stdout/stderr writers
// receive their respective tagged streams; either may be nil to discard.
func (c *Client) LogsStream(ctx context.Context, id string, opts LogsOptions, stdout, stderr io.Writer) error {
	q := url.Values{"follow": []string{"true"}}
	if opts.TailLines > 0 {
		q.Set("tail", strconv.Itoa(opts.TailLines))
	}
	path := "/v1/sandboxes/" + id + "/logs?" + q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.addr+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "text/event-stream")
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}

	stream := &http.Client{Transport: c.hc.Transport}
	resp, err := stream.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		return readErrorResponse(resp)
	}

	// parseSSE blocks until "exit" or stream close. Logs streams never emit
	// "exit"; they end on EOF (server cancelled / client cancelled).
	_, err = parseSSE(resp.Body, stdout, stderr)
	if err == io.ErrUnexpectedEOF {
		return nil
	}
	return err
}
