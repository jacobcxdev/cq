//go:build darwin || linux || freebsd || openbsd || netbsd || dragonfly

package installer

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"

	"golang.org/x/sys/unix"
)

// DefaultGoDestinationResolver resolves the current user's Go binary path.
func DefaultGoDestinationResolver() GoDestinationResolver {
	return GoDestinationResolver{
		GOOS:        runtime.GOOS,
		Getenv:      os.Getenv,
		GoEnvGOPATH: queryGoEnvGOPATH,
		Stat:        os.Stat,
		Writable:    writableGoDestination,
	}
}

func queryGoEnvGOPATH(ctx context.Context) (string, error) {
	output, err := exec.CommandContext(ctx, "go", "env", "GOPATH").Output()
	if err != nil {
		return "", err
	}
	if len(output) > 64<<10 {
		return "", fmt.Errorf("go env GOPATH output exceeds size limit")
	}
	return strings.TrimSpace(string(output)), nil
}

func writableGoDestination(directory string) error {
	info, err := os.Stat(directory)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("not a directory")
	}
	return unix.Access(directory, unix.W_OK)
}
