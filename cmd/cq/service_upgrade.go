package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/jacobcxdev/cq/internal/installer"
	"github.com/jacobcxdev/cq/internal/installstate"
	"github.com/jacobcxdev/cq/internal/proxy"
)

var (
	ErrServiceUpgradeDeferred    = errors.New("runtime upgrade deferred; previous runtime remains active")
	ErrServiceUpgradeMaintenance = errors.New("predecessor needs maintenance adoption; finish active turns and stop clients before Homebrew upgrade")
)

type serviceRuntimeArtifacts interface {
	Stage(context.Context, string) (installer.RuntimeArtifact, error)
	Verify(context.Context, installer.RuntimeArtifact) error
}

func (lifecycle *serviceLifecycle) Upgrade(ctx context.Context, owner installstate.Owner, candidatePath string) (proxy.RuntimeUpgradeReceiptV1, error) {
	if err := lifecycle.validate(owner); err != nil {
		return proxy.RuntimeUpgradeReceiptV1{}, err
	}
	if owner != installstate.OwnerHomebrew || lifecycle.RuntimeApply == nil || lifecycle.RuntimeArtifacts == nil || lifecycle.RuntimePreflight == nil || lifecycle.RuntimeRestoreRefresh == nil {
		return proxy.RuntimeUpgradeReceiptV1{}, proxy.ErrRuntimeUpgradeUnsupported
	}
	if err := lifecycle.Store.CheckClaim(owner, lifecycle.Executable); err != nil {
		return proxy.RuntimeUpgradeReceiptV1{}, err
	}
	record, err := lifecycle.Store.Load()
	if err != nil {
		return proxy.RuntimeUpgradeReceiptV1{}, err
	}
	if err := lifecycle.RuntimePreflight(ctx, record); err != nil {
		return proxy.RuntimeUpgradeReceiptV1{}, err
	}
	sameRuntime := false
	if lifecycle.RuntimePrune != nil {
		defer func() {
			if sameRuntime {
				return
			}
			cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
			defer cancel()
			if err := lifecycle.RuntimePrune(cleanup); err != nil {
				fmt.Fprintf(os.Stderr, "warning: prune unused runtimes: %v\n", err)
			}
		}()
	}
	candidate, err := lifecycle.RuntimeArtifacts.Stage(ctx, candidatePath)
	if err != nil {
		return proxy.RuntimeUpgradeReceiptV1{}, err
	}
	if err := lifecycle.RuntimeArtifacts.Verify(ctx, candidate); err != nil {
		return proxy.RuntimeUpgradeReceiptV1{}, err
	}
	status, err := lifecycle.Platform.Inspect(ctx)
	if err != nil {
		return proxy.RuntimeUpgradeReceiptV1{}, err
	}
	previous, err := lifecycle.RuntimeArtifacts.Stage(ctx, status.Proxy.LiveExecutable)
	if err != nil {
		return proxy.RuntimeUpgradeReceiptV1{}, ErrServiceUpgradeMaintenance
	}
	if err := lifecycle.RuntimeArtifacts.Verify(ctx, previous); err != nil {
		return proxy.RuntimeUpgradeReceiptV1{}, err
	}
	snapshot, err := lifecycle.Platform.Snapshot(ctx)
	if err != nil {
		return proxy.RuntimeUpgradeReceiptV1{}, err
	}
	var receipt proxy.RuntimeUpgradeReceiptV1
	if candidate == previous && status.Proxy.Registered && status.Proxy.Running && status.Proxy.Healthy &&
		sameServiceExecutable(status.Proxy.LiveExecutable, candidate.Path) && lifecycle.RuntimeReceipt != nil {
		if retained, receiptErr := lifecycle.RuntimeReceipt(); receiptErr == nil && retained.Validate() == nil {
			selected := retained.Previous
			if retained.Phase == "committed" {
				selected = retained.Candidate
			}
			terminal := retained.Phase == "committed" || retained.Phase == "deferred" || retained.Phase == "rolled_back"
			bound := record.BinaryDigest == selected.SHA256 || (retained.Phase == "committed" && record.BinaryDigest == retained.Previous.SHA256)
			if terminal && selected == candidate && bound {
				// This is package reconciliation, not another runtime switch.
				// Preserve the prior attempt's truthful receipt and its artifacts.
				receipt, sameRuntime = retained, true
			}
		}
	}
	if !sameRuntime {
		receipt, err = lifecycle.RuntimeApply(ctx, candidate)
		if err != nil {
			return receipt, err
		}
		if receipt.Phase == "deferred" {
			return receipt, fmt.Errorf("%w: %s", ErrServiceUpgradeDeferred, receipt.ErrorCode)
		}
		if receipt.Phase != "committed" {
			return receipt, fmt.Errorf("runtime upgrade %s: %s", receipt.Phase, receipt.ErrorCode)
		}
	}
	lifecycle.RuntimeExecutable = candidate.Path
	rollback := func(cause error) (proxy.RuntimeUpgradeReceiptV1, error) {
		recovery, cancel := context.WithTimeout(context.WithoutCancel(ctx), 90*time.Second)
		defer cancel()
		reverted := receipt
		if !sameRuntime {
			var revertErr error
			reverted, revertErr = lifecycle.RuntimeApply(recovery, previous)
			if revertErr != nil || reverted.Phase != "committed" {
				return reverted, errors.Join(cause, fmt.Errorf("runtime rollback unverified: %s", reverted.Phase), revertErr)
			}
		}
		lifecycle.RuntimeExecutable = previous.Path
		refreshErr := lifecycle.RuntimeRestoreRefresh(recovery, snapshot)
		stateErr := lifecycle.Store.Save(record)
		return reverted, errors.Join(cause, wrapServiceError("restore previous refresh", refreshErr), wrapServiceError("restore previous ownership", stateErr))
	}
	if err := lifecycle.Platform.InstallRefresh(ctx, candidate.Path); err != nil {
		return rollback(fmt.Errorf("install upgraded refresh: %w", err))
	}
	if _, err := lifecycle.waitHealthy(ctx); err != nil {
		return rollback(err)
	}
	candidateRecord := record
	candidateRecord.Version = candidate.Version
	candidateRecord.BinaryDigest = candidate.SHA256
	if err := lifecycle.Store.Save(candidateRecord); err != nil {
		return rollback(fmt.Errorf("save upgraded ownership: %w", err))
	}
	return receipt, nil
}

func (lifecycle *serviceLifecycle) runtimeExecutable() string {
	if lifecycle.RuntimeExecutable != "" {
		return lifecycle.RuntimeExecutable
	}
	return lifecycle.Executable
}
func (lifecycle *serviceLifecycle) proxyHealthy(status serviceStatus) bool {
	if lifecycle.RuntimeExecutable != "" {
		return status.Proxy.Registered && status.Proxy.Running && status.Proxy.Healthy && sameServiceExecutable(status.Proxy.LiveExecutable, lifecycle.RuntimeExecutable)
	}
	return status.proxyHealthyFor(lifecycle.Executable)
}
func (lifecycle *serviceLifecycle) servicesHealthy(status serviceStatus) bool {
	return lifecycle.proxyHealthy(status) && status.Refresh.Registered && status.Refresh.Healthy && sameServiceExecutable(status.Refresh.ConfiguredExecutable, lifecycle.runtimeExecutable())
}
