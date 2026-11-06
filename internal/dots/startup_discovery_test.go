package dots

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

func startupDiscoveryFixture(t *testing.T) (Controller, *Settings) {
	t.Helper()
	c, s := controllerFixture(t)
	now := time.Unix(1_800_000_000, 0)
	catalogueTestWrite(t, s.SystemAuth, catalogueTestDocument(s.SystemAccountID, now.Unix()+3600))
	catalogueTestWrite(t, filepath.Join(s.CQAccounts, "secondary.auth.json"), catalogueTestDocument("fixture-secondary", now.Unix()+3600))
	c.Catalogue = &CatalogueClient{Settings: s, Now: func() time.Time { return now }}
	return c, s
}

func TestStartDiscoveredNewOwnerBeforeFreezingLaunch(t *testing.T) {
	c, s := controllerFixture(t)
	now := time.Unix(1_800_000_000, 0)
	for _, owner := range []string{s.SystemAccountID, "fixture-secondary", "new-owner"} {
		path := filepath.Join(s.CQAccounts, owner+".auth.json")
		if owner == s.SystemAccountID {
			path = s.SystemAuth
		}
		catalogueTestWrite(t, path, catalogueTestDocument(owner, now.Unix()+3600))
	}
	requests := 0
	c.Catalogue = &CatalogueClient{Settings: s, Now: func() time.Time { return now }, HTTP: catalogueTestDoer(func(r *http.Request) (*http.Response, error) {
		requests++
		if r.Header.Get("ChatGPT-Account-Id") != "new-owner" {
			t.Errorf("cached owner delayed launch: %s", r.Header.Get("ChatGPT-Account-Id"))
		}
		return catalogueTestResponse(http.StatusOK, catalogueTestPrimary("new-owner", "New Dot", "root-new-owner")), nil
	})}
	// Stop before process staging. The catalogue must already include this
	// owner before any immutable worker snapshots can be launched.
	c.Executable = filepath.Join(t.TempDir(), "missing-helper")
	if _, err := c.Start(context.Background(), false); err == nil {
		t.Fatal("missing helper accepted")
	}
	loaded, err := c.settings()
	if err != nil {
		t.Fatal(err)
	}
	if requests != 1 || len(loaded.Dots) != 3 || loaded.Dots[StableHandle("new-owner", "profile-new-owner")].DisplayName != "New Dot" {
		t.Fatalf("launch froze stale catalogue: requests=%d profiles=%d", requests, len(loaded.Dots))
	}
}

func TestStartupDiscoverySkippedCachedOwnersAndPreservedIncompleteInventory(t *testing.T) {
	for _, expired := range []bool{false, true} {
		t.Run(map[bool]string{false: "healthy", true: "incomplete cached owner"}[expired], func(t *testing.T) {
			c, s := startupDiscoveryFixture(t)
			if expired {
				catalogueTestWrite(t, filepath.Join(s.CQAccounts, "secondary.auth.json"), catalogueTestDocument("fixture-secondary", 0))
			}
			c.Catalogue.HTTP = catalogueTestDoer(func(*http.Request) (*http.Response, error) {
				t.Error("cached owner made startup wait for HTTP")
				return nil, errors.New("unexpected")
			})
			before, _ := os.ReadFile(filepath.Join(c.Root, "settings.json"))
			pending, err := c.prepareStartupDiscovery(context.Background(), s)
			after, _ := os.ReadFile(filepath.Join(c.Root, "settings.json"))
			if err != nil || pending || !bytes.Equal(before, after) || len(s.Dots) != 2 {
				t.Fatalf("cached catalogue changed: pending=%v err=%v", pending, err)
			}
		})
	}
}

