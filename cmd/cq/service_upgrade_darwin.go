//go:build darwin

package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/jacobcxdev/cq/internal/fsutil"
	"github.com/jacobcxdev/cq/internal/httputil"
	"github.com/jacobcxdev/cq/internal/installer"
	"github.com/jacobcxdev/cq/internal/installstate"
	"github.com/jacobcxdev/cq/internal/proxy"
	"github.com/jacobcxdev/cq/internal/userdirs"
	"golang.org/x/sys/unix"
)

type darwinServiceRuntimeArtifacts struct{ installer.RuntimeArtifactStore }

func (store darwinServiceRuntimeArtifacts) Stage(ctx context.Context, path string) (installer.RuntimeArtifact, error) {
	source, err := filepath.EvalSymlinks(path)
	if err != nil {
		return installer.RuntimeArtifact{}, err
	}
	return store.RuntimeArtifactStore.Stage(ctx, source)
}

func configureDarwinServiceUpgrades(lifecycle *serviceLifecycle, platform *darwinServicePlatform) {
	artifacts := installer.RuntimeArtifactStore{FS: fsutil.OSFileSystem{}, Roots: platform.roots}
	receipts := proxy.RuntimeUpgradeStore{FS: fsutil.OSFileSystem{}, Roots: platform.roots}
	if record, err := lifecycle.Store.Load(); err == nil && record.Owner == installstate.OwnerHomebrew {
		if selected, err := darwinSelectedRuntimeArtifact(context.Background(), artifacts, receipts, record); err == nil {
			lifecycle.RuntimeExecutable = selected.Path
		}
	}
	lifecycle.RuntimeArtifacts = darwinServiceRuntimeArtifacts{RuntimeArtifactStore: artifacts}
	lifecycle.RuntimeReceipt = receipts.Load
	lifecycle.RuntimeApply = func(ctx context.Context, candidate installer.RuntimeArtifact) (proxy.RuntimeUpgradeReceiptV1, error) {
		return applyDarwinServiceRuntimeUpgrade(ctx, artifacts, receipts, candidate)
	}
	lifecycle.RuntimeInitialise = func(ctx context.Context) error {
		for _, label := range []string{proxyAgentLabel, agentLabel} {
			loaded, _, err := platform.printJob(ctx, label)
			if err != nil {
				return err
			}
			if loaded {
				if err := platform.waitJobUnloaded(ctx, label); err != nil {
					return fmt.Errorf("maintenance predecessor remains registered: %w", err)
				}
			}
		}
		return nil
	}
	lifecycle.RuntimePreflight = func(ctx context.Context, record installstate.Record) error {
		if record.Owner != installstate.OwnerHomebrew || record.Executable != lifecycle.Executable || !sameServiceIDs(record.Services, []string{proxyAgentLabel, agentLabel}) {
			return installstate.ErrOwnershipConflict
		}
		selected, err := darwinSelectedRuntimeArtifact(ctx, artifacts, receipts, record)
		if err != nil {
			return err
		}
		for _, check := range []struct {
			label string
			args  []string
		}{{proxyAgentLabel, []string{"proxy", "start"}}, {agentLabel, []string{"refresh"}}} {
			definition, exists, err := platform.readDefinition(check.label)
			if err != nil {
				return err
			}
			if !exists || definition.Label != check.label || len(definition.ProgramArguments) != len(check.args)+1 || !equalStrings(definition.ProgramArguments[1:], check.args) {
				return installstate.ErrOwnershipConflict
			}
			pinned, err := darwinExistingRuntimeArtifact(ctx, artifacts, definition.ProgramArguments[0])
			if err != nil || pinned.Path != definition.ProgramArguments[0] {
				return ErrServiceUpgradeMaintenance
			}
		}
		legacy, _, err := platform.printJob(ctx, homebrewProxyAgentLabel)
		if err != nil {
			return err
		}
		if legacy {
			return installstate.ErrOwnershipConflict
		}
		lifecycle.RuntimeExecutable = selected.Path
		return nil
	}
	lifecycle.RuntimeRestoreRefresh = func(ctx context.Context, snapshot servicePlatformSnapshot) error {
		if snapshot.Manager != "launchd" || len(snapshot.Components) != 2 || snapshot.Components[1].ID != agentLabel {
			return fmt.Errorf("invalid refresh snapshot")
		}
		component := snapshot.Components[1]
		return platform.restore(ctx, agentLabel, platform.plistPath(agentLabel), component.Definition, component.Exists, component.Running)
	}
	lifecycle.RuntimeCleanup = func(ctx context.Context) error { return cleanupDarwinServiceRuntime(ctx, platform, receipts) }
	lifecycle.RuntimePrune = func(ctx context.Context) error {
		return pruneDarwinServiceRuntime(ctx, platform, lifecycle.Store, artifacts, receipts)
	}
	lifecycle.RuntimeSnapshotWrite = func(ctx context.Context, path string, data []byte) error {
		return writeDarwinRuntimeSnapshot(ctx, artifacts, path, data)
	}
	lifecycle.RuntimeSnapshotCheck = func(ctx context.Context, snapshot servicePlatformSnapshot) error {
		paths, err := darwinRuntimeSnapshotPaths(snapshot)
		if err != nil {
			return err
		}
		for _, path := range paths {
			if _, err := darwinExistingRuntimeArtifact(ctx, artifacts, path); err != nil {
				return err
			}
		}
		return nil
	}
}

