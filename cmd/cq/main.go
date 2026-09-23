package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"time"

	"github.com/jacobcxdev/cq/internal/cache"
	"github.com/jacobcxdev/cq/internal/cli"
	"github.com/jacobcxdev/cq/internal/provider"
)

// version is set at build time via -ldflags. Falls back to "dev".
var version = "dev"

// geminiOAuthClientSecret is injected into release builds via -ldflags.
// Development builds can use a still-valid Antigravity access token without it.
var geminiOAuthClientSecret string

func main() {
	if handled, exit := dispatchMachineABI(os.Args[1:]); handled {
		os.Exit(exit)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	session := &cli.Session{In: os.Stdin, Out: os.Stdout, Err: os.Stderr, Interactive: isStdinTerminal(), BuildInfo: cli.BuildInfo{Version: version}}
	os.Exit(runCLIV2(ctx, os.Args[1:], session))
}

// invalidateProviderCache removes the cached result file for a provider.
// Best-effort: errors are logged to stderr.
func invalidateProviderCache(id provider.ID) {
	dir, err := cache.DefaultDir()
	if err != nil {
		fmt.Fprintf(os.Stderr, "cq: cache invalidate %s: %v\n", id, err)
		return
	}
	path := filepath.Join(dir, string(id)+".json")
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		fmt.Fprintf(os.Stderr, "cq: cache invalidate %s: %v\n", id, err)
	}
}

type systemClock struct{}

func (systemClock) Now() time.Time { return time.Now() }

func isTerminal() bool {
	fi, err := os.Stdout.Stat()
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeCharDevice != 0
}

func cacheTTL() time.Duration {
	if v := os.Getenv("CQ_TTL"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			if n < 0 {
				n = 0
			}
			if n > 3600 {
				n = 3600
			}
			return time.Duration(n) * time.Second
		}
	}
	return 30 * time.Second
}
