package main

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"time"

	"github.com/jacobcxdev/cq/internal/installstate"
)

const serviceStatusSchemaVersion = 1

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
	InstallProxy(context.Context, string) error
	InstallRefresh(context.Context, string) error
	RestartProxy(context.Context) error
	RestartRefresh(context.Context) error
	RemoveProxy(context.Context) error
	RemoveRefresh(context.Context) error
	Inspect(context.Context) (serviceStatus, error)
}

type serviceStateStore interface {
	Load() (installstate.Record, error)
	Save(installstate.Record) error
	Remove() error
	CheckClaim(installstate.Owner, string) error
}

type serviceLifecycle struct {
	Platform       servicePlatform
	Store          serviceStateStore
	Executable     string
	Version        string
	StatusAttempts int
	StatusInterval time.Duration
	Wait           func(context.Context, time.Duration) error
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
	before, err := lifecycle.Platform.Inspect(ctx)
	if err != nil {
		return fmt.Errorf("inspect services before install: %w", err)
	}
	if err := lifecycle.Platform.InstallProxy(ctx, lifecycle.Executable); err != nil {
		return fmt.Errorf("install proxy service: %w", err)
	}
	if err := lifecycle.Platform.InstallRefresh(ctx, lifecycle.Executable); err != nil {
		return lifecycle.rollbackNew(ctx, before, fmt.Errorf("install refresh service: %w", err))
	}
	status, err := lifecycle.waitHealthy(ctx)
	if err != nil {
		return lifecycle.rollbackNew(ctx, before, err)
	}
	record := installstate.Record{
		SchemaVersion: installstate.CurrentSchemaVersion,
		Owner:         owner,
		Version:       lifecycle.Version,
		Executable:    lifecycle.Executable,
		Services:      []string{status.Proxy.ID, status.Refresh.ID},
	}
	if err := lifecycle.Store.Save(record); err != nil {
		return lifecycle.rollbackNew(ctx, before, fmt.Errorf("save service ownership: %w", err))
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
	if err == nil && record.Owner != owner {
		return fmt.Errorf(
			"%w: existing owner %q; requested owner %q",
			installstate.ErrOwnershipConflict,
			record.Owner,
			owner,
		)
	}
	if err != nil && !errors.Is(err, installstate.ErrNotInstalled) {
		return fmt.Errorf("load service ownership: %w", err)
	}

	removeErr := errors.Join(
		wrapServiceError("remove refresh service", lifecycle.Platform.RemoveRefresh(ctx)),
		wrapServiceError("remove proxy service", lifecycle.Platform.RemoveProxy(ctx)),
	)
	if removeErr != nil {
		return removeErr
	}
	status, err := lifecycle.Platform.Inspect(ctx)
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

func (lifecycle *serviceLifecycle) rollbackNew(ctx context.Context, before serviceStatus, cause error) error {
	var rollbackErr error
	if !before.Refresh.Registered {
		rollbackErr = errors.Join(rollbackErr, wrapServiceError("roll back refresh service", lifecycle.Platform.RemoveRefresh(ctx)))
	}
	if !before.Proxy.Registered {
		rollbackErr = errors.Join(rollbackErr, wrapServiceError("roll back proxy service", lifecycle.Platform.RemoveProxy(ctx)))
	}
	return errors.Join(cause, rollbackErr)
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
