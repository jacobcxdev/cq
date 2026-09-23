package main

import (
	"encoding/json"
	"errors"
	"io"
	"path/filepath"
	"strings"
	"time"

	"github.com/jacobcxdev/cq/internal/fsutil"
	"github.com/jacobcxdev/cq/internal/userdirs"
)

const serviceRefreshCompletionName = "refresh-completion.json"

type serviceRefreshCompletion struct {
	SchemaVersion int            `json:"schema_version"`
	StartedAt     time.Time      `json:"started_at"`
	CompletedAt   time.Time      `json:"completed_at"`
	ExitCode      int            `json:"exit_code"`
	Executable    string         `json:"executable"`
	Roots         userdirs.Roots `json:"roots"`
}

func recordServiceRefresh(executable string, roots userdirs.Roots, run func() error, now func() time.Time) error {
	// Open the authority before execution and retain it through receipt publication.
	fs := fsutil.OSFileSystem{}
	if err := fsutil.EnsureSecureDirectory(fs, roots.State); err != nil {
		return err
	}
	directory, err := fs.OpenSecureDirectory(roots.State)
	if err != nil {
		return err
	}
	defer directory.Close()
	started := now().UTC()
	runErr := run()
	completed := now().UTC()
	exit := 0
	if runErr != nil {
		exit = 1
	}
	receipt := serviceRefreshCompletion{SchemaVersion: 1, StartedAt: started, CompletedAt: completed, ExitCode: exit, Executable: executable, Roots: roots}
	data, err := json.Marshal(receipt)
	if err != nil {
		return errors.Join(runErr, err)
	}
	if err := fsutil.ValidateSecureDirectoryHandle(fs, directory, roots.State); err != nil {
		return errors.Join(runErr, err)
	}
	err = fsutil.SecureAtomicWriteInDirectoryChecked(fs, directory, serviceRefreshCompletionName, append(data, '\n'), func() error { return fsutil.ValidateSecureDirectoryHandle(fs, directory, roots.State) })
	return errors.Join(runErr, err)
}
func readServiceRefreshCompletion(executable string, roots userdirs.Roots, now time.Time) (serviceRefreshCompletion, error) {
	var receipt serviceRefreshCompletion
	fs := fsutil.OSFileSystem{}
	data, err := fsutil.ReadSecureFile(fs, filepath.Join(roots.State, serviceRefreshCompletionName), 4096)
	if err != nil {
		return receipt, err
	}
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&receipt); err != nil {
		return receipt, errServiceUnavailable
	}
	// Exact canonical encoding rejects nulls/omissions, case aliases and unknown or
	// duplicate fields, including nested root fields. Whitespace remains harmless.
	raw, err := serviceRefreshObject(data)
	if err != nil || len(raw) != 6 {
		return receipt, errServiceUnavailable
	}
	for _, key := range []string{"schema_version", "started_at", "completed_at", "exit_code", "executable", "roots"} {
		value, ok := raw[key]
		if !ok || strings.TrimSpace(string(value)) == "null" {
			return receipt, errServiceUnavailable
		}
	}
	rootFields, err := serviceRefreshObject(raw["roots"])
	if err != nil || len(rootFields) != 5 {
		return receipt, errServiceUnavailable
	}
	for _, key := range []string{"Config", "State", "Cache", "Runtime", "Logs"} {
		value, ok := rootFields[key]
		if !ok || strings.TrimSpace(string(value)) == "null" {
			return receipt, errServiceUnavailable
		}
	}
	if receipt.SchemaVersion != 1 || receipt.StartedAt.IsZero() || receipt.CompletedAt.IsZero() || receipt.CompletedAt.Before(receipt.StartedAt) || receipt.CompletedAt.After(now) || receipt.ExitCode < 0 || receipt.Executable != executable || receipt.Roots != roots {
		return receipt, errServiceUnavailable
	}
	for _, key := range []string{"started_at", "completed_at"} {
		var value string
		_ = json.Unmarshal(raw[key], &value)
		if !strings.HasSuffix(value, "Z") {
			return receipt, errServiceUnavailable
		}
	}
	return receipt, nil
}

// Receipts contain exactly two objects. Decode each separately so repeated keys
// cannot disappear into a map before the fixed schema is checked.
func serviceRefreshObject(data []byte) (map[string]json.RawMessage, error) {
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	opening, err := decoder.Token()
	if err != nil || opening != json.Delim('{') {
		return nil, errServiceUnavailable
	}
	fields := make(map[string]json.RawMessage)
	for decoder.More() {
		token, err := decoder.Token()
		key, ok := token.(string)
		if err != nil || !ok {
			return nil, errServiceUnavailable
		}
		if _, duplicate := fields[key]; duplicate {
			return nil, errServiceUnavailable
		}
		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			return nil, errServiceUnavailable
		}
		fields[key] = value
	}
	if closing, err := decoder.Token(); err != nil || closing != json.Delim('}') {
		return nil, errServiceUnavailable
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return nil, errServiceUnavailable
	}
	return fields, nil
}
