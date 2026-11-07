package nativehost

import (
	"context"
	"errors"
	"os/exec"
	"time"
)

type AppWakeup struct {
	Client IPC
	Scheme string
	Open   func(context.Context, string) error
}

func (w AppWakeup) Ensure(ctx context.Context, hidden bool) error {
	if w.Client.Available(ctx) {
		return nil
	}
	uri := w.Scheme + ":"
	if hidden {
		uri += "?hidden=true"
	}
	open := w.Open
	if open == nil {
		open = func(ctx context.Context, uri string) error {
			return exec.CommandContext(ctx, "/usr/bin/open", uri).Run()
		}
	}
	if err := open(ctx, uri); err != nil {
		return err
	}
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return errors.New("app unavailable")
		case <-ticker.C:
			if w.Client.Available(ctx) {
				return nil
			}
		}
	}
}
