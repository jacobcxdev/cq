package harness

import (
	"context"
	"errors"
	"os"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jacobcxdev/cq/internal/fsutil"
)

const controllerOtherChat = "019c7714-3b77-74d1-9866-e1f484aae2ab"

type controllerPluginUpdate struct {
	snapshot Snapshot
	plugins  []string
}

type controllerDesktop struct {
	snapshots    map[string]Snapshot
	inspected    []string
	updates      []controllerPluginUpdate
	applied      bool
	updateErr    error
	beforeUpdate func(Snapshot, []string)
}

func (desktop *controllerDesktop) Inspect(_ context.Context, id string) (Snapshot, error) {
	desktop.inspected = append(desktop.inspected, id)
	snapshot, ok := desktop.snapshots[id]
	if !ok {
		return Snapshot{}, os.ErrNotExist
	}
	snapshot.DisabledPluginIDs = slices.Clone(snapshot.DisabledPluginIDs)
	return snapshot, nil
}

func (desktop *controllerDesktop) ReadModel(_ context.Context, id string) (Snapshot, error) {
	snapshot, ok := desktop.snapshots[id]
	if !ok {
		return Snapshot{}, os.ErrNotExist
	}
	return Snapshot{ChatID: id, Model: snapshot.Model, OwnerID: snapshot.OwnerID}, nil
}

func (desktop *controllerDesktop) UpdatePlugins(_ context.Context, snapshot Snapshot, ids []string) (bool, error) {
	desktop.updates = append(desktop.updates, controllerPluginUpdate{snapshot, slices.Clone(ids)})
	if desktop.beforeUpdate != nil {
		desktop.beforeUpdate(snapshot, ids)
	}
	if desktop.updateErr != nil || !desktop.applied {
		return false, desktop.updateErr
	}
	current := desktop.snapshots[snapshot.ChatID]
	current.DisabledPluginIDs = slices.Clone(ids)
	desktop.snapshots[snapshot.ChatID] = current
	return true, nil
}

type controllerInventory struct {
	ids    []string
	cwds   []string
	err    error
	change func()
}

func (inventory *controllerInventory) Installed(_ context.Context, cwd string) ([]string, error) {
	inventory.cwds = append(inventory.cwds, cwd)
	if inventory.change != nil {
		inventory.change()
	}
	return slices.Clone(inventory.ids), inventory.err
}

func controllerPolicy() Policy {
	return Policy{Version: 1, Enabled: true, Projects: map[string]Profile{}, Chats: map[string]Profile{}}
}

func controllerFixture() (*Controller, *controllerDesktop, *controllerInventory, Receipts) {
	effort := "high"
	snapshot := Snapshot{
		ChatID: testChat, OwnerID: desktopTestOwner, Model: "glm-5.3", ModelProvider: "cq",
		Cwd: "/work/project", DisabledPluginIDs: []string{"user-disabled"}, PluginBaselineKnown: true,
		ReasoningEffort: &effort, ReasoningEffortKnown: true, ActiveTurnID: "existing-turn", Active: true,
	}
	other := snapshot
	other.ChatID, other.DisabledPluginIDs = controllerOtherChat, []string{"other-chat-setting"}
	desktop := &controllerDesktop{snapshots: map[string]Snapshot{testChat: snapshot, controllerOtherChat: other}, applied: true}
	inventory := &controllerInventory{ids: []string{"z-plugin", "user-disabled", "a-plugin", "a-plugin"}}
	receipts := Receipts{FS: fsutil.NewMemFS(), Dir: "/state/controller-receipts"}
	return NewController(func() (Policy, error) { return controllerPolicy(), nil }, desktop, receipts, inventory), desktop, inventory, receipts
}

