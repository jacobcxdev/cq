package proxy

import (
	"errors"
	"path/filepath"
	"sort"
	"time"

	"github.com/jacobcxdev/cq/internal/fsutil"
)

// NewCodexContinuityOpenOptions freezes the routing epochs and authority paths
// used by one continuity coordinator for the lifetime of a proxy process.
func NewCodexContinuityOpenOptions(
	fsys fsutil.DurableFileSystem,
	stateDir string,
	runtime *CodexRoutingRuntime,
	retention time.Duration,
	now func() time.Time,
) (CodexContinuityOpenOptions, error) {
	if fsys == nil || stateDir == "" || runtime == nil || retention <= 0 || now == nil {
		return CodexContinuityOpenOptions{}, errors.New("incomplete Codex continuity configuration")
	}

	epochSet := make(map[uint64]struct{})
	addStatus := func(status CodexModeStatus) {
		if status.AuthoritativeEpoch != 0 {
			epochSet[status.AuthoritativeEpoch] = struct{}{}
		}
		for _, epoch := range status.RetainedAuthoritativeEpochs {
			if epoch != 0 {
				epochSet[epoch] = struct{}{}
			}
		}
	}
	addStatus(runtime.HTTP)
	addStatus(runtime.WebSocket)
	epochs := make([]uint64, 0, len(epochSet))
	for epoch := range epochSet {
		epochs = append(epochs, epoch)
	}
	sort.Slice(epochs, func(i, j int) bool { return epochs[i] < epochs[j] })

	return CodexContinuityOpenOptions{
		FS:          fsys,
		KeyPath:     filepath.Join(stateDir, "codex-turn-leases.key"),
		JournalPath: filepath.Join(stateDir, "codex-turn-leases.json"),
		Policy: CodexLeasePolicy{
			Retention: retention,
			Now:       now,
		},
		Modes: CodexModeAuthoritySnapshot{RecognisedAuthoritativeEpochs: epochs},
	}, nil
}
