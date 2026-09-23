package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"time"

	"github.com/jacobcxdev/cq/internal/fsutil"
	"github.com/jacobcxdev/cq/internal/installer"
	"github.com/jacobcxdev/cq/internal/installstate"
	"github.com/jacobcxdev/cq/internal/userdirs"
)

const (
	serviceStatusSchemaVersion   = 1
	serviceSnapshotSchemaVersion = 1
	maxServiceSnapshotBytes      = 3 << 20
)

var ErrServiceUnhealthy = errors.New("CQ services are unhealthy")

// serviceObservation is native evidence for canonical service commands. It is
// deliberately excluded from the frozen schema1 machine status.
type serviceObservation struct {
	Enabled      *bool
	Healthy      *bool
	Owner        string
	Roots        *userdirs.Roots
	LastRunAt    *time.Time
	LastExitCode *int
	ErrorCode    string
}

type serviceSelection string

const (
	serviceAll     serviceSelection = "all"
	serviceProxy   serviceSelection = "proxy"
	serviceRefresh serviceSelection = "token-refresh"
)

type serviceAction string

const (
	serviceInstall   serviceAction = "install"
	serviceStart     serviceAction = "start"
	serviceStop      serviceAction = "stop"
	serviceRestart   serviceAction = "restart"
	serviceInspect   serviceAction = "status"
	serviceUninstall serviceAction = "uninstall"
)

var errServiceUnavailable = errors.New("selected service platform is unavailable")

type componentStatus struct {
	Observed             *serviceObservation `json:"-"`
	ID                   string              `json:"id"`
	Manager              string              `json:"manager"`
	Registered           bool                `json:"registered"`
	Running              bool                `json:"running"`
	ConfiguredExecutable string              `json:"configured_executable,omitempty"`
	LiveExecutable       string              `json:"live_executable,omitempty"`
	PID                  int                 `json:"pid,omitempty"`
	Listener             string              `json:"listener,omitempty"`
	Healthy              bool                `json:"healthy"`
	LastResult           string              `json:"last_result,omitempty"`
	Error                string              `json:"error,omitempty"`
}

type serviceStatus struct {
	SchemaVersion int                `json:"schema_version"`
	Owner         installstate.Owner `json:"owner,omitempty"`
	Executable    string             `json:"executable,omitempty"`
	Proxy         componentStatus    `json:"proxy"`
	Refresh       componentStatus    `json:"refresh"`
	Conflict      string             `json:"conflict,omitempty"`
}

type servicePlatform interface {
	// Selected operations must never access or restore an unselected component.
	PreflightSelected(context.Context, string, serviceSelection) error
	InspectSelected(context.Context, serviceSelection) (serviceStatus, error)
	SnapshotSelected(context.Context, serviceSelection) (servicePlatformSnapshot, error)
	RestoreSelected(context.Context, serviceSelection, servicePlatformSnapshot) error
	StartProxy(context.Context) error
	StopProxy(context.Context) error
	StartRefresh(context.Context) error
	StopRefresh(context.Context) error
	Preflight(context.Context, string) error
	PrepareRollback(context.Context) (serviceRestore, error)
	Snapshot(context.Context) (servicePlatformSnapshot, error)
	Restore(context.Context, servicePlatformSnapshot) error
	InstallProxy(context.Context, string) error
	InstallRefresh(context.Context, string) error
	RestartProxy(context.Context) error
	RestartRefresh(context.Context) error
	RemoveProxy(context.Context) error
	RemoveRefresh(context.Context) error
	Inspect(context.Context) (serviceStatus, error)
}

type serviceRestore func(context.Context) error

type servicePlatformSnapshot struct {
	Manager                  string                     `json:"manager"`
	FolderExists             bool                       `json:"folder_exists,omitempty"`
	FolderSecurityDescriptor string                     `json:"folder_security_descriptor,omitempty"`
	Components               []serviceComponentSnapshot `json:"components"`
}

type serviceComponentSnapshot struct {
	ID            string `json:"id"`
	Definition    []byte `json:"definition,omitempty"`
	Exists        bool   `json:"exists"`
	Enabled       bool   `json:"enabled,omitempty"`
	UnitFileState string `json:"unit_file_state,omitempty"`
	Running       bool   `json:"running,omitempty"`
}

type persistedServiceSnapshot struct {
	SchemaVersion int                     `json:"schema_version"`
	Owner         installstate.Owner      `json:"owner"`
	Executable    string                  `json:"executable"`
	Platform      servicePlatformSnapshot `json:"platform"`
}

