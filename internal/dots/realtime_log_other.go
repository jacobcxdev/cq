//go:build !darwin && !linux

package dots

import (
	"errors"
	"os"
)

func openDotsPrivateLog(string) (*os.File, error) {
	return nil, errors.New("Dots logging unsupported on this platform")
}
