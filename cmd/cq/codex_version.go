package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/jacobcxdev/cq/internal/fsutil"
	"github.com/jacobcxdev/cq/internal/modelregistry"
)

// fallbackCodexClientVersion is the pinned value used when no other source
// yields a usable Codex client_version. Update when the Codex API starts
// rejecting it; verified to be accepted by /models.
const fallbackCodexClientVersion = "0.124.0"

// codexVersionResolver resolves the Codex client_version string used when
// querying the upstream /models endpoint. Dependencies are injected so the
// resolver can be tested without filesystem or subprocess side effects.
type codexVersionResolver struct {
	FS                fsutil.FileSystem
	CachePath         string
	SubprocessVersion func() (string, bool)
	Fallback          string
}

func (r codexVersionResolver) Resolve() string {
	if r.FS != nil && r.CachePath != "" {
		if v := modelregistry.DiscoverCodexClientVersion(r.FS, r.CachePath); v != "" {
			return v
		}
	}
	if r.SubprocessVersion != nil {
		if v, ok := r.SubprocessVersion(); ok {
			return v
		}
	}
	return r.Fallback
}

// defaultCodexClientVersion resolves the Codex client_version from OS
// resources: existing models_cache.json first (source of truth once Codex
// CLI has run), the installed codex binary's --version second, and a pinned
// fallback if both are unavailable.
func defaultCodexClientVersion() string {
	fsys := fsutil.OSFileSystem{}
	cachePath := ""
	if home, err := fsys.UserHomeDir(); err == nil {
		codexHome := os.Getenv("CODEX_HOME")
		if codexHome == "" {
			codexHome = filepath.Join(home, ".codex")
		}
		cachePath = filepath.Join(codexHome, "models_cache.json")
	}
	return codexVersionResolver{
		FS:                fsys,
		CachePath:         cachePath,
		SubprocessVersion: probeCodexBinaryVersion,
		Fallback:          fallbackCodexClientVersion,
	}.Resolve()
}

// probeCodexBinaryVersion runs `codex --version` with a short timeout and
// returns the parsed semver. Any failure path returns ("", false) so the
// resolver can continue to its next tier.
func probeCodexBinaryVersion() (string, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "codex", "--version").Output()
	if err != nil {
		return "", false
	}
	return parseCodexVersionOutput(string(out))
}

var codexSemverRe = regexp.MustCompile(`\d+\.\d+\.\d+(?:[-+][0-9A-Za-z.-]+)?`)

func parseCodexVersionOutput(s string) (string, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", false
	}
	if match := codexSemverRe.FindString(s); match != "" {
		return match, true
	}
	return "", false
}
