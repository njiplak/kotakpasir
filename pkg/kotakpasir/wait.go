package kotakpasir

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// DefaultWaitInterval is used by WaitFor when interval == 0.
const DefaultWaitInterval = 200 * time.Millisecond

// WaitFor polls Get(id) until the sandbox reaches the target state,
// the context is cancelled, or it lands in a terminal state different
// from the target. interval=0 uses DefaultWaitInterval.
//
// Terminal states for the purposes of WaitFor are StateRemoved and StateError;
// reaching those when waiting for something else returns immediately with an error.
//
// A common use is to call WaitFor(ctx, id, StateRunning, 0) right after Create
// when the request resolved to a non-default spec (which forces a cold start).
func (c *Client) WaitFor(ctx context.Context, id, target string, interval time.Duration) (*Sandbox, error) {
	if interval <= 0 {
		interval = DefaultWaitInterval
	}

	t := time.NewTicker(interval)
	defer t.Stop()

	for {
		sb, err := c.Get(ctx, id)
		if err != nil {
			// On NotFound there's nothing to wait for — surface it.
			if errors.Is(err, ErrNotFound) {
				return nil, fmt.Errorf("WaitFor %q: %w", target, err)
			}
			return nil, err
		}
		if sb.State == target {
			return sb, nil
		}
		if isTerminalState(sb.State) && sb.State != target {
			return sb, fmt.Errorf("sandbox %s reached terminal state %q while waiting for %q", id, sb.State, target)
		}

		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-t.C:
		}
	}
}

func isTerminalState(s string) bool {
	return s == StateRemoved || s == StateError
}
