package userdirs

import (
	"errors"
	"fmt"
	"os"
)

type Roots struct {
	Config  string
	State   string
	Cache   string
	Runtime string
	Logs    string
}

type Resolver struct {
	Getenv         func(string) string
	UserHomeDir    func() (string, error)
	RoamingAppData func() (string, error)
	LocalAppData   func() (string, error)
}

// Root selects the storage needed by an operation. Omitted selectors resolve all roots.
type Root uint8

const (
	ConfigRoot Root = iota
	StateRoot
	CacheRoot
	RuntimeRoot
	LogsRoot
)

func selected(roots []Root, root Root) bool {
	if len(roots) == 0 {
		return true
	}
	for _, r := range roots {
		if r == root {
			return true
		}
	}
	return false
}

// EnvironmentError carries the public diagnostic without disclosing path values.
type EnvironmentError struct {
	Code     string
	ExitCode int
	message  string
}

func (e *EnvironmentError) Error() string { return e.message }
func invalidPath(name string) error {
	return &EnvironmentError{Code: "environment_path_invalid", ExitCode: 2, message: fmt.Sprintf("Environment variable %s must name an absolute path.", name)}
}
func rootUnavailable() error {
	return &EnvironmentError{Code: "environment_root_unavailable", ExitCode: 4, message: "Cannot resolve the user storage root."}
}

// WorkingDirectory captures the path base without exposing an OS error's path.
func WorkingDirectory() (string, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return "", workingDirectoryUnavailable()
	}
	return cwd, nil
}
func workingDirectoryUnavailable() error {
	return &EnvironmentError{Code: "environment_path_invalid", ExitCode: 2, message: "Cannot resolve the working directory."}
}
func safeHomeError(err error) error {
	var diagnostic *EnvironmentError
	if errors.As(err, &diagnostic) {
		return diagnostic
	}
	return rootUnavailable()
}
