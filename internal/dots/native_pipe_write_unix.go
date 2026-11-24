//go:build !windows

package dots

import (
	"context"
	"os"
	"time"
)

func nativePipeWrite(ctx context.Context, file *os.File, data []byte) error {
	deadline, ok := ctx.Deadline()
	if !ok {
		deadline = time.Now().Add(nativeTimeout)
	}
	if err := file.SetWriteDeadline(deadline); err != nil {
		return err
	}
	for len(data) > 0 {
		n, err := file.Write(data)
		if err != nil {
			return err
		}
		data = data[n:]
	}
	return nil
}