func TestControllerMinimalUnionRetainsDurableOriginalBaseline(t *testing.T) {
	controller, desktop, inventory, receipts := controllerFixture()
	original := desktop.snapshots[testChat]
	other := desktop.snapshots[controllerOtherChat]
	desktop.beforeUpdate = func(snapshot Snapshot, desired []string) {
		receipt, err := receipts.Load(testChat)
		if err != nil || receipt.State != "prepared" || !slices.Equal(receipt.BaselinePlugins, []string{"user-disabled"}) || !slices.Equal(receipt.PendingPlugins, desired) {
			t.Fatalf("update preceded durable baseline: %#v, %v", receipt, err)
		}
		if snapshot.ChatID != testChat || snapshot.Model != "glm-5.3" || snapshot.ReasoningEffort == nil || *snapshot.ReasoningEffort != "high" {
			t.Fatalf("update lost exact chat conditions: %#v", snapshot)
		}
	}
	if _, err := controller.Reconcile(context.Background(), testChat, controllerPolicy()); err != nil {
		t.Fatal(err)
	}
	want := []string{"a-plugin", "user-disabled", "z-plugin"}
	if len(desktop.updates) != 1 || !slices.Equal(desktop.updates[0].plugins, want) || !slices.Equal(inventory.cwds, []string{"/work/project"}) {
		t.Fatalf("minimal update = %#v; inventory cwd = %v", desktop.updates, inventory.cwds)
	}
	current := desktop.snapshots[testChat]
	current.DisabledPluginIDs = original.DisabledPluginIDs
	if !reflect.DeepEqual(current, original) || !reflect.DeepEqual(desktop.snapshots[controllerOtherChat], other) {
		t.Fatal("profile changed other settings or another chat")
	}
	if _, err := receipts.Load(controllerOtherChat); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("created another chat's receipt: %v", err)
	}
	// Recreate the controller to prove the original baseline lives in the receipt,
	// then discover another plugin without capturing our own applied list.
	controller = NewController(nil, desktop, receipts, inventory)
	inventory.ids = append(inventory.ids, "new-plugin")
	if _, err := controller.Reconcile(context.Background(), testChat, controllerPolicy()); err != nil {
		t.Fatal(err)
	}
	receipt, err := receipts.Load(testChat)
	if err != nil || receipt.State != "applied" || receipt.Mode != "plugins" || receipt.Profile != Minimal || !slices.Equal(receipt.BaselinePlugins, []string{"user-disabled"}) || !slices.Equal(receipt.AppliedPlugins, []string{"a-plugin", "new-plugin", "user-disabled", "z-plugin"}) {
		t.Fatalf("repeated minimal lost original baseline: %#v, %v", receipt, err)
	}
}

func TestControllerRestoresOriginalPluginsForUnmanagedAndFullSelections(t *testing.T) {
	for _, test := range []struct {
		name   string
		change func(*Policy, *Snapshot)
		source string
	}{
		{"non GLM", func(_ *Policy, snapshot *Snapshot) { snapshot.Model = "gpt-6.1" }, "model"},
		{"disabled", func(policy *Policy, _ *Snapshot) { policy.Enabled = false }, "disabled"},
		{"chat full", func(policy *Policy, _ *Snapshot) { policy.Chats[testChat] = Full }, "chat"},
		{"project full", func(policy *Policy, _ *Snapshot) { policy.Projects["/work"] = Full }, "project"},
	} {
		t.Run(test.name, func(t *testing.T) {
			controller, desktop, inventory, receipts := controllerFixture()
			if _, err := controller.Reconcile(context.Background(), testChat, controllerPolicy()); err != nil {
				t.Fatal(err)
			}
			policy, snapshot := controllerPolicy(), desktop.snapshots[testChat]
			test.change(&policy, &snapshot)
			desktop.snapshots[testChat] = snapshot
			controller = NewController(nil, desktop, receipts, inventory)
			if _, err := controller.Reconcile(context.Background(), testChat, policy); err != nil {
				t.Fatal(err)
			}
			receipt, err := receipts.Load(testChat)
			if err != nil || receipt.State != "restored" || receipt.Profile != Full || receipt.Source != test.source || !slices.Equal(desktop.snapshots[testChat].DisabledPluginIDs, []string{"user-disabled"}) {
				t.Fatalf("restore = %#v, %v; desktop = %#v", receipt, err, desktop.snapshots[testChat])
			}
			if len(desktop.updates) != 2 || len(inventory.cwds) != 1 {
				t.Fatalf("restore rediscovered inventory or failed to update: %d, %v", len(desktop.updates), inventory.cwds)
			}
			if _, err := controller.Reconcile(context.Background(), testChat, policy); err != nil || len(desktop.updates) != 2 {
				t.Fatalf("restored state caused redundant update: %v, %#v", err, desktop.updates)
			}
		})
	}
}