type serviceStateStore interface {
	Load() (installstate.Record, error)
	Save(installstate.Record) error
	Remove() error
	CheckClaim(installstate.Owner, string) error
}

type serviceLifecycle struct {
	Platform         servicePlatform
	Store            serviceStateStore
	Executable       string
	Version          string
	StatusAttempts   int
	StatusInterval   time.Duration
	Wait             func(context.Context, time.Duration) error
	DigestExecutable func(string) (string, error)
	MutationLocker   installer.InstallerLocker
}

func (lifecycle *serviceLifecycle) Install(ctx context.Context, owner installstate.Owner) (returnErr error) {
	if err := lifecycle.validate(owner); err != nil {
		return err
	}
	if err := lifecycle.Store.CheckClaim(owner, lifecycle.Executable); err != nil {
		return err
	}
	if err := lifecycle.Platform.Preflight(ctx, lifecycle.Executable); err != nil {
		return fmt.Errorf("service preflight: %w", err)
	}
	_, err := lifecycle.Platform.Inspect(ctx)
	if err != nil {
		return fmt.Errorf("inspect services before install: %w", err)
	}
	restore, err := lifecycle.Platform.PrepareRollback(ctx)
	if err != nil {
		return fmt.Errorf("snapshot services before install: %w", err)
	}
	if err := lifecycle.Platform.InstallProxy(ctx, lifecycle.Executable); err != nil {
		return lifecycle.rollbackNew(ctx, restore, fmt.Errorf("install proxy service: %w", err))
	}
	if err := lifecycle.waitProxyHealthy(ctx); err != nil {
		return lifecycle.rollbackNew(ctx, restore, err)
	}
	if err := lifecycle.Platform.InstallRefresh(ctx, lifecycle.Executable); err != nil {
		return lifecycle.rollbackNew(ctx, restore, fmt.Errorf("install refresh service: %w", err))
	}
	status, err := lifecycle.waitHealthy(ctx)
	if err != nil {
		return lifecycle.rollbackNew(ctx, restore, err)
	}
	digestExecutable := lifecycle.DigestExecutable
	if digestExecutable == nil {
		digestExecutable = installstate.DigestFile
	}
	binaryDigest, err := digestExecutable(lifecycle.Executable)
	if err != nil {
		return lifecycle.rollbackNew(ctx, restore, fmt.Errorf("digest service executable: %w", err))
	}
	record := installstate.Record{
		SchemaVersion: installstate.CurrentSchemaVersion,
		Owner:         owner,
		Version:       lifecycle.Version,
		Executable:    lifecycle.Executable,
		BinaryDigest:  binaryDigest,
		Services:      []string{status.Proxy.ID, status.Refresh.ID},
	}
	if err := lifecycle.Store.Save(record); err != nil {
		return lifecycle.rollbackNew(ctx, restore, fmt.Errorf("save service ownership: %w", err))
	}
	return nil
}

func (lifecycle *serviceLifecycle) Restart(ctx context.Context) error {
	if err := lifecycle.validateWithoutOwner(); err != nil {
		return err
	}
	if err := lifecycle.Platform.RestartProxy(ctx); err != nil {
		return fmt.Errorf("restart proxy service: %w", err)
	}
	if err := lifecycle.waitProxyHealthy(ctx); err != nil {
		return err
	}
	if err := lifecycle.Platform.RestartRefresh(ctx); err != nil {
		return fmt.Errorf("restart refresh service: %w", err)
	}
	if _, err := lifecycle.waitHealthy(ctx); err != nil {
		return err
	}
	return nil
}

func (lifecycle *serviceLifecycle) Snapshot(ctx context.Context, owner installstate.Owner, path string) error {
	if err := lifecycle.validate(owner); err != nil {
		return err
	}
	if err := validateServiceSnapshotPath(path); err != nil {
		return err
	}
	if err := lifecycle.Store.CheckClaim(owner, lifecycle.Executable); err != nil {
		return err
	}
	if err := lifecycle.Platform.Preflight(ctx, lifecycle.Executable); err != nil {
		return fmt.Errorf("service snapshot preflight: %w", err)
	}
	platformSnapshot, err := lifecycle.Platform.Snapshot(ctx)
	if err != nil {
		return fmt.Errorf("snapshot services: %w", err)
	}
	data, err := json.Marshal(persistedServiceSnapshot{
		SchemaVersion: serviceSnapshotSchemaVersion,
		Owner:         owner,
		Executable:    lifecycle.Executable,
		Platform:      platformSnapshot,
	})
	if err != nil {
		return fmt.Errorf("encode service snapshot: %w", err)
	}
	if len(data) > maxServiceSnapshotBytes {
		return fmt.Errorf("service snapshot exceeds size limit")
	}
	if err := fsutil.SecureAtomicWrite(fsutil.OSFileSystem{}, path, data); err != nil {
		return fmt.Errorf("write service snapshot: %w", err)
	}
	return nil
}

