package cli

import (
	"context"
	"fmt"
	"time"
)

// defaultPollInterval is how often a --wait looks again.
const defaultPollInterval = 5 * time.Second

func (o Options) pollInterval() time.Duration {
	if o.PollInterval <= 0 {
		return defaultPollInterval
	}
	return o.PollInterval
}

// poll calls done every poll interval until it reports true or fails,
// for at most --timeout.
func poll(ctx context.Context, o Options, what string, done func() (bool, error)) error {
	if o.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, o.Timeout)
		defer cancel()
	}
	for {
		ok, err := done()
		if err != nil || ok {
			return err
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("gave up waiting for %s after %s (--timeout)", what, o.Timeout)
		case <-time.After(o.pollInterval()):
		}
	}
}

// progressf writes to Progress (stderr), when there is one.
func (o Options) progressf(format string, args ...any) {
	if o.Progress != nil {
		_, _ = fmt.Fprintf(o.Progress, format, args...) // progress on stderr: nothing to do if it cannot be written
	}
}