func TestControllerChatOverrideTakesPrecedenceOverProject(t *testing.T) {
	controller, desktop, _, receipts := controllerFixture()
	policy := controllerPolicy()
	policy.Projects["/work"] = Full
	policy.Chats[testChat] = Minimal
	if _, err := controller.Reconcile(context.Background(), testChat, policy); err != nil {
		t.Fatal(err)
	}
	receipt, err := receipts.Load(testChat)
	if err != nil || receipt.Source != "chat" || receipt.Profile != Minimal || len(desktop.updates) != 1 {
		t.Fatalf("chat override = %#v, %v; updates = %#v", receipt, err, desktop.updates)
	}
}

func TestControllerDoesNotCompeteWithFullLauncherLease(t *testing.T) {
	controller, desktop, inventory, receipts := controllerFixture()
	lease, err := receipts.Acquire(testChat)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()
	receipt := receiptFixture()
	receipt.Mode, receipt.State = "full", "applied"
	if err := receipts.Save(receipt); err != nil {
		t.Fatal(err)
	}
	before, _ := receipts.Load(testChat)
	if _, err := controller.Reconcile(context.Background(), testChat, controllerPolicy()); err != nil {
		t.Fatal(err)
	}
	after, _ := receipts.Load(testChat)
	if !reflect.DeepEqual(before, after) || len(desktop.inspected) != 0 || len(desktop.updates) != 0 || len(inventory.cwds) != 0 {
		t.Fatalf("controller competed with full bridge: %#v, %#v", before, after)
	}
}

func TestControllerPreservesManualPluginDivergence(t *testing.T) {
	controller, desktop, _, receipts := controllerFixture()
	if _, err := controller.Reconcile(context.Background(), testChat, controllerPolicy()); err != nil {
		t.Fatal(err)
	}
	snapshot := desktop.snapshots[testChat]
	snapshot.DisabledPluginIDs = []string{"manually-selected"}
	desktop.snapshots[testChat] = snapshot
	if _, err := controller.Reconcile(context.Background(), testChat, controllerPolicy()); err != nil {
		t.Fatal(err)
	}
	receipt, err := receipts.Load(testChat)
	if err != nil || receipt.State != "diverged" || !slices.Equal(receipt.BaselinePlugins, []string{"user-disabled"}) {
		t.Fatalf("manual divergence was not retained: %#v, %v", receipt, err)
	}
	// A later model switch must preserve the user's manual settings too.
	snapshot.Model = "gpt-6.1"
	desktop.snapshots[testChat] = snapshot
	if _, err := controller.Reconcile(context.Background(), testChat, controllerPolicy()); err != nil {
		t.Fatal(err)
	}
	receipt, err = receipts.Load(testChat)
	if err != nil || !slices.Equal(desktop.snapshots[testChat].DisabledPluginIDs, []string{"manually-selected"}) || len(desktop.updates) != 1 {
		t.Fatalf("manual divergence overwritten: %#v, %v; updates = %#v", receipt, err, desktop.updates)
	}
}