func (lifecycle *serviceLifecycle) Restore(ctx context.Context, owner installstate.Owner, path string) error {
	if err := lifecycle.validate(owner); err != nil {
		return err
	}
	if err := validateServiceSnapshotPath(path); err != nil {
		return err
	}
	data, err := fsutil.ReadSecureFile(fsutil.OSFileSystem{}, path, maxServiceSnapshotBytes)
	if err != nil {
		return fmt.Errorf("read service snapshot: %w", err)
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var snapshot persistedServiceSnapshot
	if err := decoder.Decode(&snapshot); err != nil {
		return fmt.Errorf("decode service snapshot: %w", err)
	}
	if err := requireServiceSnapshotEOF(decoder); err != nil {
		return err
	}
	if snapshot.SchemaVersion != serviceSnapshotSchemaVersion || snapshot.Owner != owner || snapshot.Executable != lifecycle.Executable {
		return fmt.Errorf("service snapshot identity differs")
	}
	if err := lifecycle.Platform.Restore(ctx, snapshot.Platform); err != nil {
		return fmt.Errorf("restore services: %w", err)
	}
	restored, err := lifecycle.Platform.Snapshot(ctx)
	if err != nil {
		return fmt.Errorf("verify restored services: %w", err)
	}
	if !sameServicePlatformSnapshot(restored, snapshot.Platform) {
		return fmt.Errorf("restored services differ from snapshot")
	}
	return nil
}

func (lifecycle *serviceLifecycle) Status(ctx context.Context) (serviceStatus, error) {
	if err := lifecycle.validateWithoutOwner(); err != nil {
		return serviceStatus{}, err
	}
	status, err := lifecycle.Platform.Inspect(ctx)
	if err != nil {
		return serviceStatus{}, fmt.Errorf("inspect services: %w", err)
	}
	status.SchemaVersion = serviceStatusSchemaVersion
	record, err := lifecycle.Store.Load()
	if err == nil {
		status.Owner = record.Owner
		status.Executable = record.Executable
	} else if !errors.Is(err, installstate.ErrNotInstalled) {
		return serviceStatus{}, fmt.Errorf("load service ownership: %w", err)
	}
	return status, nil
}

func (lifecycle *serviceLifecycle) Uninstall(ctx context.Context, owner installstate.Owner) error {
	if err := lifecycle.validate(owner); err != nil {
		return err
	}
	record, err := lifecycle.Store.Load()
	if errors.Is(err, installstate.ErrNotInstalled) {
		status, inspectErr := lifecycle.Platform.Inspect(ctx)
		if inspectErr != nil {
			return fmt.Errorf("inspect unowned services: %w", inspectErr)
		}
		if !status.Proxy.Registered && !status.Refresh.Registered {
			return nil
		}
		return fmt.Errorf("%w: CQ services exist without installation state", installstate.ErrOwnershipConflict)
	}
	if err != nil {
		return fmt.Errorf("load service ownership: %w", err)
	}
	if record.Owner != owner || record.Executable != lifecycle.Executable {
		return fmt.Errorf(
			"%w: existing owner %q executable %q; requested owner %q executable %q",
			installstate.ErrOwnershipConflict,
			record.Owner,
			record.Executable,
			owner,
			lifecycle.Executable,
		)
	}
	digestExecutable := lifecycle.DigestExecutable
	if digestExecutable == nil {
		digestExecutable = installstate.DigestFile
	}
	digest, err := digestExecutable(lifecycle.Executable)
	if err != nil || digest != record.BinaryDigest {
		return fmt.Errorf("%w: installed executable digest differs", installstate.ErrOwnershipConflict)
	}
	if err := lifecycle.Platform.Preflight(ctx, lifecycle.Executable); err != nil {
		return fmt.Errorf("service ownership preflight: %w", err)
	}
	status, err := lifecycle.Platform.Inspect(ctx)
	if err != nil {
		return fmt.Errorf("inspect services before uninstall: %w", err)
	}
	wantServices := []string{status.Proxy.ID, status.Refresh.ID}
	if !sameServiceIDs(record.Services, wantServices) {
		return fmt.Errorf("%w: recorded service identifiers differ", installstate.ErrOwnershipConflict)
	}

	removeErr := errors.Join(
		wrapServiceError("remove refresh service", lifecycle.Platform.RemoveRefresh(ctx)),
		wrapServiceError("remove proxy service", lifecycle.Platform.RemoveProxy(ctx)),
	)
	if removeErr != nil {
		return removeErr
	}
	status, err = lifecycle.Platform.Inspect(ctx)
	if err != nil {
		return fmt.Errorf("inspect services after uninstall: %w", err)
	}
	if status.Proxy.Registered || status.Refresh.Registered {
		return fmt.Errorf("services remain registered after uninstall")
	}
	if err := lifecycle.Store.Remove(); err != nil {
		return fmt.Errorf("remove service ownership: %w", err)
	}
	return nil
}

func sameServiceIDs(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func validateServiceSnapshotPath(path string) error {
	if path == "" || !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return fmt.Errorf("service snapshot path must be a clean absolute path")
	}
	return nil
}

func requireServiceSnapshotEOF(decoder *json.Decoder) error {
	var extra any
	err := decoder.Decode(&extra)
	if errors.Is(err, io.EOF) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("decode service snapshot trailer: %w", err)
	}
	return fmt.Errorf("service snapshot contains trailing data")
}

