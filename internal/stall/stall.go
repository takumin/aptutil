// Package stall detects HTTP downloads that make no progress.
//
// Without it, a download from an upstream that keeps the connection
// open but stops sending data would hang forever.
package stall

import (
	"context"
	"errors"
	"io"
	"time"
)

// ErrStalled is returned when a download makes no progress for the
// timeout given to Watch.
var ErrStalled = errors.New("download stalled")

// Watcher cancels a request when it makes no progress for a while.
type Watcher struct {
	timer   *time.Timer
	timeout time.Duration
	cancel  context.CancelCauseFunc
}

// Watch returns a context for a request derived from ctx, and
// a Watcher that cancels it after timeout without progress.
//
// The whole download is not limited in time so that large files on
// slow links can still be downloaded.
func Watch(ctx context.Context, timeout time.Duration) (context.Context, *Watcher) {
	ctx, cancel := context.WithCancelCause(ctx)
	w := &Watcher{
		timeout: timeout,
		cancel:  cancel,
	}
	w.timer = time.AfterFunc(timeout, func() { cancel(ErrStalled) })
	return ctx, w
}

// Wrap returns a ReadCloser that postpones the timeout whenever data
// is read from rc.
func (w *Watcher) Wrap(rc io.ReadCloser) io.ReadCloser {
	return &reader{ReadCloser: rc, w: w}
}

// Stop releases the resources of w.  It must be called after the
// response body is closed so that the connection can be reused.
func (w *Watcher) Stop() {
	w.timer.Stop()
	w.cancel(nil)
}

type reader struct {
	io.ReadCloser
	w *Watcher
}

func (r *reader) Read(p []byte) (int, error) {
	n, err := r.ReadCloser.Read(p)
	if n > 0 {
		r.w.timer.Reset(r.w.timeout)
	}
	return n, err
}

// Error returns ErrStalled if ctx was canceled by a Watcher,
// or err otherwise.
func Error(ctx context.Context, err error) error {
	if errors.Is(context.Cause(ctx), ErrStalled) {
		return ErrStalled
	}
	return err
}