func TestStartupDiscoveryPreservedKnownDotsOnNewOwnerFailure(t *testing.T) {
	for _, variant := range []string{"HTTP unavailable", "panic", "invalid new credential"} {
		t.Run(variant, func(t *testing.T) {
			c, s := startupDiscoveryFixture(t)
			expires := c.Catalogue.now().Unix() + 3600
			if variant == "invalid new credential" {
				expires = 0
			}
			catalogueTestWrite(t, filepath.Join(s.CQAccounts, "new.auth.json"), catalogueTestDocument("new-owner", expires))
			var requests atomic.Int32
			c.Catalogue.HTTP = catalogueTestDoer(func(r *http.Request) (*http.Response, error) {
				requests.Add(1)
				if r.Header.Get("ChatGPT-Account-Id") != "new-owner" {
					t.Error("cached account contacted")
				}
				if variant == "panic" {
					panic("synthetic upstream panic")
				}
				return catalogueTestResponse(http.StatusServiceUnavailable, map[string]any{"error": "unavailable"}), nil
			})
			before, _ := os.ReadFile(filepath.Join(c.Root, "settings.json"))
			pending, err := c.prepareStartupDiscovery(context.Background(), s)
			after, _ := os.ReadFile(filepath.Join(c.Root, "settings.json"))
			if err != nil || !pending || !bytes.Equal(before, after) || len(s.Dots) != 2 {
				t.Fatalf("known profiles lost: pending=%v err=%v", pending, err)
			}
			want := int32(1)
			if variant == "invalid new credential" {
				want = 0
			}
			if requests.Load() != want {
				t.Fatalf("requests=%d want=%d", requests.Load(), want)
			}
		})
	}
}