func sameServicePlatformSnapshot(left, right servicePlatformSnapshot) bool {
	if left.Manager != right.Manager || left.FolderExists != right.FolderExists || left.FolderSecurityDescriptor != right.FolderSecurityDescriptor || len(left.Components) != len(right.Components) {
		return false
	}
	for index := range left.Components {
		leftComponent := left.Components[index]
		rightComponent := right.Components[index]
		if leftComponent.ID != rightComponent.ID || leftComponent.Exists != rightComponent.Exists || leftComponent.Enabled != rightComponent.Enabled || leftComponent.UnitFileState != rightComponent.UnitFileState || leftComponent.Running != rightComponent.Running {
			return false
		}
		if leftComponent.Exists && !bytes.Equal(leftComponent.Definition, rightComponent.Definition) {
			return false
		}
	}
	return true
}

func (lifecycle *serviceLifecycle) waitHealthy(ctx context.Context) (serviceStatus, error) {
	attempts := lifecycle.StatusAttempts
	if attempts <= 0 {
		attempts = 20
	}
	interval := lifecycle.StatusInterval
	if interval <= 0 {
		interval = time.Second
	}
	wait := lifecycle.Wait
	if wait == nil {
		wait = waitForServicePoll
	}

	var status serviceStatus
	var inspectErr error
	for attempt := 0; attempt < attempts; attempt++ {
		status, inspectErr = lifecycle.Platform.Inspect(ctx)
		if inspectErr == nil && status.healthyFor(lifecycle.Executable) {
			return status, nil
		}
		if attempt+1 < attempts {
			if err := wait(ctx, interval); err != nil {
				return serviceStatus{}, err
			}
		}
	}
	if inspectErr != nil {
		return serviceStatus{}, fmt.Errorf("%w: inspect: %v", ErrServiceUnhealthy, inspectErr)
	}
	return serviceStatus{}, fmt.Errorf(
		"%w: proxy registered=%t running=%t healthy=%t result=%q; refresh registered=%t healthy=%t result=%q",
		ErrServiceUnhealthy,
		status.Proxy.Registered,
		status.Proxy.Running,
		status.Proxy.Healthy,
		status.Proxy.LastResult,
		status.Refresh.Registered,
		status.Refresh.Healthy,
		status.Refresh.LastResult,
	)
}

func (lifecycle *serviceLifecycle) waitProxyHealthy(ctx context.Context) error {
	attempts := lifecycle.StatusAttempts
	if attempts <= 0 {
		attempts = 20
	}
	interval := lifecycle.StatusInterval
	if interval <= 0 {
		interval = time.Second
	}
	wait := lifecycle.Wait
	if wait == nil {
		wait = waitForServicePoll
	}

	var status serviceStatus
	var inspectErr error
	for attempt := 0; attempt < attempts; attempt++ {
		status, inspectErr = lifecycle.Platform.Inspect(ctx)
		if inspectErr == nil && status.proxyHealthyFor(lifecycle.Executable) {
			return nil
		}
		if attempt+1 < attempts {
			if err := wait(ctx, interval); err != nil {
				return err
			}
		}
	}
	if inspectErr != nil {
		return fmt.Errorf("%w: inspect proxy: %v", ErrServiceUnhealthy, inspectErr)
	}
	return fmt.Errorf(
		"%w: proxy registered=%t running=%t healthy=%t",
		ErrServiceUnhealthy,
		status.Proxy.Registered,
		status.Proxy.Running,
		status.Proxy.Healthy,
	)
}

