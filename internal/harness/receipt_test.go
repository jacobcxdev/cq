package harness

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/jacobcxdev/cq/internal/fsutil"
)

func receiptFixture() Receipt {
	return Receipt{
		Version: 1, ChatID: testChat, Mode: "plugins", Profile: Minimal,
		Source: "chat", State: "prepared", BaselinePlugins: []string{"calendar", "drive"},
		AppliedPlugins: []string{"calendar"}, PendingPlugins: []string{"drive"},
		UpdatedAt: time.Date(2026, 11, 5, 11, 0, 0, 0, time.UTC),
	}
}

func TestReceiptMissingDoesNotCreateState(t *testing.T) {
	fsys := fsutil.NewMemFS()
	receipts := Receipts{FS: fsys, Dir: "/state/receipts"}
	if _, err := receipts.Load(testChat); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing receipt error = %v, want not exist", err)
	}
	if _, err := fsys.Stat(receipts.Dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing load created directory: %v", err)
	}
}

func TestReceiptSavesPrivateBaselineWhileLeaseHeld(t *testing.T) {
	fsys := fsutil.NewMemFS()
	receipts := Receipts{FS: fsys, Dir: "/state/receipts"}
	lease, err := receipts.Acquire(testChat)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()
	before := time.Now().UTC()
	want := receiptFixture()
	if err := receipts.Save(want); err != nil {
		t.Fatal(err)
	}
	got, err := receipts.Load(testChat)
	if err != nil {
		t.Fatal(err)
	}
	if got.UpdatedAt.Before(before) || got.UpdatedAt.After(time.Now().UTC()) || got.UpdatedAt.Location() != time.UTC {
		t.Fatalf("saved timestamp = %v, want current UTC", got.UpdatedAt)
	}
	want.UpdatedAt = got.UpdatedAt
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("receipt = %#v, want %#v", got, want)
	}
	for path, mode := range map[string]os.FileMode{
		receipts.Dir: 0o700,
		filepath.Join(receipts.Dir, testChat+".json"): 0o600,
		filepath.Join(receipts.Dir, testChat+".lock"): 0o600,
	} {
		info, err := fsys.Stat(path)
		if err != nil || info.Mode().Perm() != mode {
			t.Fatalf("private receipt state %s = %v, %v; want %04o", path, info, err, mode)
		}
	}
}

func TestReceiptAcceptsLifecycleStatesAndFullMode(t *testing.T) {
	receipts := Receipts{FS: fsutil.NewMemFS(), Dir: "/state/receipts"}
	for _, state := range []string{"prepared", "applied", "pending", "failed", "diverged", "restored"} {
		receipt := receiptFixture()
		receipt.Mode, receipt.Profile, receipt.State = "full", Full, state
		if err := receipts.Save(receipt); err != nil {
			t.Fatalf("save state %s: %v", state, err)
		}
		if got, err := receipts.Load(testChat); err != nil || got.Mode != "full" || got.Profile != Full || got.State != state {
			t.Fatalf("load state %s = %#v, %v", state, got, err)
		}
	}
}

func TestReceiptRejectedUnclassifiedFailureData(t *testing.T) {
	for name, alter := range map[string]func(*ApplicationFailure){
		"stage":                   func(failure *ApplicationFailure) { failure.Stage = "native-secret" },
		"reason":                  func(failure *ApplicationFailure) { failure.Reason = "native-secret" },
		"rollback":                func(failure *ApplicationFailure) { failure.Rollback = "unknown" },
		"rollback detail":         func(failure *ApplicationFailure) { failure.RollbackStage = "resume" },
		"missing rollback detail": func(failure *ApplicationFailure) { failure.Rollback = "failed" },
	} {
		t.Run(name, func(t *testing.T) {
			receipt := receiptFixture()
			receipt.State = "failed"
			receipt.Failure = &ApplicationFailure{Stage: "resume", Reason: "rpc_rejected", Rollback: "restored"}
			alter(receipt.Failure)
			if err := (Receipts{FS: fsutil.NewMemFS(), Dir: "/state/receipts"}).Save(receipt); err == nil {
				t.Fatal("unclassified failure detail accepted")
			}
		})
	}
}

