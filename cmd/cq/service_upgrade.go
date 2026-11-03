package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
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
	return lifecycle.upgradeWithWaitTimeout(ctx, owner, candidatePath, 0)
}

// The existing refresh invocation stays alive while its installed candidate is
// pending. Each bounded attempt relinquishes the installer lock before backoff.
func (lifecycle *serviceLifecycle) reconcilePendingUpgrade(ctx context.Context, candidate installer.RuntimeArtifact, refresh func() error) error {
	lifecycle.refreshExecuting = true
	defer func() { lifecycle.refreshExecuting = false }()
	wait := lifecycle.Wait
	if wait == nil {
		wait = waitForServicePoll
	}
	digest := lifecycle.DigestExecutable
	if digest == nil {
		digest = installstate.DigestFile
	}
	nextRefresh := time.Now().Add(30 * time.Minute)
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		done, err := func() (done bool, returnErr error) {
			lock, err := lifecycle.MutationLocker.Acquire()
			if errors.Is(err, installer.ErrInstallationInProgress) {
				// A quiet foreground upgrade starts this refresh after runtime
				// commit, then waits for its successful exit before ownership is
				// saved. Do not wait for that same installer's lock in this phase.
				retained, receiptErr := lifecycle.RuntimeReceipt()
				if receiptErr == nil && retained.Validate() == nil && retained.Phase == "committed" && retained.Candidate == candidate {
					record, stateErr := lifecycle.Store.Load()
					if stateErr == nil && record.Owner == installstate.OwnerHomebrew && record.Executable == lifecycle.Executable && (record.BinaryDigest == retained.Previous.SHA256 || record.BinaryDigest == candidate.SHA256) {
						installed, digestErr := digest(record.Executable)
						if digestErr == nil && installed == candidate.SHA256 {
							if verifyErr := lifecycle.RuntimeArtifacts.Verify(ctx, candidate); verifyErr != nil {
								return true, verifyErr
							}
							return true, nil
						}
					}
				}
				return false, nil
			}
			if err != nil {
				return true, err
			}
			defer func() { returnErr = errors.Join(returnErr, lock.Close()) }()
			record, err := lifecycle.Store.Load()
			if errors.Is(err, installstate.ErrNotInstalled) {
				return true, nil
			}
			if err != nil {
				return true, err
			}
			if record.Owner != installstate.OwnerHomebrew || record.Executable != lifecycle.Executable {
				return true, nil
			}
			receipt, err := lifecycle.RuntimeReceipt()
			if errors.Is(err, os.ErrNotExist) {
				return true, nil
			}
			if err != nil {
				return true, err
			}
			if err := receipt.Validate(); err != nil {
				return true, err
			}
			if receipt.Candidate != candidate {
				return true, nil // A later installation superseded this invocation.
			}
			installed, err := digest(record.Executable)
			if err != nil {
				return true, err
			}
			if installed != candidate.SHA256 {
				return true, nil
			}
			switch receipt.Phase {
			case "prepared", "waiting", "handoff", "verifying", "rolling_back":
				return false, nil
			case "committed":
				if record.BinaryDigest == candidate.SHA256 {
					return true, nil
				}
			case "deferred":
				if receipt.ErrorCode != "quiet_deadline" && receipt.ErrorCode != "supervisor_restarted_before_handoff" {
					return true, fmt.Errorf("runtime upgrade deferred: %s", receipt.ErrorCode)
				}
			default:
				return true, fmt.Errorf("runtime upgrade %s: %s", receipt.Phase, receipt.ErrorCode)
			}
			status, err := lifecycle.Platform.Inspect(ctx)
			if err != nil {
				return true, err
			}
			if status.Proxy.Registered && (!status.Proxy.Running || !status.Proxy.Healthy) {
				return false, nil // Known managed proxy is still starting/recovering.
			}
			receipt, err = lifecycle.upgradeWithWaitTimeout(ctx, installstate.OwnerHomebrew, candidate.Path, time.Second)
			var rpcErr *url.Error
			var networkErr net.Error
			if ctx.Err() == nil && errors.As(err, &rpcErr) && errors.As(rpcErr.Err, &networkErr) {
				fresh, inspectErr := lifecycle.Platform.Inspect(ctx)
				if inspectErr != nil {
					return true, errors.Join(err, inspectErr)
				}
				if fresh.Proxy.Registered && (!fresh.Proxy.Running || !fresh.Proxy.Healthy) {
					// The proxy may stop listening between the health observation
					// and submission. Retry only while the installation still binds
					// this candidate; protocol/auth/ownership errors remain errors.
					current, stateErr := lifecycle.Store.Load()
					if errors.Is(stateErr, installstate.ErrNotInstalled) {
						return true, nil
					}
					if stateErr != nil {
						return true, errors.Join(err, stateErr)
					}
					if current.Owner != record.Owner || current.Executable != record.Executable {
						return true, nil
					}
					if !sameServiceIDs(current.Services, record.Services) || (current.BinaryDigest != record.BinaryDigest && current.BinaryDigest != candidate.SHA256) {
						return true, errors.Join(err, installstate.ErrOwnershipConflict)
					}
					retained, stateErr := lifecycle.RuntimeReceipt()
					if errors.Is(stateErr, os.ErrNotExist) {
						return true, nil
					}
					if stateErr == nil {
						stateErr = retained.Validate()
					}
					if stateErr != nil {
						return true, errors.Join(err, stateErr)
					}
					if retained.Candidate != candidate {
						return true, nil
					}
					installed, stateErr := digest(current.Executable)
					if stateErr != nil {
						return true, errors.Join(err, stateErr)
					}
					if installed != candidate.SHA256 {
						return true, nil
					}
					if verifyErr := lifecycle.RuntimeArtifacts.Verify(ctx, candidate); verifyErr != nil {
						return true, errors.Join(err, verifyErr)
					}
					if retained.Phase == "failed" || retained.Phase == "rolled_back" || (retained.Phase == "deferred" && retained.ErrorCode != "quiet_deadline" && retained.ErrorCode != "supervisor_restarted_before_handoff") {
						return true, err
					}
					return false, nil
				}
			}
			return err != nil || receipt.Phase == "committed", err
		}()
		if err != nil || done {
			return err
		}
		if !time.Now().Before(nextRefresh) {
			if refresh != nil {
				if err := refresh(); err != nil {
					fmt.Fprintln(os.Stderr, "warning: credential refresh failed during pending upgrade")
				}
			}
			nextRefresh = time.Now().Add(30 * time.Minute)
		}
		if err := wait(ctx, 5*time.Second); err != nil {
			return err
		}
	}
}

