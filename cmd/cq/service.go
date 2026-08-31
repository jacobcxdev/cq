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
)

const (
	serviceStatusSchemaVersion   = 1
	serviceSnapshotSchemaVersion = 1
	maxServiceSnapshotBytes      = 3 << 20
)

var ErrServiceUnhealthy = errors.New("CQ services are unhealthy")

type componentStatus struct {
	ID                   string `json:"id"`
	Manager              string `json:"manager"`
	Registered           bool   `json:"registered"`
	Running              bool   `json:"running"`
	ConfiguredExecutable string `json:"configured_executable,omitempty"`
	LiveExecutable       string `json:"live_executable,omitempty"`
	PID                  int    `json:"pid,omitempty"`
	Listener             string `json:"listener,omitempty"`
	Healthy              bool   `json:"healthy"`
	LastResult           string `json:"last_result,omitempty"`
	Error                string `json:"error,omitempty"`
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
	if err := lifecycle.Platform.RestartRefresh(ctx); err != nil {
		return fmt.Errorf("restart refresh service: %w", err)
	}
	if err := lifecycle.Platform.RestartProxy(ctx); err != nil {
		return fmt.Errorf("restart proxy service: %w", err)
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
		"%w: proxy registered=%t running=%t healthy=%t; refresh registered=%t healthy=%t",
		ErrServiceUnhealthy,
		status.Proxy.Registered,
		status.Proxy.Running,
		status.Proxy.Healthy,
		status.Refresh.Registered,
		status.Refresh.Healthy,
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
	return status.Proxy.Registered &&
		status.Proxy.Running &&
		status.Proxy.Healthy &&
		status.Proxy.ConfiguredExecutable == executable &&
		status.Proxy.LiveExecutable == executable &&
		status.Refresh.Registered &&
		status.Refresh.Healthy &&
		status.Refresh.ConfiguredExecutable == executable
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
