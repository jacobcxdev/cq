//go:build windows

package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/jacobcxdev/cq/internal/harness"
	"golang.org/x/sys/windows"
)

func TestHarnessSkillInstallUnderWindowsCodexHome(t *testing.T) {
	profile := t.TempDir()
	t.Setenv("USERPROFILE", profile)
	t.Setenv("CODEX_HOME", "")
	home := filepath.Join(profile, ".codex")
	skills := filepath.Join(home, "skills")
	if err := os.Mkdir(home, 0o700); err != nil {
		t.Fatal(err)
	}
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	sid := user.User.Sid.String()
	descriptor, err := windows.SecurityDescriptorFromString(fmt.Sprintf("O:%sD:P(A;;FA;;;%s)(A;;FA;;;SY)(A;;FA;;;BA)(A;;GRGX;;;WD)", sid, sid))
	if err != nil {
		t.Fatal(err)
	}
	owner, _, err := descriptor.Owner()
	if err != nil {
		t.Fatal(err)
	}
	dacl, _, err := descriptor.DACL()
	if err != nil {
		t.Fatal(err)
	}
	if err := windows.SetNamedSecurityInfo(home, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, owner, nil, dacl, nil); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(skills, "curate-harness")
	cmd := CodexHarnessCmd{}
	for attempt, wantChanged := range []bool{true, false} {
		var output bytes.Buffer
		if err := runHarnessSkillCommand("skill install", cmd, true, &output); err != nil {
			t.Fatalf("install %d: %v", attempt+1, err)
		}
		var result harness.SkillInstallation
		if err := json.Unmarshal(output.Bytes(), &result); err != nil {
			t.Fatal(err)
		}
		if result.Path != filepath.Join(target, "SKILL.md") || result.Changed != wantChanged {
			t.Fatalf("install %d = %#v", attempt+1, result)
		}
	}
	if info, err := os.Lstat(filepath.Join(target, "SKILL.md")); err != nil || !info.Mode().IsRegular() {
		t.Fatalf("skill asset = %v, %v", info, err)
	}
}
