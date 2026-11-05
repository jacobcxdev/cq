package harness

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os/exec"
	"sort"
	"time"
)

// PluginCLI uses the native client's installed inventory, including project
// marketplaces. Cache directory names are not authoritative plugin identities.
type PluginCLI struct {
	Executable string
	Run        func(context.Context, string, string, io.Writer) error
}

func (client PluginCLI) Installed(ctx context.Context, cwd string) ([]string, error) {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	run := client.Run
	if run == nil {
		run = runNativePluginInventory
	}
	output := &pluginOutput{}
	if err := run(ctx, client.Executable, cwd, output); err != nil {
		if ctx.Err() != nil {
			return nil, fmt.Errorf("native plugin inventory unavailable: %w", ctx.Err())
		}
		return nil, fmt.Errorf("native plugin inventory unavailable")
	}
	type installedPlugin struct {
		ID        string `json:"id"`
		Installed *bool  `json:"installed"`
		Enabled   *bool  `json:"enabled"`
	}
	var response struct {
		Marketplaces *[]struct {
			Plugins *[]installedPlugin `json:"plugins"`
		} `json:"marketplaces"`
		MarketplaceLoadErrors *[]json.RawMessage `json:"marketplaceLoadErrors"`
	}
	if output.exceeded || json.Unmarshal(output.Bytes(), &response) != nil || response.Marketplaces == nil || response.MarketplaceLoadErrors == nil || len(*response.MarketplaceLoadErrors) != 0 {
		return nil, fmt.Errorf("invalid native plugin inventory")
	}
	ids := make([]string, 0)
	seen := make(map[string]bool)
	for _, marketplace := range *response.Marketplaces {
		if marketplace.Plugins == nil {
			return nil, fmt.Errorf("invalid native marketplace plugin inventory")
		}
		for _, plugin := range *marketplace.Plugins {
			if plugin.Installed == nil || plugin.Enabled == nil {
				return nil, fmt.Errorf("native plugin eligibility unavailable")
			}
			if !*plugin.Installed || !*plugin.Enabled {
				continue
			}
			if err := validateDesktopPlugins([]string{plugin.ID}); err != nil {
				return nil, err
			}
			if !seen[plugin.ID] {
				ids = append(ids, plugin.ID)
				seen[plugin.ID] = true
			}
		}
	}
	sort.Strings(ids)
	return ids, nil
}

// runNativePluginInventory queries cached installed state without invoking the
// catalogue-facing plugin CLI command or creating any thread. Child output is
// never forwarded until the correlated response and clean teardown are proven.
func runNativePluginInventory(ctx context.Context, executable, cwd string, output io.Writer) error {
	command := exec.CommandContext(ctx, executable, "app-server", "--stdio", "--strict-config")
	command.Dir, command.Stderr, command.WaitDelay = cwd, io.Discard, time.Second
	stdin, err := command.StdinPipe()
	if err != nil {
		return err
	}
	defer stdin.Close()
	stdout, err := command.StdoutPipe()
	if err != nil {
		return err
	}
	if err := command.Start(); err != nil {
		return err
	}
	waited := false
	defer func() {
		if !waited {
			_ = command.Process.Kill()
			_ = command.Wait()
		}
	}()
	// A descendant can retain stdout after CommandContext kills its parent.
	// Close the manually-read pipes on cancellation instead of waiting for EOF.
	cancellationDone := make(chan struct{})
	stopCancellation := context.AfterFunc(ctx, func() {
		defer close(cancellationDone)
		defer func() { _ = recover() }()
		_ = stdin.Close()
		_ = stdout.Close()
	})
	defer func() {
		if !stopCancellation() {
			<-cancellationDone
		}
	}()
	encoder := json.NewEncoder(stdin)
	if err := encoder.Encode(map[string]any{"id": 1, "method": "initialize", "params": map[string]any{"clientInfo": map[string]any{"name": "cq_harness_plugin_inventory", "version": "0.0.0"}, "capabilities": map[string]any{"experimentalApi": true}}}); err != nil {
		return err
	}
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 64*1024), 1<<20)
	phase, messages, totalBytes := 1, 0, 0
	var inventory json.RawMessage
	for scanner.Scan() {
		messages++
		line := scanner.Bytes()
		totalBytes += len(line) + 1
		if messages > 128 || totalBytes > 4<<20 {
			return fmt.Errorf("native inventory protocol exceeds limit")
		}
		var message struct {
			ID     json.RawMessage `json:"id"`
			Result json.RawMessage `json:"result"`
			Error  json.RawMessage `json:"error"`
		}
		if json.Unmarshal(line, &message) != nil {
			return fmt.Errorf("invalid native inventory protocol JSON")
		}
		if phase == 3 || string(message.ID) != fmt.Sprint(phase) {
			continue
		}
		if len(message.Error) > 0 && !bytes.Equal(bytes.TrimSpace(message.Error), []byte("null")) {
			return fmt.Errorf("native inventory request rejected")
		}
		result := bytes.TrimSpace(message.Result)
		if len(result) == 0 || result[0] != '{' {
			return fmt.Errorf("invalid native inventory response")
		}
		if phase == 1 {
			if err := encoder.Encode(map[string]any{"method": "initialized", "params": map[string]any{}}); err != nil {
				return err
			}
			if err := encoder.Encode(map[string]any{"id": 2, "method": "plugin/installed", "params": map[string]any{"cwds": []string{cwd}}}); err != nil {
				return err
			}
			phase = 2
		} else {
			inventory = append(json.RawMessage(nil), message.Result...)
			phase = 3
			if err := stdin.Close(); err != nil {
				return err
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("incomplete native inventory protocol")
	}
	if phase != 3 {
		return fmt.Errorf("native inventory response missing")
	}
	err = command.Wait()
	waited = true
	if err != nil {
		return err
	}
	_, err = output.Write(inventory)
	return err
}

type pluginOutput struct {
	data     bytes.Buffer
	exceeded bool
}

func (output *pluginOutput) Bytes() []byte { return output.data.Bytes() }

func (output *pluginOutput) Write(data []byte) (int, error) {
	if output.data.Len()+len(data) > 1<<20 {
		output.exceeded = true
		return 0, fmt.Errorf("plugin inventory exceeds limit")
	}
	return output.data.Write(data)
}