func validateServiceUpgradeWaitTimeout(timeout time.Duration) error {
	if timeout < 0 || timeout > 30*time.Second || timeout%time.Second != 0 {
		return fmt.Errorf("service upgrade: --wait-timeout must be 0 or whole seconds from 1s to 30s")
	}
	return nil
}

func (lifecycle *serviceLifecycle) upgradeWithWaitTimeout(ctx context.Context, owner installstate.Owner, candidatePath string, waitTimeout time.Duration) (proxy.RuntimeUpgradeReceiptV1, error) {
	if err := validateServiceUpgradeWaitTimeout(waitTimeout); err != nil {
		return proxy.RuntimeUpgradeReceiptV1{}, err
	}
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
		receipt, err = lifecycle.RuntimeApply(ctx, candidate, waitTimeout)
		if err != nil {
			return receipt, err
		}
		if receipt.Phase == "deferred" {
			if receipt.ErrorCode == "quiet_deadline" || receipt.ErrorCode == "supervisor_restarted_before_handoff" {
				// The package and its retry executable remain installed while the
				// selected predecessor finishes admitted work. Ownership still
				// describes that predecessor until activation is committed.
				if !status.Refresh.Registered || (!status.Refresh.Healthy && !lifecycle.refreshExecuting) || !sameServiceExecutable(status.Refresh.ConfiguredExecutable, candidate.Path) {
					err = lifecycle.Platform.InstallRefresh(ctx, candidate.Path)
				}
				if err == nil {
					var pending serviceStatus
					pending, err = lifecycle.Platform.Inspect(ctx)
					// A newly bootstrapped retry has not exited yet. Its successful
					// bootstrap and exact definition establish the pending job;
					// requiring a prior successful exit would reject its first run.
					if err == nil && (!lifecycle.proxyHealthy(pending) || !pending.Refresh.Registered || !sameServiceExecutable(pending.Refresh.ConfiguredExecutable, candidate.Path)) {
						err = ErrServiceUnhealthy
					}
				}
				if err != nil {
					recovery, cancel := context.WithTimeout(context.WithoutCancel(ctx), 90*time.Second)
					defer cancel()
					return receipt, errors.Join(err, wrapServiceError("restore previous refresh", lifecycle.RuntimeRestoreRefresh(recovery, snapshot)))
				}
				return receipt, nil
			}
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
			reverted, revertErr = lifecycle.RuntimeApply(recovery, previous, waitTimeout)
			if revertErr != nil || reverted.Phase != "committed" {
				return reverted, errors.Join(cause, fmt.Errorf("runtime rollback unverified: %s", reverted.Phase), revertErr)
			}
		}
		lifecycle.RuntimeExecutable = previous.Path
		refreshErr := lifecycle.RuntimeRestoreRefresh(recovery, snapshot)
		stateErr := lifecycle.Store.Save(record)
		return reverted, errors.Join(cause, wrapServiceError("restore previous refresh", refreshErr), wrapServiceError("restore previous ownership", stateErr))
	}
	keepRefresh := status.Refresh.Registered && (status.Refresh.Healthy || lifecycle.refreshExecuting) &&
		sameServiceExecutable(status.Refresh.ConfiguredExecutable, candidate.Path)
	if !keepRefresh {
		if err := lifecycle.Platform.InstallRefresh(ctx, candidate.Path); err != nil {
			return rollback(fmt.Errorf("install upgraded refresh: %w", err))
		}
	}
	if lifecycle.refreshExecuting && keepRefresh {
		// This invocation is the exact retained candidate refresh job. Waiting
		// for its successful exit here would wait for ourselves indefinitely.
		if err := lifecycle.waitProxyHealthy(ctx); err != nil {
			return rollback(err)
		}
	} else {
		if _, err := lifecycle.waitHealthy(ctx); err != nil {
			return rollback(err)
		}
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