func TestReceiptRejectsInvalidValuesBeforeCreatingState(t *testing.T) {
	for name, alter := range map[string]func(*Receipt){
		"future version":        func(receipt *Receipt) { receipt.Version = 2 },
		"chat uri":              func(receipt *Receipt) { receipt.ChatID = "codex://threads/" + testChat },
		"uppercase uuid":        func(receipt *Receipt) { receipt.ChatID = strings.ToUpper(testChat) },
		"path chat":             func(receipt *Receipt) { receipt.ChatID = "../other" },
		"unknown mode":          func(receipt *Receipt) { receipt.Mode = "launcher" },
		"auto profile":          func(receipt *Receipt) { receipt.Profile = "auto" },
		"missing source":        func(receipt *Receipt) { receipt.Source = "" },
		"unknown state":         func(receipt *Receipt) { receipt.State = "complete" },
		"empty baseline plugin": func(receipt *Receipt) { receipt.BaselinePlugins = []string{""} },
		"nul applied plugin":    func(receipt *Receipt) { receipt.AppliedPlugins = []string{"drive\x00"} },
		"duplicate baseline":    func(receipt *Receipt) { receipt.BaselinePlugins = []string{"drive", "drive"} },
		"empty pending plugin":  func(receipt *Receipt) { receipt.PendingPlugins = []string{""} },
		"nul pending plugin":    func(receipt *Receipt) { receipt.PendingPlugins = []string{"drive\x00"} },
		"duplicate pending":     func(receipt *Receipt) { receipt.PendingPlugins = []string{"drive", "drive"} },
	} {
		t.Run(name, func(t *testing.T) {
			fsys := fsutil.NewMemFS()
			receipts := Receipts{FS: fsys, Dir: "/state/receipts"}
			receipt := receiptFixture()
			alter(&receipt)
			if err := receipts.Save(receipt); err == nil {
				t.Fatal("accepted invalid receipt")
			}
			if _, err := fsys.Stat(receipts.Dir); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("invalid receipt created state: %v", err)
			}
		})
	}
	for _, dir := range []string{"", "relative", "/state/../receipts", "/state/receipts\x00"} {
		receipts := Receipts{FS: fsutil.NewMemFS(), Dir: dir}
		if err := receipts.Save(receiptFixture()); err == nil {
			t.Errorf("save accepted directory %q", dir)
		}
		if _, err := receipts.Load(testChat); err == nil {
			t.Errorf("load accepted directory %q", dir)
		}
		if _, err := receipts.Acquire(testChat); err == nil {
			t.Errorf("lease accepted directory %q", dir)
		}
	}
}

