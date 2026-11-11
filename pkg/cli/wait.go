package cli

import (
	"context"
	"errors"
	"fmt"
	"time"

	"connectrpc.com/connect/v2"
)

// pollInterval is how often --wait reads the resource. Each read also
// moves the server's work on (e.g. the next tier of a delete).
const pollInterval = 15 * time.Second

// DefaultWaitTimeout bounds --wait: an EKS cluster takes ~15 minutes, a
// delete with draining node groups longer.
const DefaultWaitTimeout = 45 * time.Minute

// observation is one read of a resource while waiting.
type observation struct {
	state, message string
	// done: the wait is over (ready, or gone for a delete); failed: it is
	// over and failed.
	done, failed bool
}

// waitUntil polls read until it reports done, printing each state change
// to o.Progress. read returning NOT_FOUND counts as done when gone is
// true (a delete), and as an error otherwise.
func waitUntil(ctx context.Context, o Options, what string, gone bool, read func(context.Context) (observation, error)) error {
	timeout := o.Timeout
	if timeout <= 0 {
		timeout = DefaultWaitTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	last := ""
	for {
		obs, err := read(ctx)
		switch {
		case connect.CodeOf(err) == connect.CodeNotFound && gone:
			o.progress("%s: deleted", what)
			return nil
		case err != nil:
			return fmt.Errorf("waiting for %s: %w", what, err)
		}
		if line := obs.state + ": " + obs.message; line != last {
			o.progress("%s: %s", what, line)
			last = line
		}
		if obs.failed {
			return fmt.Errorf("%s is %s: %s", what, obs.state, obs.message)
		}
		if obs.done {
			return nil
		}
		select {
		case <-ctx.Done():
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				return fmt.Errorf("%s: still %s after %s; check again with get", what, obs.state, timeout)
			}
			return ctx.Err()
		case <-time.After(pollInterval):
		}
	}
}

func (o Options) progress(format string, args ...any) {
	if o.Progress != nil {
		// Progress lines are best effort: a closed stderr must not fail the wait.
		_, _ = fmt.Fprintf(o.Progress, "%s %s\n", time.Now().Format("15:04:05"), fmt.Sprintf(format, args...))
	}
}
