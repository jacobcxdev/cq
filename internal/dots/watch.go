package dots

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/jacobcxdev/cq/internal/fsutil"
)

const accountWatchInterval = 5 * time.Second

func accountWatchMetadata(path string) (*fileRevision, error) {
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	value := revision(info)
	return &value, nil
}

// accountWatchFingerprint inspects metadata only; credential contents stay unread.
func accountWatchFingerprint(settings *Settings) (string, error) {
	accounts, err := accountWatchMetadata(settings.CQAccounts)
	if err != nil {
		return "", err
	}
	entries := []any{}
	if accounts != nil && accounts.Mode.IsDir() && accounts.Mode&os.ModeSymlink == 0 {
		children, err := os.ReadDir(settings.CQAccounts)
		if err != nil {
			return "", err
		}
		for _, child := range children {
			metadata, err := accountWatchMetadata(filepath.Join(settings.CQAccounts, child.Name()))
			if err != nil {
				return "", err
			}
			entries = append(entries, []any{child.Name(), metadata})
		}
	}
	var manifest *fileRevision
	if settings.AccountSource != "" {
		manifest, err = accountWatchMetadata(filepath.Join(settings.AccountSource, "managed-codex-accounts.json"))
		if err != nil {
			return "", err
		}
	}
	system, err := accountWatchMetadata(settings.SystemAuth)
	if err != nil {
		return "", err
	}
	configuration, err := accountWatchMetadata(filepath.Join(filepath.Dir(settings.RuntimeRoot), "settings.json"))
	if err != nil {
		return "", err
	}
	data, err := json.Marshal([]any{accounts, entries, manifest, system, configuration})
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:]), nil
}

func accountWatchOnce(ctx context.Context, settings *Settings, previous string, execute func(context.Context, Request) (map[string]any, error)) (string, string) {
	current, err := accountWatchFingerprint(settings)
	if err != nil {
		return previous, "account_sync_failed"
	}
	if current == previous {
		return previous, ""
	}
	ctx, cancel := context.WithTimeout(ctx, 120*time.Second)
	defer cancel()
	outcome, err := execute(ctx, Request{Operation: "sync", Watcher: true})
	_, completeOK := outcome["discovery_complete"].(bool)
	retryable, retryableOK := outcome["discovery_retryable"].(bool)
	if err != nil || !completeOK || !retryableOK || retryable {
		return previous, "account_sync_failed"
	}
	return current, "account_sync_complete"
}

func accountWatchDelay(delay time.Duration, event string) time.Duration {
	if event == "account_sync_failed" {
		return min(delay*2, time.Minute)
	}
	return accountWatchInterval
}

// RunAccountWatch reconciles account changes through the controller's operation
// lock. Sync reports restart-required instead of interrupting the native app.
func RunAccountWatch(ctx context.Context, settings *Settings, controller Controller) error {
	if err := ValidateRuntimePaths(settings.RuntimeRoot); err != nil {
		return err
	}
	lock, err := fsutil.AcquireExclusiveLock(fsutil.OSFileSystem{}, filepath.Join(settings.RuntimeRoot, "account-watch.lock"))
	if err != nil {
		return err
	}
	defer lock.Close()
	previous := ""
	delay := accountWatchInterval
	output := controller.Stdout
	if output == nil {
		output = io.Discard
	}
	encoder := json.NewEncoder(output)
	for {
		if err := waitContext(ctx, delay); err != nil {
			return err
		}
		event := "account_sync_failed"
		fresh, err := controller.settings()
		if err == nil && fresh.RuntimeRoot == settings.RuntimeRoot {
			settings = fresh
			previous, event = accountWatchOnce(ctx, settings, previous, controller.Execute)
		}
		if event != "" {
			if err := encoder.Encode(map[string]string{"event": event}); err != nil {
				return err
			}
		}
		delay = accountWatchDelay(delay, event)
	}
}

func (c Controller) ensureWatcher(ctx context.Context, settings *Settings) (map[string]any, error) {
	if err := ValidateRuntimePaths(settings.RuntimeRoot); err != nil {
		return nil, err
	}
	lock, err := fsutil.AcquireExclusiveLock(fsutil.OSFileSystem{}, filepath.Join(settings.RuntimeRoot, "account-watch-start.lock"))
	if err != nil {
		return nil, err
	}
	defer lock.Close()
	count := func() (int, error) {
		rows, err := RuntimeProcesses(ctx, settings)
		if err != nil {
			return 0, err
		}
		watchers := 0
		for _, row := range rows {
			if row.Role == "account-watch" {
				watchers++
			}
		}
		if watchers > 1 {
			return 0, fmt.Errorf("Multiple verified account watchers are running")
		}
		return watchers, nil
	}
	watchers, err := count()
	if err != nil {
		return nil, err
	}
	if watchers == 0 {
		if _, err := c.spawn(settings, "account-watch"); err != nil {
			return nil, err
		}
		if err := waitContext(ctx, 500*time.Millisecond); err != nil {
			return nil, err
		}
		started, err := count()
		if err != nil {
			return nil, err
		}
		if started != 1 {
			return nil, fmt.Errorf("Account watcher failed to start")
		}
	}
	return map[string]any{"account_watcher_started": true, "account_watcher_existing": watchers != 0}, nil
}