func TestControllerRestoreConditionRejectionCanRetry(t *testing.T) {
	controller, desktop, _, receipts := controllerFixture()
	if _, err := controller.Reconcile(context.Background(), testChat, controllerPolicy()); err != nil {
		t.Fatal(err)
	}
	snapshot := desktop.snapshots[testChat]
	snapshot.Model = "gpt-6.1"
	desktop.snapshots[testChat] = snapshot
	desktop.applied = false
	if _, err := controller.Reconcile(context.Background(), testChat, controllerPolicy()); err == nil || !strings.Contains(err.Error(), "declined") {
		t.Fatalf("restore condition rejection = %v", err)
	}
	receipt, err := receipts.Load(testChat)
	if err != nil || receipt.State != "prepared" || !slices.Equal(receipt.AppliedPlugins, []string{"a-plugin", "user-disabled", "z-plugin"}) || !slices.Equal(receipt.PendingPlugins, []string{"user-disabled"}) || !slices.Equal(receipt.BaselinePlugins, []string{"user-disabled"}) {
		t.Fatalf("restore rejection lost prior applied list: %#v, %v", receipt, err)
	}
	desktop.applied = true
	controller = NewController(nil, desktop, receipts, nil)
	if _, err := controller.Reconcile(context.Background(), testChat, controllerPolicy()); err != nil {
		t.Fatal(err)
	}
	receipt, err = receipts.Load(testChat)
	if err != nil || receipt.State != "restored" || receipt.PendingPlugins != nil || !slices.Equal(desktop.snapshots[testChat].DisabledPluginIDs, []string{"user-disabled"}) {
		t.Fatalf("restore retry diverged or failed to restore: %#v, %v", receipt, err)
	}
}

func TestControllerRecoversPreparedReceiptWhosePendingTargetWasApplied(t *testing.T) {
	for _, restoring := range []bool{false, true} {
		t.Run(map[bool]string{false: "minimal target applied", true: "restore target applied"}[restoring], func(t *testing.T) {
			controller, desktop, _, receipts := controllerFixture()
			baseline := []string{"user-disabled"}
			minimal := []string{"a-plugin", "user-disabled", "z-plugin"}
			receipt := Receipt{Version: 1, ChatID: testChat, Mode: "plugins", Profile: Minimal, Source: "default", State: "prepared", BaselinePlugins: baseline, AppliedPlugins: baseline, PendingPlugins: minimal}
			snapshot := desktop.snapshots[testChat]
			snapshot.DisabledPluginIDs = slices.Clone(minimal)
			wantState := "applied"
			if restoring {
				receipt.Profile, receipt.Source, receipt.AppliedPlugins, receipt.PendingPlugins = Full, "model", minimal, baseline
				snapshot.Model, snapshot.DisabledPluginIDs = "gpt-6.1", slices.Clone(baseline)
				wantState = "restored"
			}
			desktop.snapshots[testChat] = snapshot
			lease, err := receipts.Acquire(testChat)
			if err != nil {
				t.Fatal(err)
			}
			if err := receipts.Save(receipt); err != nil {
				_ = lease.Close()
				t.Fatal(err)
			}
			if err := lease.Close(); err != nil {
				t.Fatal(err)
			}
			if _, err := controller.Reconcile(context.Background(), testChat, controllerPolicy()); err != nil {
				t.Fatal(err)
			}
			got, err := receipts.Load(testChat)
			if err != nil || got.State != wantState || got.PendingPlugins != nil || !slices.Equal(got.BaselinePlugins, baseline) || !slices.Equal(got.AppliedPlugins, snapshot.DisabledPluginIDs) || len(desktop.updates) != 0 {
				t.Fatalf("prepared crash recovery = %#v, %v; updates = %#v", got, err, desktop.updates)
			}
			if !restoring {
				snapshot.Model = "gpt-6.1"
				desktop.snapshots[testChat] = snapshot
				if _, err := controller.Reconcile(context.Background(), testChat, controllerPolicy()); err != nil || !slices.Equal(desktop.snapshots[testChat].DisabledPluginIDs, baseline) {
					t.Fatalf("recovered minimal could not restore original baseline: %v", err)
				}
			}
		})
	}
}