func (lifecycle *serviceLifecycle) rollbackNew(ctx context.Context, restore serviceRestore, cause error) error {
	if restore == nil {
		return errors.Join(cause, fmt.Errorf("service rollback is unavailable"))
	}
	return errors.Join(cause, wrapServiceError("restore previous services", restore(ctx)))
}

func (lifecycle *serviceLifecycle) validate(owner installstate.Owner) error {
	if !owner.Valid() {
		return fmt.Errorf("invalid service owner %q", owner)
	}
	return lifecycle.validateWithoutOwner()
}

func (lifecycle *serviceLifecycle) validateWithoutOwner() error {
	if lifecycle == nil || lifecycle.Platform == nil || lifecycle.Store == nil {
		return fmt.Errorf("service lifecycle is unavailable")
	}
	if lifecycle.Executable == "" || !filepath.IsAbs(lifecycle.Executable) || filepath.Clean(lifecycle.Executable) != lifecycle.Executable {
		return fmt.Errorf("service executable must be a clean absolute path")
	}
	if lifecycle.Version == "" {
		return fmt.Errorf("service version is empty")
	}
	return nil
}

func (status serviceStatus) healthyFor(executable string) bool {
	return status.proxyHealthyFor(executable) &&
		status.Refresh.Registered &&
		status.Refresh.Healthy &&
		sameServiceExecutable(status.Refresh.ConfiguredExecutable, executable)
}

func (status serviceStatus) proxyHealthyFor(executable string) bool {
	return status.Proxy.Registered &&
		status.Proxy.Running &&
		status.Proxy.Healthy &&
		sameServiceExecutable(status.Proxy.ConfiguredExecutable, executable) &&
		sameServiceExecutable(status.Proxy.LiveExecutable, executable)
}

