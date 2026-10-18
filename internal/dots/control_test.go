package dots

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func controllerFixture(t *testing.T) (Controller, *Settings) {
	t.Helper()
	options, s := preparedInstallationFixture(t)
	if err := PrivateDirectory(s.CQAccounts); err != nil {
		t.Fatal(err)
	}
	for _, owner := range []string{s.SystemAccountID, "fixture-secondary"} {
		name := "Pingy"
		if owner != s.SystemAccountID {
			name = "null"
		}
		handle := StableHandle(owner, "profile-"+owner)
		s.Dots[handle] = Dot{OwnerAlias: AccountAlias(owner), OwnerAccountID: owner, ProfileID: "profile-" + owner, RootThreadID: "root-" + owner, DisplayName: name}
	}
	if err := SaveJSON(filepath.Join(options.Root, "settings.json"), s); err != nil {
		t.Fatal(err)
	}
	return Controller{Root: options.Root, Executable: options.Executable}, s
}

func controllerReadyStatus() map[string]any {
	return map[string]any{"profiles": []any{"profile-fixture-secondary", "profile-fixture-owner"}, "primary": "profile-fixture-owner", "available": true, "workspaceError": false}
}

func TestControllerStatusVerifiedAllConfiguredProfilesAndWorkspace(t *testing.T) {
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("native process enumeration")
	}
	c, s := controllerFixture(t)
	calls := 0
	c.Native = nativeEvaluationFunc(func(_ context.Context, expression string) ([]any, error) {
		calls++
		for _, required := range []string{s.ModuleAsset, "safeGet('/tbo')", "safeGet('/tbo/primary')", "load workspace settings"} {
			if !strings.Contains(expression, required) {
				t.Fatalf("status omitted %q", required)
			}
		}
		if strings.Contains(expression, "safePost") || strings.Contains(expression, "safePatch") {
			t.Fatal("status performed native writes")
		}
		return []any{nil, map[string]any{"profiles": []any{}}, controllerReadyStatus()}, nil
	})
	value, err := c.Execute(context.Background(), Request{Operation: "status"})
	if err != nil || value["native_catalog_ready"] != true || value["profiles"] != 2 || value["controller_runtime"] != "go" || value["system_app_modified"] != false || calls != 1 {
		t.Fatalf("status %#v, calls %d, error %v", value, calls, err)
	}
	roles, ok := value["research_components"].([]string)
	if !ok || len(roles) != 0 {
		t.Fatalf("synthetic installation matched unrelated runtime processes: %#v", value["research_components"])
	}
}

func TestControllerStatusRejectedIncompleteOrForeignNativeCatalogue(t *testing.T) {
	for _, variant := range []string{"missing profile", "foreign profile", "duplicate profile", "missing primary", "foreign primary", "workspace failure", "unknown workspace", "wrong profile shape", "isolated unavailable"} {
		t.Run(variant, func(t *testing.T) {
			c, s := controllerFixture(t)
			value := controllerReadyStatus()
			switch variant {
			case "missing profile":
				value["profiles"] = []any{"profile-fixture-owner"}
			case "foreign profile":
				value["profiles"] = []any{"profile-fixture-owner", "foreign"}
			case "duplicate profile":
				value["profiles"] = []any{"profile-fixture-owner", "profile-fixture-owner"}
			case "missing primary":
				delete(value, "primary")
			case "foreign primary":
				value["primary"] = "foreign"
			case "workspace failure":
				value["workspaceError"] = true
			case "unknown workspace":
				delete(value, "workspaceError")
			case "wrong profile shape":
				value["profiles"] = []string{"profile-fixture-owner", "profile-fixture-secondary"}
			case "isolated unavailable":
				s.IntegrationMode = "isolated"
				if err := SaveJSON(filepath.Join(c.Root, "settings.json"), s); err != nil {
					t.Fatal(err)
				}
				value["available"] = false
			}
			c.Native = nativeEvaluationFunc(func(context.Context, string) ([]any, error) { return []any{value}, nil })
			if _, err := c.Status(context.Background()); err == nil || !strings.Contains(err.Error(), "not ready") {
				t.Fatalf("invalid status accepted: %v", err)
			}
		})
	}
}

func TestControllerStatusReportedOnboardingAndSanitisedNativeFailure(t *testing.T) {
	c, _ := controllerFixture(t)
	c.Native = nativeEvaluationFunc(func(context.Context, string) ([]any, error) {
		return []any{map[string]any{"native_catalog_ready": false, "onboarding_required": true}}, nil
	})
	value, err := c.Status(context.Background())
	if err != nil || value["onboarding_required"] != true || value["native_catalog_ready"] != false {
		t.Fatalf("onboarding %#v, %v", value, err)
	}
	c.Native = nativeEvaluationFunc(func(context.Context, string) ([]any, error) {
		return nil, errors.New("private native transport details")
	})
	if _, err := c.Status(context.Background()); err == nil || err.Error() != "Native Dot status unavailable" {
		t.Fatalf("native failure exposed: %v", err)
	}
}