func TestControllerRechecksSettingsAfterInventoryDiscovery(t *testing.T) {
	for _, test := range []struct {
		name   string
		change func(*Snapshot)
	}{
		{"manual plugins", func(snapshot *Snapshot) { snapshot.DisabledPluginIDs = []string{"manual-during-discovery"} }},
		{"owner changed", func(snapshot *Snapshot) { snapshot.OwnerID = controllerOtherChat }},
		{"model changed", func(snapshot *Snapshot) { snapshot.Model = "gpt-6.1" }},
		{"baseline became unknown", func(snapshot *Snapshot) { snapshot.PluginBaselineKnown = false }},
	} {
		t.Run(test.name, func(t *testing.T) {
			controller, desktop, inventory, receipts := controllerFixture()
			inventory.change = func() {
				snapshot := desktop.snapshots[testChat]
				test.change(&snapshot)
				desktop.snapshots[testChat] = snapshot
			}
			if _, err := controller.Reconcile(context.Background(), testChat, controllerPolicy()); err != nil {
				t.Fatal(err)
			}
			receipt, err := receipts.Load(testChat)
			if err != nil || receipt.State != "diverged" || len(desktop.updates) != 0 || !slices.Equal(receipt.BaselinePlugins, []string{"user-disabled"}) {
				t.Fatalf("fresh settings were overwritten: %#v, %v; updates = %#v", receipt, err, desktop.updates)
			}
		})
	}
}

func TestControllerRejectedConditionRetainsRecoveryReceipt(t *testing.T) {
	controller, desktop, _, receipts := controllerFixture()
	desktop.applied = false
	if _, err := controller.Reconcile(context.Background(), testChat, controllerPolicy()); err == nil || !strings.Contains(err.Error(), "declined") {
		t.Fatalf("rejected condition = %v", err)
	}
	receipt, err := receipts.Load(testChat)
	if err != nil || receipt.State == "applied" || !slices.Equal(receipt.BaselinePlugins, []string{"user-disabled"}) || !slices.Equal(desktop.snapshots[testChat].DisabledPluginIDs, []string{"user-disabled"}) {
		t.Fatalf("rejected condition lost baseline or claimed application: %#v, %v", receipt, err)
	}
}

func TestControllerUnknownBaselineDoesNotWriteReceiptOrUpdate(t *testing.T) {
	for _, change := range []func(*Snapshot){
		func(snapshot *Snapshot) { snapshot.PluginBaselineKnown = false },
		func(snapshot *Snapshot) { snapshot.ReasoningEffortKnown = false },
		func(snapshot *Snapshot) { snapshot.Model = "" },
		func(snapshot *Snapshot) { snapshot.ChatID = controllerOtherChat },
	} {
		controller, desktop, inventory, receipts := controllerFixture()
		snapshot := desktop.snapshots[testChat]
		change(&snapshot)
		desktop.snapshots[testChat] = snapshot
		if _, err := controller.Reconcile(context.Background(), testChat, controllerPolicy()); err == nil {
			t.Fatal("unknown or mismatched baseline was accepted")
		}
		if _, err := receipts.Load(testChat); !errors.Is(err, os.ErrNotExist) || len(desktop.updates) != 0 || len(inventory.cwds) != 0 {
			t.Fatalf("unknown baseline caused mutation: %v, %#v", err, desktop.updates)
		}
	}
}