func waitForServicePoll(ctx context.Context, duration time.Duration) error {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func wrapServiceError(operation string, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("%s: %w", operation, err)
}

// Selected is the canonical lifecycle. The legacy methods above retain the
// whole-installation machine ABI used by package transactions.
type serviceOperationResult struct {
	Status   serviceStatus
	Rollback string
}
type serviceComponentError struct {
	Component serviceSelection
	Cause     error
}

func (err *serviceComponentError) Error() string {
	return fmt.Sprintf("service %s: %v", err.Component, err.Cause)
}
func (err *serviceComponentError) Unwrap() error { return err.Cause }
func (selection serviceSelection) components() []serviceSelection {
	if selection == serviceAll {
		return []serviceSelection{serviceProxy, serviceRefresh}
	}
	if selection == serviceProxy || selection == serviceRefresh {
		return []serviceSelection{selection}
	}
	return nil
}
func (status serviceStatus) component(id serviceSelection) componentStatus {
	if id == serviceProxy {
		return status.Proxy
	}
	return status.Refresh
}
func (status *serviceStatus) setComponent(id serviceSelection, c componentStatus) {
	if id == serviceProxy {
		status.Proxy = c
	} else {
		status.Refresh = c
	}
}

// selectedServiceContext distinguishes canonical semantics in native methods also
// called by the frozen machine lifecycle, without retaining adapter state.
type selectedServiceContextKey struct{}

func selectedServiceContext(ctx context.Context) bool {
	selected, _ := ctx.Value(selectedServiceContextKey{}).(bool)
	return selected
}

func (lifecycle *serviceLifecycle) Selected(ctx context.Context, action serviceAction, selection serviceSelection, strict bool) (result serviceOperationResult, returnErr error) {
	ctx = context.WithValue(ctx, selectedServiceContextKey{}, true)
	result.Rollback = "not_needed"
	if err := lifecycle.validateWithoutOwner(); err != nil {
		return result, errors.Join(errServiceUnavailable, err)
	}
	components := selection.components()
	if len(components) == 0 {
		return result, fmt.Errorf("invalid service selection")
	}
	switch action {
	case serviceInstall, serviceStart, serviceStop, serviceRestart, serviceInspect, serviceUninstall:
	default:
		return result, fmt.Errorf("invalid service action")
	}
	if action != serviceInspect {
		if lifecycle.MutationLocker == nil {
			return result, errServiceUnavailable
		}
		lock, err := lifecycle.acquireSelectedLock(ctx)
		if err != nil {
			return result, err
		}
		defer func() { returnErr = errors.Join(returnErr, lock.Close()) }()
	}
	if err := ctx.Err(); err != nil {
		return result, err
	}
	status, err := lifecycle.Platform.InspectSelected(ctx, selection)
	if err != nil {
		return result, err
	}
	result.Status = status
	if err := ctx.Err(); err != nil {
		return result, err
	}
	record, loadErr := lifecycle.Store.Load()
	if loadErr != nil && !errors.Is(loadErr, installstate.ErrNotInstalled) {
		return result, loadErr
	}
	recordExists := loadErr == nil
	defer func() {
		if recordExists {
			result.Status = selectedServiceOwnership(result.Status, selection, record)
		}
	}()
	// Ownership is never inferred from process liveness or an unknown record.
	if recordExists {
		status = selectedServiceOwnership(status, selection, record)
	}
	result.Status = status
	if err := ctx.Err(); err != nil {
		return result, err
	}
	if action == serviceInspect {
		if strict {
			for _, id := range components {
				if !status.component(id).Registered {
					return result, &serviceComponentError{id, installstate.ErrNotInstalled}
				}
			}
			for _, id := range components {
				if !serviceSelectedHealthy(id, status.component(id)) {
					return result, &serviceComponentError{id, ErrServiceUnhealthy}
				}
			}
		}
		return result, nil
	}
	for _, id := range components {
		c := status.component(id)
		if c.Registered && (c.Observed == nil || (c.Observed.Owner != "cq" && c.Observed.Owner != "package") || !recordExists || !record.HasService(c.ID) || !sameServiceExecutable(c.ConfiguredExecutable, record.Executable)) {
			return result, installstate.ErrOwnershipConflict
		}
		if (action == serviceStart || action == serviceRestart) && !c.Registered {
			return result, &serviceComponentError{id, installstate.ErrNotInstalled}
		}
	}
	if action == serviceInstall || action == serviceUninstall {
		if recordExists && (record.Owner != installstate.OwnerManual || !sameServiceExecutable(record.Executable, lifecycle.Executable)) {
			return result, installstate.ErrOwnershipConflict
		}
	}
	if action == serviceUninstall && !recordExists {
		return result, nil
	}
	executable := lifecycle.Executable
	if recordExists && action != serviceInstall {
		executable = record.Executable
	}
	if err := lifecycle.Platform.PreflightSelected(ctx, executable, selection); err != nil {
		return result, err
	}
	if err := ctx.Err(); err != nil {
		return result, err
	}
	unselectedOwnership := false
	if recordExists {
		selectedIDs := make(map[string]bool, len(components))
		for _, id := range components {
			selectedIDs[status.component(id).ID] = true
		}
		for _, id := range record.Services {
			if !selectedIDs[id] {
				unselectedOwnership = true
			}
		}
	}
	digest := ""
	if action == serviceInstall || (action == serviceUninstall && recordExists) {
		digestFile := lifecycle.DigestExecutable
		if digestFile == nil {
			digestFile = installstate.DigestFile
		}
		digest, err = digestFile(executable)
		if err != nil {
			return result, err
		}
		if action == serviceInstall && recordExists && unselectedOwnership && digest != record.BinaryDigest {
			return result, installstate.ErrOwnershipConflict
		}
		if action == serviceUninstall && digest != record.BinaryDigest {
			return result, installstate.ErrOwnershipConflict
		}
	}
	if err := ctx.Err(); err != nil {
		return result, err
	}
	snapshot, err := lifecycle.Platform.SnapshotSelected(ctx, selection)
	if err != nil {
		return result, err
	}
	ownershipTouched := false
	mutationAttempted := false
	rollback := func(cause error) (serviceOperationResult, error) {
		if !mutationAttempted {
			return result, cause
		}
		result.Rollback = "failed"
		// Restoration may alter every selected component, including components
		// whose earlier mutation was already verified. Only fresh inspection below
		// can establish their current enablement and health again.
		for _, id := range components {
			uncertain := result.Status.component(id)
			uncertain.Observed = nil
			result.Status.setComponent(id, uncertain)
		}
		// Cleanup shares the original deadline. Expiry is never proof of rollback.
		restoreErr := ctx.Err()
		if restoreErr == nil {
			restoreErr = lifecycle.Platform.RestoreSelected(ctx, selection, snapshot)
		}
		if ownershipTouched && ctx.Err() == nil {
			if recordExists {
				restoreErr = errors.Join(restoreErr, lifecycle.Store.Save(record))
			} else {
				restoreErr = errors.Join(restoreErr, lifecycle.Store.Remove())
			}
		}
		restoreErr = errors.Join(restoreErr, ctx.Err())
		if restoreErr == nil {
			restored, snapshotErr := lifecycle.Platform.SnapshotSelected(ctx, selection)
			if snapshotErr != nil {
				restoreErr = snapshotErr
			} else if !sameServicePlatformSnapshot(restored, snapshot) {
				restoreErr = fmt.Errorf("selected rollback differs from snapshot")
			}
		}
		if restoreErr == nil && ownershipTouched {
			restored, loadErr := lifecycle.Store.Load()
			if recordExists {
				if loadErr != nil || !sameServiceOwnership(restored, record) {
					restoreErr = fmt.Errorf("restored ownership differs")
				}
			} else if !errors.Is(loadErr, installstate.ErrNotInstalled) {
				restoreErr = fmt.Errorf("ownership remains after rollback")
			}
		}
		if restoreErr == nil && ctx.Err() == nil {
			result.Rollback = "restored"
		}
		if ctx.Err() == nil {
			if observed, inspectErr := lifecycle.Platform.InspectSelected(ctx, selection); inspectErr == nil {
				result.Status = observed
			}
		}
		return result, errors.Join(cause, restoreErr)
	}
	order := append([]serviceSelection(nil), components...)
	if action == serviceStop || action == serviceUninstall {
		for i, j := 0, len(order)-1; i < j; i, j = i+1, j-1 {
			order[i], order[j] = order[j], order[i]
		}
	}
	for _, id := range order {
		before := status.component(id)
		if (action == serviceStop || action == serviceUninstall) && !before.Registered {
			continue
		}
		if action == serviceStart && before.Observed != nil && before.Observed.Enabled != nil && *before.Observed.Enabled && serviceSelectedHealthy(id, before) {
			continue
		}
		if action == serviceStop && before.Observed != nil && before.Observed.Enabled != nil && !*before.Observed.Enabled && !before.Running {
			continue
		}
		if err := ctx.Err(); err != nil {
			return rollback(err)
		}
		requested := time.Now()
		mutationAttempted = true
		// Once a manager call begins, old enablement/health is not a current
		// observation. Keep completed sibling observations if verification times out.
		uncertain := before
		uncertain.Observed = nil
		result.Status.setComponent(id, uncertain)
		if err := lifecycle.mutateSelected(ctx, action, id); err != nil {
			return rollback(&serviceComponentError{id, err})
		}
		observed, verifyErr := lifecycle.waitSelected(ctx, action, id, before, requested)
		if observed.ID != "" {
			result.Status.setComponent(id, observed)
		}
		if verifyErr != nil {
			return rollback(&serviceComponentError{id, verifyErr})
		}
	}
	if action == serviceInstall || action == serviceUninstall {
		next := record
		if !recordExists {
			next = installstate.Record{SchemaVersion: installstate.CurrentSchemaVersion, Owner: installstate.OwnerManual, Version: lifecycle.Version, Executable: lifecycle.Executable, BinaryDigest: digest}
		}
		ids := make([]string, 0, len(components))
		for _, id := range components {
			ids = append(ids, result.Status.component(id).ID)
		}
		if action == serviceInstall {
			if !unselectedOwnership {
				next.Version = lifecycle.Version
				next.BinaryDigest = digest
			}
			next = next.WithServices(ids...)
		} else {
			next = next.WithoutServices(ids...)
		}
		if !sameServiceOwnership(next, record) {
			if err := ctx.Err(); err != nil {
				return rollback(err)
			}
			ownershipTouched = true
			mutationAttempted = true
			if len(next.Services) == 0 {
				err = lifecycle.Store.Remove()
			} else {
				err = lifecycle.Store.Save(next)
			}
			if err != nil {
				return rollback(err)
			}
			if err := ctx.Err(); err != nil {
				return rollback(err)
			}
			saved, loadErr := lifecycle.Store.Load()
			if len(next.Services) == 0 {
				if !errors.Is(loadErr, installstate.ErrNotInstalled) {
					return rollback(fmt.Errorf("removed ownership still exists"))
				}
			} else if loadErr != nil || !sameServiceOwnership(saved, next) {
				return rollback(fmt.Errorf("saved ownership differs"))
			}
		}
	}
	if err := ctx.Err(); err != nil {
		return rollback(err)
	}
	return result, nil
}

func (lifecycle *serviceLifecycle) acquireSelectedLock(ctx context.Context) (installer.InstallLock, error) {
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		lock, err := lifecycle.MutationLocker.Acquire()
		if !errors.Is(err, installer.ErrInstallationInProgress) {
			return lock, err
		}
		if err := waitForServicePoll(ctx, 10*time.Millisecond); err != nil {
			return nil, err
		}
	}
}
func sameServiceOwnership(a, b installstate.Record) bool {
	return a.SchemaVersion == b.SchemaVersion && a.Owner == b.Owner && a.Version == b.Version && a.Executable == b.Executable && a.BinaryDigest == b.BinaryDigest && sameServiceIDs(a.Services, b.Services)
}
func serviceSelectedHealthy(id serviceSelection, c componentStatus) bool {
	value := projectV2ServiceComponent(id, c, time.Now())
	return value.Healthy != nil && *value.Healthy
}
func (lifecycle *serviceLifecycle) mutateSelected(ctx context.Context, action serviceAction, id serviceSelection) error {
	if id == serviceProxy {
		switch action {
		case serviceInstall:
			return lifecycle.Platform.InstallProxy(ctx, lifecycle.Executable)
		case serviceStart:
			return lifecycle.Platform.StartProxy(ctx)
		case serviceStop:
			return lifecycle.Platform.StopProxy(ctx)
		case serviceRestart:
			return lifecycle.Platform.RestartProxy(ctx)
		case serviceUninstall:
			return lifecycle.Platform.RemoveProxy(ctx)
		}
	} else {
		switch action {
		case serviceInstall:
			return lifecycle.Platform.InstallRefresh(ctx, lifecycle.Executable)
		case serviceStart:
			return lifecycle.Platform.StartRefresh(ctx)
		case serviceStop:
			return lifecycle.Platform.StopRefresh(ctx)
		case serviceRestart:
			return lifecycle.Platform.RestartRefresh(ctx)
		case serviceUninstall:
			return lifecycle.Platform.RemoveRefresh(ctx)
		}
	}
	return fmt.Errorf("invalid selected service mutation")
}
func (lifecycle *serviceLifecycle) waitSelected(ctx context.Context, action serviceAction, id serviceSelection, before componentStatus, requested time.Time) (componentStatus, error) {
	attempts := lifecycle.StatusAttempts
	if attempts <= 0 {
		attempts = 20
	}
	interval := lifecycle.StatusInterval
	if interval <= 0 {
		interval = time.Second
	}
	wait := lifecycle.Wait
	if wait == nil {
		wait = waitForServicePoll
	}
	var c componentStatus
	var inspectErr error
	for attempt := 0; attempt < attempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return c, err
		}
		status, err := lifecycle.Platform.InspectSelected(ctx, id)
		inspectErr = err
		if err == nil {
			c = status.component(id)
			verified := false
			switch action {
			case serviceUninstall:
				verified = !c.Registered && !c.Running
			case serviceStop:
				verified = c.Registered && !c.Running && c.Observed != nil && c.Observed.Enabled != nil && !*c.Observed.Enabled
			default:
				if c.Observed != nil && c.Observed.Enabled != nil {
					expectedExecutable := before.ConfiguredExecutable
					if action == serviceInstall {
						expectedExecutable = lifecycle.Executable
					}
					identityOK := c.Registered && sameServiceExecutable(c.ConfiguredExecutable, expectedExecutable)
					enabled := *c.Observed.Enabled
					policyOK := enabled
					if action == serviceRestart {
						policyOK = before.Observed != nil && before.Observed.Enabled != nil && enabled == *before.Observed.Enabled
					}
					if id == serviceProxy {
						verified = identityOK && policyOK && serviceSelectedHealthy(id, c)
					} else {
						fresh := c.Observed.LastRunAt != nil && c.Observed.LastExitCode != nil && *c.Observed.LastExitCode == 0 && !c.Observed.LastRunAt.Before(requested.Truncate(time.Second))
						if before.Observed != nil && before.Observed.LastRunAt != nil {
							fresh = fresh && c.Observed.LastRunAt != nil && c.Observed.LastRunAt.After(*before.Observed.LastRunAt)
						}
						projected := projectV2ServiceComponent(id, c, time.Now())
						verified = identityOK && policyOK && fresh && (serviceSelectedHealthy(id, c) || (action == serviceRestart && !enabled && projected.State != "indeterminate"))
					}
				}
			}
			if verified {
				return c, ctx.Err()
			}
		}
		if attempt+1 < attempts {
			if err := wait(ctx, interval); err != nil {
				return c, err
			}
		}
	}
	if inspectErr != nil {
		return c, inspectErr
	}
	return c, ErrServiceUnhealthy
}

func selectedServiceOwnership(status serviceStatus, selection serviceSelection, record installstate.Record) serviceStatus {
	for _, id := range selection.components() {
		c := status.component(id)
		if c.Registered && c.Observed != nil && c.Observed.Owner == "cq" && record.HasService(c.ID) && sameServiceExecutable(c.ConfiguredExecutable, record.Executable) {
			obs := *c.Observed
			if record.Owner != installstate.OwnerManual {
				obs.Owner = "package"
			}
			c.Observed = &obs
			status.setComponent(id, c)
		}
	}
	return status
}
