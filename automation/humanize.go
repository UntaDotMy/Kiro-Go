package automation

import (
	"context"
	"time"
)

// Timing helpers for the automation flows.

// DefaultLoginTimeout bounds one fully-automated login attempt.
const DefaultLoginTimeout = 90 * time.Second

// DefaultManualTimeout bounds an assisted (operator finishes by hand) login.
const DefaultManualTimeout = 15 * time.Minute

// sleepCtx sleeps for d unless ctx is cancelled first.
func sleepCtx(ctx context.Context, d time.Duration) {
	if d <= 0 {
		return
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}
