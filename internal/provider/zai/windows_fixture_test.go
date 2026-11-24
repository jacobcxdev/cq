package zai

import (
	"runtime"

	"github.com/jacobcxdev/cq/internal/fsutil"
)

func newZaiTestFS() *fsutil.MemFS {
	if runtime.GOOS == "windows" {
		return fsutil.NewMemFSWithHome(`C:\home\test`)
	}
	return fsutil.NewMemFS()
}