func TestStartupDiscoveryUsedOwnerParallelismAndSharedCancellation(t *testing.T) {
	c, s := startupDiscoveryFixture(t)
	for _, owner := range []string{"new-one", "new-two"} {
		catalogueTestWrite(t, filepath.Join(s.CQAccounts, owner+".auth.json"), catalogueTestDocument(owner, c.Catalogue.now().Unix()+3600))
	}
	arrived := make(chan string, 2)
	release := make(chan struct{})
	c.Catalogue.HTTP = catalogueTestDoer(func(r *http.Request) (*http.Response, error) {
		owner := r.Header.Get("ChatGPT-Account-Id")
		arrived <- owner
		select {
		case <-release:
			return catalogueTestResponse(http.StatusOK, catalogueTestPrimary(owner, owner, "root-"+owner)), nil
		case <-r.Context().Done():
			return nil, r.Context().Err()
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		defer func() {
			if recover() != nil {
				done <- errors.New("startup panic")
			}
		}()
		_, err := c.prepareStartupDiscovery(ctx, s)
		done <- err
	}()
	for range 2 {
		select {
		case <-arrived:
		case <-ctx.Done():
			t.Fatal("new owner discovery was serial")
		}
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if len(s.Dots) != 4 {
		t.Fatalf("parallel discovery lost owner: %d", len(s.Dots))
	}
	cancelled, stop := context.WithCancel(context.Background())
	stop()
	if _, err := c.prepareStartupDiscovery(cancelled, s); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled startup continued: %v", err)
	}
}

func TestStartupDiscoveryRejectedOwnerCollisionWithoutPersisting(t *testing.T) {
	c, s := startupDiscoveryFixture(t)
	catalogueTestWrite(t, filepath.Join(s.CQAccounts, "new.auth.json"), catalogueTestDocument("new-owner", c.Catalogue.now().Unix()+3600))
	c.Catalogue.HTTP = catalogueTestDoer(func(*http.Request) (*http.Response, error) {
		return catalogueTestResponse(http.StatusOK, catalogueTestPrimary(s.SystemAccountID, "forged owner", "root-"+s.SystemAccountID)), nil
	})
	before, _ := os.ReadFile(filepath.Join(c.Root, "settings.json"))
	if _, err := c.prepareStartupDiscovery(context.Background(), s); err == nil {
		t.Fatal("cross-owner profile collision accepted")
	}
	after, _ := os.ReadFile(filepath.Join(c.Root, "settings.json"))
	if !bytes.Equal(before, after) || len(s.Dots) != 2 {
		t.Fatal("failed discovery changed known profiles")
	}
}

func TestStartupDiscoveryRejectedCachedAliasCollision(t *testing.T) {
	c, s := startupDiscoveryFixture(t)
	oldOwner, newOwner := "collision-owner-90901", "collision-owner-95743"
	if AccountAlias(oldOwner) != AccountAlias(newOwner) {
		t.Fatal("collision fixture changed")
	}
	handle := StableHandle(oldOwner, "profile-collision")
	s.Dots[handle] = Dot{OwnerAlias: AccountAlias(oldOwner), OwnerAccountID: oldOwner, ProfileID: "profile-collision", RootThreadID: "root-collision", DisplayName: "Cached"}
	if err := SaveJSON(filepath.Join(c.Root, "settings.json"), s); err != nil {
		t.Fatal(err)
	}
	catalogueTestWrite(t, filepath.Join(s.CQAccounts, "new.auth.json"), catalogueTestDocument(newOwner, c.Catalogue.now().Unix()+3600))
	c.Catalogue.HTTP = catalogueTestDoer(func(*http.Request) (*http.Response, error) {
		t.Error("colliding owner contacted upstream")
		return nil, errors.New("unexpected")
	})
	before, _ := os.ReadFile(filepath.Join(c.Root, "settings.json"))
	_, err := c.prepareStartupDiscovery(context.Background(), s)
	var collision *IdentityCollision
	if !errors.As(err, &collision) {
		t.Fatalf("cached alias collision accepted: %v", err)
	}
	after, _ := os.ReadFile(filepath.Join(c.Root, "settings.json"))
	if !bytes.Equal(before, after) {
		t.Fatal("alias collision changed catalogue")
	}
}

func TestStartPreservedAppOpenedDuringNewOwnerDiscovery(t *testing.T) {
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("native process enumeration")
	}
	c, s := startupDiscoveryFixture(t)
	c.BaseURL = "http://127.0.0.1:12345"
	source, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	binary, err := os.ReadFile(source)
	if err != nil {
		t.Fatal(err)
	}
	helper := filepath.Join(s.RuntimeRoot, "cq-dots-runtime")
	app := filepath.Join(s.AppPath, "Contents", "MacOS", "ChatGPT")
	if err := os.MkdirAll(filepath.Dir(app), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{helper, app} {
		if err := os.WriteFile(path, binary, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	input, keepAlive, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = input.Close(); _ = keepAlive.Close() })
	env := append(os.Environ(), "CQ_DOTS_CONTROL_PROCESS_FIXTURE=1")
	watcher := exec.Command(helper, "__dots", "account-watch", c.Root)
	watcher.Dir = s.RuntimeRoot
	watcher.Stdin, watcher.Env = input, env
	if err := watcher.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = watcher.Process.Kill(); _ = watcher.Wait() })
	command := exec.Command(app, "--")
	command.Stdin, command.Env = input, env
	t.Cleanup(func() {
		if command.Process != nil {
			_ = command.Process.Kill()
			_ = command.Wait()
		}
	})
	catalogueTestWrite(t, filepath.Join(s.CQAccounts, "new.auth.json"), catalogueTestDocument("new-owner", c.Catalogue.now().Unix()+3600))
	c.Catalogue.HTTP = catalogueTestDoer(func(*http.Request) (*http.Response, error) {
		if err := command.Start(); err != nil {
			return nil, err
		}
		return catalogueTestResponse(http.StatusOK, catalogueTestPrimary("new-owner", "New Dot", "root-new-owner")), nil
	})
	c.Native = nativeEvaluationFunc(func(context.Context, string) ([]any, error) {
		t.Error("startup attached to the app opened during discovery")
		return nil, errors.New("unexpected")
	})
	result, err := c.Start(context.Background(), false)
	if err != nil || result["restart_required"] != true || result["started"] != false {
		t.Fatalf("racing app not preserved: %#v %v", result, err)
	}
	if command.Process == nil {
		t.Fatal("inert app never started")
	}
	if err := command.Process.Signal(syscall.Signal(0)); err != nil {
		t.Fatalf("startup killed app opened during discovery: %v", err)
	}
	if err := watcher.Process.Signal(syscall.Signal(0)); err != nil {
		t.Fatalf("startup replaced live workers: %v", err)
	}
}