func TestControllerRefreshRetainedUnavailableOwnerThenPropagatedRename(t *testing.T) {
	c, s := controllerFixture(t)
	now := time.Unix(1_800_000_000, 0)
	catalogueTestWrite(t, s.SystemAuth, catalogueTestDocument(s.SystemAccountID, now.Unix()+3600))
	secondaryPath := filepath.Join(s.CQAccounts, "secondary.auth.json")
	catalogueTestWrite(t, secondaryPath, catalogueTestDocument("fixture-secondary", now.Unix()+3600))
	systemBefore, _ := os.ReadFile(s.SystemAuth)
	secondaryBefore, _ := os.ReadFile(secondaryPath)
	secondaryUnavailable, secondaryAbsent, renamed := true, false, false
	c.Catalogue = &CatalogueClient{Settings: s, Now: func() time.Time { return now }, Origin: "https://synthetic.invalid", HTTP: catalogueTestDoer(func(request *http.Request) (*http.Response, error) {
		if request.Method != http.MethodGet || request.URL.Path != "/backend-api/tbo/primary" || request.URL.Host != "synthetic.invalid" {
			t.Fatalf("unexpected discovery request %s %s", request.Method, request.URL)
		}
		owner := request.Header.Get("ChatGPT-Account-Id")
		if owner != s.SystemAccountID && owner != "fixture-secondary" || !strings.HasPrefix(request.Header.Get("Authorization"), "Bearer synthetic.") {
			t.Fatal("discovery lost owner authentication")
		}
		if owner == "fixture-secondary" {
			if secondaryUnavailable {
				return catalogueTestResponse(http.StatusServiceUnavailable, map[string]any{"error": "unavailable"}), nil
			}
			if secondaryAbsent {
				return catalogueTestResponse(http.StatusOK, map[string]any{"selection": map[string]any{"available": false}}), nil
			}
			name, root := "null", "root-fixture-secondary"
			if renamed {
				name, root = "Null", "new-root-fixture-secondary"
			}
			return catalogueTestResponse(http.StatusOK, catalogueTestPrimary(owner, name, root)), nil
		}
		return catalogueTestResponse(http.StatusOK, catalogueTestPrimary(owner, "Pingy", "root-"+owner)), nil
	})}
	handle := StableHandle("fixture-secondary", "profile-fixture-secondary")
	changed, outcomes, err := c.Refresh(context.Background())
	if err != nil || changed || len(outcomes) != 2 {
		t.Fatalf("unavailable refresh changed %v, outcomes %#v, error %v", changed, outcomes, err)
	}
	loaded, err := c.settings()
	if err != nil || loaded.Dots[handle] != s.Dots[handle] {
		t.Fatalf("unavailable owner lost known profile: %#v %v", loaded, err)
	}
	secondaryUnavailable, renamed = false, true
	changed, _, err = c.Refresh(context.Background())
	if err != nil || !changed {
		t.Fatalf("rename was not propagated: %v %v", changed, err)
	}
	loaded, err = c.settings()
	if err != nil || len(loaded.Dots) != 2 || loaded.Dots[handle].DisplayName != "Null" || loaded.Dots[handle].RootThreadID != "new-root-fixture-secondary" || loaded.Dots[handle].ProfileID != "profile-fixture-secondary" {
		t.Fatalf("stable handle or rename lost: %#v %v", loaded, err)
	}
	if changed, _, err = c.Refresh(context.Background()); err != nil || changed {
		t.Fatalf("unchanged refresh changed state: %v %v", changed, err)
	}
	secondaryAbsent = true
	if changed, _, err = c.Refresh(context.Background()); err != nil || !changed {
		t.Fatalf("authoritative removal ignored: %v %v", changed, err)
	}
	loaded, err = c.settings()
	if err != nil || len(loaded.Dots) != 1 {
		t.Fatalf("removed owner retained: %#v %v", loaded, err)
	}
	for path, before := range map[string][]byte{s.SystemAuth: systemBefore, secondaryPath: secondaryBefore} {
		after, err := os.ReadFile(path)
		if err != nil || !bytes.Equal(before, after) {
			t.Fatalf("discovery changed credentials: %v", err)
		}
	}
}

func TestControllerRefreshRetainedKnownOwnerWithExpiredLocalCredential(t *testing.T) {
	c, s := controllerFixture(t)
	now := time.Unix(1_800_000_000, 0)
	catalogueTestWrite(t, s.SystemAuth, catalogueTestDocument(s.SystemAccountID, now.Unix()+3600))
	catalogueTestWrite(t, filepath.Join(s.CQAccounts, "expired.auth.json"), catalogueTestDocument("fixture-secondary", 0))
	c.Catalogue = &CatalogueClient{Settings: s, Now: func() time.Time { return now }, HTTP: catalogueTestDoer(func(request *http.Request) (*http.Response, error) {
		if request.Header.Get("ChatGPT-Account-Id") != s.SystemAccountID {
			t.Fatal("expired owner sent an upstream request")
		}
		return catalogueTestResponse(http.StatusOK, catalogueTestPrimary(s.SystemAccountID, "Pingy", "root-"+s.SystemAccountID)), nil
	})}
	changed, outcomes, err := c.Refresh(context.Background())
	loaded, loadErr := c.settings()
	if err != nil || loadErr != nil || changed || len(outcomes) != 2 || len(loaded.Dots) != 2 {
		t.Fatalf("local unavailability lost stable profile: changed %v, outcomes %#v, error %v, load %v", changed, outcomes, err, loadErr)
	}
}
