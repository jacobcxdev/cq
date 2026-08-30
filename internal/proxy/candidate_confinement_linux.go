//go:build linux

package proxy

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"time"
)

var linuxCandidateConfinementProbe = sync.OnceValue(probeLinuxCandidateConfinement)

func probeLinuxCandidateConfinement() bool {
	abi, err := linuxLandlockABI()
	if err != nil || abi < 3 {
		return false
	}
	root, err := os.MkdirTemp("", "cq-linux-confinement-probe-")
	if err != nil {
		return false
	}
	defer os.RemoveAll(root)
	truePath := "/bin/true"
	if resolved, resolveErr := filepath.EvalSymlinks(truePath); resolveErr == nil {
		truePath = resolved
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err = (linuxCodexAcceptanceConfinement{}).Execute(ctx, codexAcceptanceExecution{
		executable: truePath,
		command: codexAcceptanceCommand{
			dir:              root,
			sandboxWriteRoot: root,
			loopbackOnly:     true,
		},
	})
	if err != nil {
		return false
	}
	return true
}

func candidatePlatformConfinementAvailable() bool {
	return linuxCandidateConfinementProbe()
}
