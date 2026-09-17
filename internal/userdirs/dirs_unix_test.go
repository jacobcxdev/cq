//go:build unix

package userdirs

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestUserDirsUnixRoots(t *testing.T) {
	for _, xdg := range []bool{false, true} {
		env := map[string]string{"XDG_STATE_HOME": "/ignored-state", "XDG_RUNTIME_DIR": "/ignored-runtime", "TMPDIR": "/ignored-temp", "CQ_HOME": "/ignored"}
		c, k := "/home/alice/.config/cq", "/home/alice/.cache/cq"
		if runtime.GOOS == "darwin" {
			k = "/home/alice/Library/Caches/cq"
		}
		if xdg {
			env["XDG_CONFIG_HOME"] = "/space dir/config/../config"
			env["XDG_CACHE_HOME"] = "/cache"
			c = "/space dir/config/cq"
			k = "/cache/cq"
		}
		r := Resolver{Getenv: func(key string) string { return env[key] }, UserHomeDir: func() (string, error) { return "/home/alice", nil }}
		got, err := r.Resolve()
		want := Roots{Config: c, State: c + "/state", Cache: k, Runtime: c + "/state", Logs: unixLogs("/home/alice", c)}
		if err != nil || got != want {
			t.Fatalf("roots %#v, %v; want %#v", got, err, want)
		}
	}
}
func TestCLIV2EnvironmentRejectsRelativeXDG(t *testing.T) {
	for _, name := range []string{"XDG_CONFIG_HOME", "XDG_CACHE_HOME"} {
		for _, value := range []string{"relative-private-path", " ", "~/secret", "$HOME/private", "/private\x00secret"} {
			_, err := (Resolver{Getenv: func(key string) string {
				if key == name {
					return value
				}
				return ""
			}, UserHomeDir: func() (string, error) { return "/home/test", nil }}).Resolve()
			var e *EnvironmentError
			if !errors.As(err, &e) || e.Code != "environment_path_invalid" || e.ExitCode != 2 || (len(value) > 2 && strings.Contains(e.Error(), value)) {
				t.Fatalf("unsafe/wrong diagnostic: %v", err)
			}
		}
	}
}
func TestCLIV2EnvironmentNoTemporaryCache(t *testing.T) {
	_, err := (Resolver{UserHomeDir: func() (string, error) { return "", os.ErrNotExist }}).Resolve(CacheRoot)
	var e *EnvironmentError
	if !errors.As(err, &e) || e.Code != "environment_root_unavailable" || e.ExitCode != 4 {
		t.Fatalf("error = %v", err)
	}
}
func TestUserDirsSelectedRootsAreLazy(t *testing.T) {
	for _, root := range []Root{ConfigRoot, StateRoot, CacheRoot, RuntimeRoot, LogsRoot} {
		t.Run(map[Root]string{ConfigRoot: "config", StateRoot: "state", CacheRoot: "cache", RuntimeRoot: "runtime", LogsRoot: "logs"}[root], func(t *testing.T) {
			homeCalls := 0
			names := []string{}
			r := Resolver{Getenv: func(name string) string { names = append(names, name); return "/isolated" }, UserHomeDir: func() (string, error) { homeCalls++; return "/home/test", nil }}
			got, err := r.Resolve(root)
			if err != nil {
				t.Fatal(err)
			}
			if root == LogsRoot && runtime.GOOS == "darwin" {
				if homeCalls != 1 || len(names) != 0 || got.Logs != "/home/test/Library/Logs/cq" {
					t.Fatalf("macOS logs %#v %v %d", got, names, homeCalls)
				}
			} else {
				wantName := "XDG_CONFIG_HOME"
				if root == CacheRoot {
					wantName = "XDG_CACHE_HOME"
				}
				if homeCalls != 0 || len(names) != 1 || names[0] != wantName {
					t.Fatalf("unrelated inputs: %v home=%d", names, homeCalls)
				}
			}
			for field, path := range map[Root]string{ConfigRoot: got.Config, StateRoot: got.State, CacheRoot: got.Cache, RuntimeRoot: got.Runtime, LogsRoot: got.Logs} {
				if (field == root) != (path != "") {
					t.Fatalf("unselected root returned: %#v", got)
				}
			}
		})
	}
}
func TestUserDirsHomeBoundary(t *testing.T) {
	for _, value := range []string{"", "relative"} {
		t.Setenv("HOME", value)
		if _, err := UserHomeDir(); err == nil {
			t.Fatalf("accepted invalid home")
		}
	}
	t.Setenv("HOME", "/space home/../space home")
	got, err := UserHomeDir()
	if err != nil || got != "/space home" {
		t.Fatalf("%q %v", got, err)
	}
}
func TestUserDirsResolutionCreatesNoDirectories(t *testing.T) {
	base := filepath.Join(t.TempDir(), "absent")
	_, err := (Resolver{Getenv: func(string) string { return base }, UserHomeDir: func() (string, error) { return base, nil }}).Resolve()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(base); !os.IsNotExist(err) {
		t.Fatalf("resolution wrote directories: %v", err)
	}
}

func TestUserDirsRejectInvalidInjectedHome(t *testing.T) {
	for _, home := range []string{"relative", "/private\x00path"} {
		_, err := (Resolver{UserHomeDir: func() (string, error) { return home, nil }}).Resolve(CacheRoot)
		var diagnostic *EnvironmentError
		if !errors.As(err, &diagnostic) || diagnostic.ExitCode != 2 || diagnostic.Code != "environment_path_invalid" {
			t.Fatalf("invalid home result %v", err)
		}
	}
}