func pruneDarwinServiceRuntime(ctx context.Context, platform *darwinServicePlatform, ownership serviceStateStore, artifacts installer.RuntimeArtifactStore, receipts proxy.RuntimeUpgradeStore) error {
	err := artifacts.Prune(ctx, func() ([]string, error) {
		record, err := ownership.Load()
		if err != nil {
			return nil, err
		}
		if record.Owner != installstate.OwnerHomebrew || !sameServiceIDs(record.Services, []string{proxyAgentLabel, agentLabel}) {
			return nil, installstate.ErrOwnershipConflict
		}
		paths := []string{filepath.Join(artifacts.Roots.State, "runtime-artifacts", record.BinaryDigest, "cq")}
		receipt, err := receipts.Load()
		if err == nil {
			selected := receipt.Previous
			switch receipt.Phase {
			case "committed":
				selected = receipt.Candidate
			case "deferred", "rolled_back":
			default:
				return nil, proxy.ErrRuntimeUpgradeBusy
			}
			if selected.SHA256 != record.BinaryDigest {
				return nil, proxy.ErrRuntimeUpgradeBusy
			}
			paths = append(paths, receipt.Previous.Path, receipt.Candidate.Path)
		} else if !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
		for _, check := range []struct {
			label string
			args  []string
		}{{proxyAgentLabel, []string{"proxy", "start"}}, {agentLabel, []string{"refresh"}}} {
			definition, exists, err := platform.readDefinition(check.label)
			if err != nil {
				return nil, err
			}
			if !exists || definition.Label != check.label || len(definition.ProgramArguments) != len(check.args)+1 || !equalStrings(definition.ProgramArguments[1:], check.args) {
				return nil, installstate.ErrOwnershipConflict
			}
			paths = append(paths, definition.ProgramArguments[0])
		}
		snapshots, err := darwinRuntimeSnapshotReferences(ctx, artifacts, record.Executable)
		return append(paths, snapshots...), err
	})
	// An unfinished handoff or package reconciliation retains everything until
	// a later upgrade attempt can prove the complete reference set.
	if errors.Is(err, proxy.ErrRuntimeUpgradeBusy) {
		return nil
	}
	return err
}

func darwinExistingRuntimeArtifact(ctx context.Context, artifacts installer.RuntimeArtifactStore, path string) (installer.RuntimeArtifact, error) {
	clean := filepath.Clean(path)
	digest := filepath.Base(filepath.Dir(clean))
	if len(digest) != 64 || clean != filepath.Join(artifacts.Roots.State, "runtime-artifacts", digest, "cq") {
		return installer.RuntimeArtifact{}, ErrServiceUpgradeMaintenance
	}
	decoded, err := hex.DecodeString(digest)
	if err != nil || len(decoded) != 32 {
		return installer.RuntimeArtifact{}, ErrServiceUpgradeMaintenance
	}
	return artifacts.Stage(ctx, clean)
}