func TestControllerUnmanagedChatWithoutReceiptStaysUntouched(t *testing.T) {
	for _, mode := range []string{"non GLM", "disabled", "full"} {
		t.Run(mode, func(t *testing.T) {
			controller, desktop, inventory, receipts := controllerFixture()
			policy, snapshot := controllerPolicy(), desktop.snapshots[testChat]
			switch mode {
			case "non GLM":
				snapshot.Model = "gpt-6.1"
			case "disabled":
				policy.Enabled = false
			case "full":
				policy.Chats[testChat] = Full
			}
			desktop.snapshots[testChat] = snapshot
			if _, err := controller.Reconcile(context.Background(), testChat, policy); err != nil {
				t.Fatal(err)
			}
			if _, err := receipts.Load(testChat); !errors.Is(err, os.ErrNotExist) || len(desktop.updates) != 0 || len(inventory.cwds) != 0 {
				t.Fatalf("unmanaged chat without receipt changed: %v, %#v", err, desktop.updates)
			}
		})
	}
}

type controllerReceiptFailure struct {
	Receipts
	failure error
}

func (store controllerReceiptFailure) Save(Receipt) error { return store.failure }

func TestControllerDependencyFailureDoesNotApplyPlugins(t *testing.T) {
	for _, dependency := range []string{"inventory", "receipt"} {
		t.Run(dependency, func(t *testing.T) {
			controller, desktop, inventory, receipts := controllerFixture()
			failure := errors.New("dependency failed")
			if dependency == "inventory" {
				inventory.err = failure
			} else {
				controller.Receipts = controllerReceiptFailure{receipts, failure}
			}
			if _, err := controller.Reconcile(context.Background(), testChat, controllerPolicy()); !errors.Is(err, failure) {
				t.Fatalf("dependency failure = %v", err)
			}
			if len(desktop.updates) != 0 {
				t.Fatalf("dependency failure applied plugins: %#v", desktop.updates)
			}
			if _, err := receipts.Load(testChat); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("dependency failure saved receipt: %v", err)
			}
		})
	}
}

type controllerWorkerPolicy struct {
	mu      sync.Mutex
	enabled bool
	reads   chan struct{}
}

func (policy *controllerWorkerPolicy) load() (Policy, error) {
	policy.mu.Lock()
	enabled := policy.enabled
	policy.mu.Unlock()
	select {
	case policy.reads <- struct{}{}:
	default:
	}
	result := controllerPolicy()
	result.Enabled = enabled
	return result, nil
}

func (policy *controllerWorkerPolicy) disable() {
	policy.mu.Lock()
	defer policy.mu.Unlock()
	policy.enabled = false
}

type controllerWorkerDesktop struct {
	mu          sync.Mutex
	snapshots   map[string]Snapshot
	inspections map[string]int
	modelReads  int
	updates     []controllerPluginUpdate
	inspected   chan string
	updated     chan controllerPluginUpdate
	gateChat    string
	gate        <-chan struct{}
}

func (desktop *controllerWorkerDesktop) Inspect(ctx context.Context, id string) (Snapshot, error) {
	desktop.mu.Lock()
	snapshot, ok := desktop.snapshots[id]
	snapshot.DisabledPluginIDs = slices.Clone(snapshot.DisabledPluginIDs)
	desktop.inspections[id]++
	desktop.mu.Unlock()
	select {
	case desktop.inspected <- id:
	case <-ctx.Done():
		return Snapshot{}, ctx.Err()
	}
	if id == desktop.gateChat && desktop.gate != nil {
		select {
		case <-desktop.gate:
		case <-ctx.Done():
			return Snapshot{}, ctx.Err()
		}
	}
	if !ok {
		return Snapshot{}, os.ErrNotExist
	}
	return snapshot, nil
}

func (desktop *controllerWorkerDesktop) ReadModel(_ context.Context, id string) (Snapshot, error) {
	desktop.mu.Lock()
	defer desktop.mu.Unlock()
	desktop.modelReads++
	snapshot, ok := desktop.snapshots[id]
	if !ok {
		return Snapshot{}, os.ErrNotExist
	}
	return Snapshot{ChatID: id, OwnerID: snapshot.OwnerID, Model: snapshot.Model}, nil
}

