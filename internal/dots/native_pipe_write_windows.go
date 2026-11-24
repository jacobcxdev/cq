//go:build windows

package dots

import (
	"context"
	"errors"
	"os"
	"time"
)

// Anonymous Windows pipes do not implement os.File deadlines. Closing the
// owned command writer on timeout permanently fails this CDP session.
func nativePipeWrite(ctx context.Context, file *os.File, data []byte) error {
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, nativeTimeout)
		defer cancel()
	}
	done := make(chan error, 1)
	go func() {
		defer func() {
			if recover() != nil {
				done <- errors.New("native debugger pipe write failed")
			}
		}()
		for len(data) > 0 {
			n, err := file.Write(data)
			if err != nil {
				done <- err
				return
			}
			data = data[n:]
		}
		done <- nil
	}()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		_ = file.Close()
		select {
		case <-done:
		case <-time.After(time.Second):
		}
		return ctx.Err()
	}
}