func resolveDarwinRetainedServiceRuntime(ctx context.Context, roots userdirs.Roots, current, configured string) (installer.RuntimeArtifact, error) {
	if filepath.Dir(filepath.Dir(configured)) != filepath.Join(roots.State, "runtime-artifacts") {
		return installer.RuntimeArtifact{}, nil
	}
	record, err := (installstate.Store{FS: fsutil.OSFileSystem{}, Roots: roots}).Load()
	// Fresh install verifies its exact configured bootstrap before publishing
	// ownership. Package CLI inspection still requires the ownership record.
	if errors.Is(err, installstate.ErrNotInstalled) && current == configured {
		return installer.RuntimeArtifact{}, nil
	}
	if err != nil {
		return installer.RuntimeArtifact{}, err
	}
	if record.Owner != installstate.OwnerHomebrew || !sameServiceIDs(record.Services, []string{proxyAgentLabel, agentLabel}) {
		return installer.RuntimeArtifact{}, installstate.ErrOwnershipConflict
	}
	artifacts := installer.RuntimeArtifactStore{FS: fsutil.OSFileSystem{}, Roots: roots}
	if pinned, err := darwinExistingRuntimeArtifact(ctx, artifacts, configured); err != nil || pinned.Path != configured {
		return installer.RuntimeArtifact{}, errors.Join(installstate.ErrOwnershipConflict, err)
	}
	receipts := proxy.RuntimeUpgradeStore{FS: fsutil.OSFileSystem{}, Roots: roots}
	selected, err := darwinSelectedRuntimeArtifact(ctx, artifacts, receipts, record)
	if err != nil {
		return selected, err
	}
	if current == configured || current == selected.Path {
		return selected, nil
	}
	packagePath, err := filepath.EvalSymlinks(record.Executable)
	if err != nil || current != packagePath {
		return installer.RuntimeArtifact{}, installstate.ErrOwnershipConflict
	}
	digest, err := installstate.DigestFile(packagePath)
	if err != nil {
		return installer.RuntimeArtifact{}, err
	}
	if digest == selected.SHA256 || digest == record.BinaryDigest {
		return selected, nil
	}
	if receipt, err := receipts.Load(); err == nil && digest == receipt.Candidate.SHA256 {
		if err := artifacts.Verify(ctx, receipt.Candidate); err == nil {
			return selected, nil
		}
	}
	return installer.RuntimeArtifact{}, installstate.ErrOwnershipConflict
}

func darwinSelectedRuntimeArtifact(ctx context.Context, artifacts installer.RuntimeArtifactStore, receipts proxy.RuntimeUpgradeStore, record installstate.Record) (installer.RuntimeArtifact, error) {
	receipt, err := receipts.Load()
	if err == nil {
		selected := receipt.Previous
		if receipt.Phase == "committed" {
			selected = receipt.Candidate
		}
		if record.BinaryDigest != selected.SHA256 && !(receipt.Phase == "committed" && record.BinaryDigest == receipt.Previous.SHA256) {
			return selected, installstate.ErrOwnershipConflict
		}
		return selected, artifacts.Verify(ctx, selected)
	}
	if !errors.Is(err, os.ErrNotExist) {
		return installer.RuntimeArtifact{}, err
	}
	path := filepath.Join(artifacts.Roots.State, "runtime-artifacts", record.BinaryDigest, "cq")
	artifact, err := darwinExistingRuntimeArtifact(ctx, artifacts, path)
	if err != nil {
		return artifact, ErrServiceUpgradeMaintenance
	}
	if artifact.SHA256 != record.BinaryDigest || artifact.Path != path {
		return artifact, installstate.ErrOwnershipConflict
	}
	return artifact, nil
}

func applyDarwinServiceRuntimeUpgrade(ctx context.Context, artifacts installer.RuntimeArtifactStore, store proxy.RuntimeUpgradeStore, candidate installer.RuntimeArtifact) (proxy.RuntimeUpgradeReceiptV1, error) {
	if err := artifacts.Verify(ctx, candidate); err != nil {
		return proxy.RuntimeUpgradeReceiptV1{}, err
	}
	cfg, err := proxy.LoadExistingConfig()
	if err != nil {
		return proxy.RuntimeUpgradeReceiptV1{}, err
	}
	generation := uint64(0)
	previous, err := store.Load()
	if err == nil {
		generation = previous.Generation
	} else if !errors.Is(err, os.ErrNotExist) {
		return previous, err
	}
	transaction := make([]byte, 16)
	if _, err := rand.Read(transaction); err != nil {
		return previous, err
	}
	id := hex.EncodeToString(transaction)
	payload, err := json.Marshal(proxy.RuntimeUpgradeRequestV1{SchemaVersion: 1, TransactionID: id, ExpectedGeneration: generation, Candidate: candidate})
	if err != nil {
		return previous, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, fmt.Sprintf("http://127.0.0.1:%d%s", cfg.Port, proxy.RuntimeUpgradePath), bytes.NewReader(payload))
	if err != nil {
		return previous, err
	}
	request.Header.Set("Authorization", "Bearer "+cfg.LocalToken)
	request.Header.Set("Content-Type", "application/json")
	request.Close = true
	transport := &http.Transport{Proxy: nil, DisableKeepAlives: true, DialContext: (&net.Dialer{Timeout: 5 * time.Second}).DialContext}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 10 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	return submitDarwinServiceRuntimeUpgrade(ctx, store, candidate, id, previous, request, client)
}

