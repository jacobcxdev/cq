//go:build !darwin && !linux

package dots

import (
	"errors"
	"net"
)

func nativePeerUID(*net.UnixConn) (uint32, error) {
	return 0, errors.New("native debugger peer verification unavailable")
}
