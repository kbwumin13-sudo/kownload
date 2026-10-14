package http

import "context"

// WaitContext lets a delegating protocol stop its waiter on pause without
// consuming the next run's completion or leaving a goroutine behind.
func (f *Fetcher) WaitContext(ctx context.Context) error {
	select {
	case err := <-f.doneCh:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}
