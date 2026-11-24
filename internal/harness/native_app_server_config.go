package harness

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os/exec"
	"runtime"
	"strings"
	"time"
)

const windowsAppManagedConfigField = "computer_use.windows.always_allowed_app_ids"

// The Windows desktop app writes a field its bundled CLI does not recognize.
// Probe strict configuration first so no other unknown field is silently lost.
func nativeAppServerCommand(ctx context.Context, executable, cwd string) (*exec.Cmd, func() error, error) {
	allowAppManaged := false
	if runtime.GOOS == "windows" {
		var err error
		allowAppManaged, err = nativeAppServerConfigMode(ctx, executable, cwd)
		if err != nil {
			return nil, nil, err
		}
	}
	args := []string{"app-server", "--stdio"}
	if !allowAppManaged {
		args = append(args, "--strict-config")
	}
	command := exec.CommandContext(ctx, executable, args...)
	command.Dir = cwd
	command.Stderr = io.Discard
	if !allowAppManaged {
		return command, func() error { return nil }, nil
	}
	// Select structured diagnostics on this child so the validation below does
	// not depend on the parent's logging format or verbosity.
	command.Env = append(command.Environ(), "LOG_FORMAT=json", "RUST_LOG=warn")
	warnings := &nativeConfigOutput{}
	command.Stderr = warnings
	return command, warnings.validateAppManagedWarning, nil
}

func nativeAppServerConfigMode(ctx context.Context, executable, cwd string) (bool, error) {
	command := exec.CommandContext(ctx, executable, "app-server", "--print-startup-config", "--strict-config")
	command.Dir, command.Stderr, command.WaitDelay = cwd, io.Discard, time.Second
	output := &nativeConfigOutput{}
	command.Stdout = output
	err := command.Run()
	if output.exceeded {
		return false, fmt.Errorf("native app-server startup response exceeds limit")
	}
	if err == nil {
		return false, nil
	}
	var response struct {
		Error *struct {
			Data struct {
				Reason string `json:"reason"`
				Detail string `json:"detail"`
			} `json:"data"`
		} `json:"error"`
	}
	if json.Unmarshal(output.Bytes(), &response) == nil && response.Error != nil &&
		response.Error.Data.Reason == "configLoad" &&
		strings.HasSuffix(response.Error.Data.Detail, "unknown configuration field `"+windowsAppManagedConfigField+"`") {
		return true, nil
	}
	return false, fmt.Errorf("native app-server strict configuration unavailable")
}

type nativeConfigOutput struct {
	data     bytes.Buffer
	exceeded bool
}

func (output *nativeConfigOutput) Bytes() []byte { return output.data.Bytes() }

func (output *nativeConfigOutput) Write(data []byte) (int, error) {
	if output.data.Len()+len(data) > 64<<10 {
		output.exceeded = true
		return 0, fmt.Errorf("native app-server configuration diagnostic exceeds limit")
	}
	return output.data.Write(data)
}

func (output *nativeConfigOutput) validateAppManagedWarning() error {
	if output.exceeded {
		return fmt.Errorf("native app-server configuration diagnostic exceeds limit")
	}
	const expected = "Codex is ignoring 1 unrecognized configuration setting."
	seen := false
	for _, line := range bytes.Split(output.Bytes(), []byte{'\n'}) {
		if bytes.Contains(line, []byte("Invalid configuration; using defaults.")) {
			return fmt.Errorf("native app-server fell back to default configuration")
		}
		if !bytes.Contains(line, []byte("unrecognized configuration setting")) {
			continue
		}
		var entry struct {
			Fields struct {
				Message string `json:"message"`
			} `json:"fields"`
		}
		if json.Unmarshal(line, &entry) != nil {
			return fmt.Errorf("native app-server configuration warning is not structured JSON")
		}
		if seen {
			return fmt.Errorf("native app-server emitted multiple configuration warnings")
		}
		if !strings.HasPrefix(entry.Fields.Message, expected) {
			return fmt.Errorf("native app-server configuration warning header changed")
		}
		if strings.Count(entry.Fields.Message, " is ignored.") != 1 {
			return fmt.Errorf("native app-server ignored multiple configuration fields")
		}
		if !strings.Contains(entry.Fields.Message, "`"+windowsAppManagedConfigField+"` is ignored.") {
			return fmt.Errorf("native app-server ignored unrelated configuration")
		}
		seen = true
	}
	if !seen {
		return fmt.Errorf("native app-server Windows configuration warning unavailable")
	}
	return nil
}