func (desktop *controllerWorkerDesktop) UpdatePlugins(_ context.Context, snapshot Snapshot, ids []string) (bool, error) {
	desktop.mu.Lock()
	current := desktop.snapshots[snapshot.ChatID]
	current.DisabledPluginIDs = slices.Clone(ids)
	desktop.snapshots[snapshot.ChatID] = current
	update := controllerPluginUpdate{snapshot, slices.Clone(ids)}
	desktop.updates = append(desktop.updates, update)
	desktop.mu.Unlock()
	desktop.updated <- update
	return true, nil
}

func (desktop *controllerWorkerDesktop) setModel(id, model string) {
	desktop.mu.Lock()
	defer desktop.mu.Unlock()
	snapshot := desktop.snapshots[id]
	snapshot.Model = model
	desktop.snapshots[id] = snapshot
}

type controllerWorkerInventory struct{}

func (controllerWorkerInventory) Installed(context.Context, string) ([]string, error) {
	return []string{"a-plugin", "z-plugin"}, nil
}

func controllerWorkerFixture() (*Controller, *controllerWorkerDesktop, *controllerWorkerPolicy, Receipts) {
	_, original, _, receipts := controllerFixture()
	other := original.snapshots[controllerOtherChat]
	other.Model = "gpt-6.1"
	original.snapshots[controllerOtherChat] = other
	desktop := &controllerWorkerDesktop{
		snapshots: original.snapshots, inspections: make(map[string]int),
		inspected: make(chan string, 256), updated: make(chan controllerPluginUpdate, 256),
	}
	policy := &controllerWorkerPolicy{enabled: true, reads: make(chan struct{}, 256)}
	controller := NewController(policy.load, desktop, receipts, controllerWorkerInventory{})
	controller.pollInterval = 5 * time.Millisecond
	return controller, desktop, policy, receipts
}

func runControllerWorker(t *testing.T, controller *Controller) (context.CancelFunc, <-chan struct{}) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer func() {
			if value := recover(); value != nil {
				t.Errorf("controller worker panicked: %v", value)
			}
		}()
		controller.Run(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Error("cancelled controller worker did not exit")
		}
	})
	return cancel, done
}

func waitControllerUpdate(t *testing.T, desktop *controllerWorkerDesktop) controllerPluginUpdate {
	t.Helper()
	select {
	case update := <-desktop.updated:
		return update
	case <-time.After(2 * time.Second):
		t.Fatal("controller did not update plugin settings")
		return controllerPluginUpdate{}
	}
}

func stopControllerWorker(t *testing.T, cancel context.CancelFunc, done <-chan struct{}) {
	t.Helper()
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("cancelled controller worker did not exit")
	}
}

func TestControllerRunPollRestoresWithoutAnotherObservedRequest(t *testing.T) {
	for _, change := range []string{"model switch", "policy disabled"} {
		t.Run(change, func(t *testing.T) {
			controller, desktop, policy, receipts := controllerWorkerFixture()
			controller.Observe(testChat, "glm-5.3")
			cancel, done := runControllerWorker(t, controller)
			first := waitControllerUpdate(t, desktop)
			if first.snapshot.ChatID != testChat || !slices.Equal(first.plugins, []string{"a-plugin", "user-disabled", "z-plugin"}) {
				t.Fatalf("initial minimal application = %#v", first)
			}
			if change == "model switch" {
				desktop.setModel(testChat, "gpt-6.1")
			} else {
				policy.disable()
			}
			// Deliberately do not call Observe again. Persistent WebSocket turns
			// must be handled through the cheap owner-model polling path.
			restored := waitControllerUpdate(t, desktop)
			if restored.snapshot.ChatID != testChat || !slices.Equal(restored.plugins, []string{"user-disabled"}) {
				t.Fatalf("poll did not restore original baseline: %#v", restored)
			}
			stopControllerWorker(t, cancel, done)
			desktop.mu.Lock()
			reads, updates := desktop.modelReads, len(desktop.updates)
			other := slices.Clone(desktop.snapshots[controllerOtherChat].DisabledPluginIDs)
			desktop.mu.Unlock()
			receipt, err := receipts.Load(testChat)
			if err != nil || receipt.State != "restored" || reads == 0 || updates != 2 || !slices.Equal(other, []string{"other-chat-setting"}) {
				t.Fatalf("poll result = %#v, %v; reads=%d updates=%d other=%v", receipt, err, reads, updates, other)
			}
		})
	}
}

