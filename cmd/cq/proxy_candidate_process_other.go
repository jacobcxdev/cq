//go:build !darwin && !linux && !windows

package main

import (
	"context"
	"github.com/jacobcxdev/cq/internal/proxy"
)

func captureCandidateProcess(context.Context, proxy.CandidateLifecycleStateV1, uint64) (candidateProcessLease, error) {
	return nil, proxy.ErrCandidateLifecycleInvalid
}
