package main

import (
	"context"
	"net/http"
	"time"

	"github.com/jacobcxdev/cq/internal/proxy"
)

func codexResetExecutorStatus(ctx context.Context) proxy.CodexResetSchedulerStatus {
	deps := defaultProxyLeaseDependencies()
	deps.LoadConfig = proxy.LoadExistingConfig
	return codexResetExecutorStatusWithDependencies(ctx, deps, time.Now())
}
func codexResetExecutorStatusWithDependencies(ctx context.Context, deps proxyLeaseDependencies, now time.Time) proxy.CodexResetSchedulerStatus {
	result := proxy.CodexResetSchedulerStatus{State: "unavailable"}
	probe, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	if err := proxyLeaseRequest(probe, 0, proxy.RuntimeCodexResetScheduleStatusPath, http.NoBody, &result, deps); err != nil {
		return proxy.CodexResetSchedulerStatus{State: "unavailable"}
	}
	if result.Running && (result.CheckedAt.IsZero() || result.CheckedAt.After(now.Add(2*time.Second)) || now.Sub(result.CheckedAt) > proxy.CodexResetSchedulerHeartbeatMaxAge) {
		result.Running = false
		result.State = "stale"
	}
	return result
}