func TestControllerRunDefaultOffObservationLeavesDesktopAndStateUntouched(t *testing.T) {
	controller, desktop, policy, receipts := controllerWorkerFixture()
	policy.disable()
	controller.Observe(testChat, "glm-5.3")
	cancel, done := runControllerWorker(t, controller)
	select {
	case <-policy.reads:
	case <-time.After(2 * time.Second):
		t.Fatal("worker did not consider the observed request")
	}
	stopControllerWorker(t, cancel, done)
	desktop.mu.Lock()
	inspections, reads, updates := len(desktop.inspections), desktop.modelReads, len(desktop.updates)
	desktop.mu.Unlock()
	if inspections != 0 || reads != 0 || updates != 0 {
		t.Fatalf("default-off observation touched Desktop: inspections=%d reads=%d updates=%d", inspections, reads, updates)
	}
	if _, err := receipts.FS.Stat(receipts.Dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("default-off observation created receipt state: %v", err)
	}
}

func TestControllerRunCoalescesQueuedAndInFlightObservations(t *testing.T) {
	controller, desktop, _, receipts := controllerWorkerFixture()
	controller.pollInterval = time.Hour
	release := make(chan struct{})
	desktop.gateChat, desktop.gate = testChat, release
	for range 100 {
		controller.Observe(testChat, "glm-5.3")
	}
	cancel, done := runControllerWorker(t, controller)
	select {
	case <-desktop.inspected:
	case <-time.After(2 * time.Second):
		t.Fatal("worker did not begin reconciliation")
	}
	for range 100 {
		controller.Observe(testChat, "glm-5.3")
	}
	// A different chat forms a queue barrier: seeing its inspection proves all
	// preceding duplicate observations had a chance to run before cancellation.
	controller.Observe(controllerOtherChat, "glm-5.3")
	close(release)
	for {
		select {
		case id := <-desktop.inspected:
			if id == controllerOtherChat {
				stopControllerWorker(t, cancel, done)
				desktop.mu.Lock()
				inspections, updates := desktop.inspections[testChat], len(desktop.updates)
				desktop.mu.Unlock()
				if inspections != 2 || updates != 1 {
					t.Fatalf("duplicate observations caused repeated work: inspections=%d updates=%d", inspections, updates)
				}
				if _, err := receipts.Load(controllerOtherChat); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("queue barrier mutated unrelated chat: %v", err)
				}
				return
			}
		case <-time.After(2 * time.Second):
			t.Fatal("worker did not reach queue barrier")
		}
	}
}

func TestControllerRunCancellationInterruptsInFlightInspection(t *testing.T) {
	controller, desktop, _, receipts := controllerWorkerFixture()
	desktop.gateChat, desktop.gate = testChat, make(chan struct{})
	controller.Observe(testChat, "glm-5.3")
	cancel, done := runControllerWorker(t, controller)
	select {
	case <-desktop.inspected:
	case <-time.After(2 * time.Second):
		t.Fatal("worker did not begin inspection")
	}
	stopControllerWorker(t, cancel, done)
	desktop.mu.Lock()
	updates := len(desktop.updates)
	desktop.mu.Unlock()
	if updates != 0 {
		t.Fatal("cancelled inspection updated Desktop")
	}
	if _, err := receipts.Load(testChat); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("cancelled inspection saved a receipt: %v", err)
	}
}
