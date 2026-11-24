package proxy

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/windows"
)

func protectProxyConfigTestRoot(t *testing.T, root string) {
	t.Helper()
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	sid := user.User.Sid.String()
	descriptor, err := windows.SecurityDescriptorFromString(fmt.Sprintf("D:P(A;OICI;FA;;;%s)(A;OICI;FA;;;SY)(A;OICI;FA;;;BA)", sid))
	if err != nil {
		t.Fatal(err)
	}
	dacl, _, err := descriptor.DACL()
	if err != nil {
		t.Fatal(err)
	}
	flags := windows.SECURITY_INFORMATION(windows.OWNER_SECURITY_INFORMATION | windows.DACL_SECURITY_INFORMATION | windows.PROTECTED_DACL_SECURITY_INFORMATION)
	if err := windows.SetNamedSecurityInfo(root, windows.SE_FILE_OBJECT, flags, user.User.Sid, nil, dacl, nil); err != nil {
		t.Fatal(err)
	}
}

func TestConfigRejectsWindowsVolumeRootStateDirectories(t *testing.T) {
	volumeRoot := filepath.VolumeName(os.TempDir()) + string(filepath.Separator)
	if filepath.VolumeName(volumeRoot) == "" {
		t.Fatalf("temporary directory has no Windows volume: %q", os.TempDir())
	}

	tests := map[string]func(*Config){
		"continuity": func(cfg *Config) { cfg.CodexContinuityStateDir = volumeRoot },
		"resilience": func(cfg *Config) { cfg.ProxyResilienceStateDir = volumeRoot },
	}
	for name, configure := range tests {
		t.Run(name, func(t *testing.T) {
			cfg := Config{
				LocalToken:              "token",
				ClaudeUpstream:          DefaultUpstream,
				CodexUpstream:           DefaultCodexUpstream,
				CodexLeaseRetentionDays: 7,
				CodexTurnRouting:        CodexRoutingOff,
				CodexWSTurnRouting:      CodexRoutingOff,
			}
			configure(&cfg)
			if err := cfg.validate(); err == nil || !strings.Contains(err.Error(), "must be a clean absolute non-root path") {
				t.Fatalf("validate accepted Windows volume root %q", volumeRoot)
			}
		})
	}
}
