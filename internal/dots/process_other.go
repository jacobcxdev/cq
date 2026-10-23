//go:build !darwin && !linux

package dots

import (
	"context"
	"errors"
	"os"
)

var errDotsProcessesUnsupported = errors.New("Dots process management is unsupported on this platform")

func dotsProcessIDs(context.Context) ([]int, error) { return nil, errDotsProcessesUnsupported }
func dotsProcessArguments(context.Context, int) (int, []string, error) {
	return 0, nil, errDotsProcessesUnsupported
}
func dotsProcessCWD(context.Context, int) (string, error) { return "", errDotsProcessesUnsupported }
func dotsTerminate(int) error                             { return errDotsProcessesUnsupported }
func SpawnHelper(*Settings, string, string, []string, []string) (*os.Process, error) {
	return nil, errDotsProcessesUnsupported
}