func submitDarwinServiceRuntimeUpgrade(ctx context.Context, store proxy.RuntimeUpgradeStore, candidate installer.RuntimeArtifact, id string, previous proxy.RuntimeUpgradeReceiptV1, request *http.Request, client httputil.Doer) (proxy.RuntimeUpgradeReceiptV1, error) {
	response, err := client.Do(request)
	var prepared proxy.RuntimeUpgradeReceiptV1
	submissionErr := err
	if response != nil {
		body, readErr := httputil.ReadBody(response.Body)
		response.Body.Close()
		if err == nil && response.StatusCode != http.StatusAccepted {
			return previous, fmt.Errorf("runtime upgrade request rejected: HTTP %d", response.StatusCode)
		}
		submissionErr = errors.Join(submissionErr, readErr)
		if submissionErr == nil {
			submissionErr = proxy.DecodeRuntimeUpgradePayload(body, &prepared)
			if submissionErr == nil && (prepared.TransactionID != id || prepared.Candidate != candidate) {
				submissionErr = proxy.ErrRuntimeUpgradeReceipt
			}
		}
	}
	// Begin persists before acknowledging. Even an unreadable or lost response
	// must reconcile this known transaction before Homebrew can revert package.
	wait, cancel := context.WithTimeout(context.WithoutCancel(ctx), 90*time.Second)
	defer cancel()
	return awaitDarwinServiceRuntimeUpgrade(wait, store, candidate, id, previous, prepared, submissionErr)
}

func awaitDarwinServiceRuntimeUpgrade(wait context.Context, store proxy.RuntimeUpgradeStore, candidate installer.RuntimeArtifact, id string, previous, prepared proxy.RuntimeUpgradeReceiptV1, submissionErr error) (proxy.RuntimeUpgradeReceiptV1, error) {
	discoveryDeadline := wait.Done()
	for {
		receipt, err := store.Load()
		if err == nil {
			if receipt.TransactionID == id {
				prepared = receipt
				if receipt.Candidate != candidate {
					return receipt, proxy.ErrRuntimeUpgradeReceipt
				}
				switch receipt.Phase {
				case "committed", "deferred", "rolled_back", "failed":
					return receipt, nil
				case "waiting", "handoff", "verifying", "rolling_back":
					// A durable waiting phase proves the controller started its
					// drain. Healthy active turns may outlast discovery or caller
					// cancellation; reconcile their terminal outcome. Prepared
					// alone cannot prove waiting publication succeeded.
					discoveryDeadline = nil
				}
			} else if receipt.Generation > previous.Generation {
				return receipt, proxy.ErrRuntimeUpgradeGeneration
			}
		} else if discoveryDeadline == nil {
			return prepared, errors.Join(submissionErr, fmt.Errorf("runtime selection unverified: %w", err))
		}
		select {
		case <-discoveryDeadline:
			return prepared, errors.Join(submissionErr, err, fmt.Errorf("runtime selection unverified: %w", wait.Err()))
		case <-time.After(20 * time.Millisecond):
		}
	}
}

func cleanupDarwinServiceRuntime(ctx context.Context, platform *darwinServicePlatform, store proxy.RuntimeUpgradeStore) error {
	for _, label := range []string{proxyAgentLabel, agentLabel} {
		loaded, _, err := platform.printJob(ctx, label)
		if err != nil {
			return err
		}
		if loaded {
			return fmt.Errorf("runtime cleanup requires unloaded jobs")
		}
	}
	path := proxy.RuntimeLifecyclePath(platform.roots.State)
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return err
	}
	lifecycle := os.NewFile(uintptr(fd), "cleanup-lifecycle")
	defer lifecycle.Close()
	if err := unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB); err != nil {
		return proxy.ErrRuntimeOwnerReleaseUnproven
	}
	receipt, err := store.Load()
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err == nil {
		if err := proxy.RetireRuntimeUpgradeGuard(ctx, lifecycle, receipt); err != nil {
			return err
		}
	}
	for _, name := range []string{"runtime-artifacts", "runtime-snapshots"} {
		root := filepath.Join(platform.roots.State, name)
		if _, err := os.Lstat(root); err == nil {
			if err := fsutil.ValidateSecureDirectory(fsutil.OSFileSystem{}, root); err != nil {
				return err
			}
			if err := os.RemoveAll(root); err != nil {
				return err
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	if err := os.Remove(store.Path()); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}
