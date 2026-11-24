//go:build darwin || linux

package dots

import (
	"context"
	"errors"
	"os/exec"
	"strconv"
	"strings"
)

func dotsParentPID(ctx context.Context, pid int) (int, error) {
	data, err := exec.CommandContext(ctx, "/bin/ps", "-p", strconv.Itoa(pid), "-o", "ppid=").Output()
	if err != nil {
		return 0, err
	}
	parent, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || parent <= 0 {
		return 0, errors.New("process parent identity invalid")
	}
	return parent, nil
}
