package stall

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"
)

func TestWatchStalled(t *testing.T) {
	t.Parallel()

	ctx, w := Watch(context.Background(), 10*time.Millisecond)
	defer w.Stop()

	select {
	case <-ctx.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("context is not canceled")
	}
	if err := Error(ctx, ctx.Err()); !errors.Is(err, ErrStalled) {
		t.Errorf("err = %v, want ErrStalled", err)
	}
}

func TestWatchProgress(t *testing.T) {
	t.Parallel()

	ctx, w := Watch(context.Background(), 100*time.Millisecond)
	defer w.Stop()

	// reading keeps postponing the timeout beyond its duration.
	r := w.Wrap(io.NopCloser(strings.NewReader("abcde")))
	buf := make([]byte, 1)
	for range 5 {
		time.Sleep(40 * time.Millisecond)
		if _, err := r.Read(buf); err != nil {
			t.Fatal(err)
		}
	}
	if err := ctx.Err(); err != nil {
		t.Errorf("context is canceled while making progress: %v", err)
	}
}

func TestErrorCanceled(t *testing.T) {
	t.Parallel()

	parent, cancel := context.WithCancel(context.Background())
	ctx, w := Watch(parent, time.Minute)
	defer w.Stop()
	cancel()

	// cancellation by others must not be reported as a stall.
	if err := Error(ctx, ctx.Err()); !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
}