func TestReceiptLoadRejectsInvalidStoredState(t *testing.T) {
	valid, err := json.Marshal(receiptFixture())
	if err != nil {
		t.Fatal(err)
	}
	for name, raw := range map[string]string{
		"unknown field":  strings.TrimSuffix(string(valid), "}") + `,"unknown":true}`,
		"trailing data":  string(valid) + `{}`,
		"null receipt":   `null`,
		"wrong chat":     strings.Replace(string(valid), testChat, "019c7714-3b77-74d1-9866-e1f484aae2ab", 1),
		"zero timestamp": strings.Replace(string(valid), "2026-11-05T11:00:00Z", "0001-01-01T00:00:00Z", 1),
		"oversized":      string(valid) + strings.Repeat(" ", 1<<20),
	} {
		t.Run(name, func(t *testing.T) {
			fsys := fsutil.NewMemFS()
			receipts := Receipts{FS: fsys, Dir: "/state/receipts"}
			if err := fsys.MkdirAll(receipts.Dir, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := fsys.WriteFile(filepath.Join(receipts.Dir, testChat+".json"), []byte(raw), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := receipts.Load(testChat); err == nil {
				t.Fatal("accepted invalid stored receipt")
			}
		})
	}
}

func TestReceiptFailedWriteLeavesBaselineIntact(t *testing.T) {
	fsys := fsutil.NewMemFS()
	receipts := Receipts{FS: fsys, Dir: "/state/receipts"}
	if err := receipts.Save(receiptFixture()); err != nil {
		t.Fatal(err)
	}
	before, err := fsys.ReadFile(filepath.Join(receipts.Dir, testChat+".json"))
	if err != nil {
		t.Fatal(err)
	}
	broken := Receipts{FS: renameFailureFS{fsys}, Dir: receipts.Dir}
	receipt := receiptFixture()
	receipt.State = "applied"
	if err := broken.Save(receipt); !errors.Is(err, errTestRename) {
		t.Fatalf("save error = %v, want injected rename failure", err)
	}
	after, err := fsys.ReadFile(filepath.Join(receipts.Dir, testChat+".json"))
	if err != nil || string(after) != string(before) {
		t.Fatalf("failed save altered durable baseline: %s, %v", after, err)
	}
}

func TestReceiptLeaseIsPerChatAndReusableAfterClose(t *testing.T) {
	receipts := Receipts{FS: fsutil.NewMemFS(), Dir: "/state/receipts"}
	first, err := receipts.Acquire(testChat)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	if second, err := receipts.Acquire(testChat); !errors.Is(err, fsutil.ErrExclusiveLockHeld) {
		if second != nil {
			second.Close()
		}
		t.Fatalf("second owner = %v, want held lock", err)
	}
	other, err := receipts.Acquire("019c7714-3b77-74d1-9866-e1f484aae2ab")
	if err != nil {
		t.Fatalf("another chat blocked: %v", err)
	}
	if err := other.Close(); err != nil {
		t.Fatal(err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	next, err := receipts.Acquire(testChat)
	if err != nil {
		t.Fatalf("released chat remained blocked: %v", err)
	}
	if err := next.Close(); err != nil {
		t.Fatal(err)
	}
	for _, chat := range []string{"../other", "", strings.ToUpper(testChat)} {
		if lease, err := receipts.Acquire(chat); err == nil {
			lease.Close()
			t.Errorf("accepted invalid lease chat %q", chat)
		}
	}
}

func TestReceiptLeaseExcludesOtherProcesses(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("OS secure temporary roots are not enabled on Windows")
	}
	if os.Getenv("CQ_TEST_RECEIPT_LEASE_HELPER") != "" {
		receipts := Receipts{FS: fsutil.OSFileSystem{}, Dir: os.Getenv("CQ_TEST_RECEIPT_LEASE_DIR")}
		lease, err := receipts.Acquire(testChat)
		if errors.Is(err, fsutil.ErrExclusiveLockHeld) {
			os.Exit(3)
		}
		if err != nil {
			os.Exit(4)
		}
		if err := lease.Close(); err != nil {
			os.Exit(5)
		}
		os.Exit(0)
	}
	receipts := Receipts{FS: fsutil.OSFileSystem{}, Dir: filepath.Join(t.TempDir(), "receipts")}
	lease, err := receipts.Acquire(testChat)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()
	run := func() error {
		command := exec.Command(os.Args[0], "-test.run=^TestReceiptLeaseExcludesOtherProcesses$")
		command.Env = append(os.Environ(), "CQ_TEST_RECEIPT_LEASE_HELPER=1", "CQ_TEST_RECEIPT_LEASE_DIR="+receipts.Dir)
		return command.Run()
	}
	if err := run(); err == nil {
		t.Fatal("second process acquired owned chat")
	} else if exitErr, ok := err.(*exec.ExitError); !ok || exitErr.ExitCode() != 3 {
		t.Fatalf("second process result = %v, want held-lock exit 3", err)
	}
	if err := lease.Close(); err != nil {
		t.Fatal(err)
	}
	if err := run(); err != nil {
		t.Fatalf("second process could not acquire released chat: %v", err)
	}
}
