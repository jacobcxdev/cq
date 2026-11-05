package harness

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/jacobcxdev/cq/internal/fsutil"
)

type DesktopSettings interface {
	Inspect(context.Context, string) (Snapshot, error)
	ReadModel(context.Context, string) (Snapshot, error)
	UpdatePlugins(context.Context, Snapshot, []string) (bool, error)
}

type ReceiptStore interface {
	Load(string) (Receipt, error)
	Save(Receipt) error
	Acquire(string) (fsutil.ExclusiveLock, error)
}

type PluginInventory interface {
	Installed(context.Context, string) ([]string, error)
}

// Controller applies stock Desktop's per-chat plugin settings. The owning
// Desktop queues these for its next turn; request bodies and history stay intact.
type Controller struct {
	Policy       func() (Policy, error)
	Desktop      DesktopSettings
	Receipts     ReceiptStore
	Plugins      PluginInventory
	Report       func(string, error)
	queue        chan string
	queueMu      sync.Mutex
	queued       map[string]bool
	pollInterval time.Duration
}

func NewController(policy func() (Policy, error), desktop DesktopSettings, receipts ReceiptStore, plugins PluginInventory) *Controller {
	return &Controller{Policy: policy, Desktop: desktop, Receipts: receipts, Plugins: plugins, queue: make(chan string, 64), queued: make(map[string]bool)}
}

// Observe never waits for Desktop IPC or changes the in-flight model request.
func (controller *Controller) Observe(chatID, model string) {
	if !canonicalChatID(chatID) {
		return
	}
	if !strings.HasPrefix(strings.ToLower(model), "glm-") {
		if _, err := controller.Receipts.Load(chatID); err != nil {
			return
		}
	}
	controller.queueMu.Lock()
	defer controller.queueMu.Unlock()
	if controller.queued[chatID] {
		return
	}
	controller.queued[chatID] = true
	select {
	case controller.queue <- chatID:
	default:
		delete(controller.queued, chatID)
	}
}

// Run belongs to the existing proxy lifetime. Cheap model reads notice switches
// and policy changes even when a persistent OpenAI WebSocket carries later turns.
func (controller *Controller) Run(ctx context.Context) {
	defer func() {
		if recover() != nil && controller.Report != nil {
			controller.Report("", errors.New("harness controller panic"))
		}
	}()
	interval := controller.pollInterval
	if interval <= 0 {
		interval = 5 * time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	type watchedChat struct {
		model, owner, cwd string
		selection         Selection
	}
	watched := make(map[string]watchedChat)
	reconcile := func(id string) {
		policy, err := controller.Policy()
		if err != nil {
			controller.report(id, err)
			return
		}
		if !policy.Enabled {
			if _, err := controller.Receipts.Load(id); errors.Is(err, os.ErrNotExist) {
				return
			}
		}
		snapshot, err := controller.Reconcile(ctx, id, policy)
		if err != nil {
			controller.report(id, err)
			return
		}
		if snapshot.Model != "" {
			watched[id] = watchedChat{snapshot.Model, snapshot.OwnerID, snapshot.Cwd, policy.Resolve(snapshot.Model, snapshot.Cwd, id)}
		}
	}
	for {
		select {
		case <-ctx.Done():
			return
		case id := <-controller.queue:
			reconcile(id)
			controller.queueMu.Lock()
			delete(controller.queued, id)
			controller.queueMu.Unlock()
		case <-ticker.C:
			for id, previous := range watched {
				if ctx.Err() != nil {
					return
				}
				current, err := controller.Desktop.ReadModel(ctx, id)
				if err != nil {
					delete(watched, id)
					continue
				}
				policy, err := controller.Policy()
				if err != nil {
					controller.report(id, err)
					continue
				}
				if current.Model != previous.model || current.OwnerID != previous.owner || policy.Resolve(current.Model, previous.cwd, id) != previous.selection {
					reconcile(id)
				}
			}
		}
	}
}

func (controller *Controller) report(id string, err error) {
	if controller.Report != nil {
		controller.Report(id, err)
	}
}

// Reconcile holds the same lease as the launcher bridge. A live full bridge
// owns all capability changes for its chat, so the stock controller stays out.
func (controller *Controller) Reconcile(ctx context.Context, chatID string, policy Policy) (Snapshot, error) {
	lock, err := controller.Receipts.Acquire(chatID)
	if errors.Is(err, fsutil.ErrExclusiveLockHeld) {
		return Snapshot{}, nil
	}
	if err != nil {
		return Snapshot{}, err
	}
	defer lock.Close()
	previous, receiptErr := controller.Receipts.Load(chatID)
	if receiptErr != nil && !errors.Is(receiptErr, os.ErrNotExist) {
		return Snapshot{}, receiptErr
	}
	snapshot, err := controller.Desktop.Inspect(ctx, chatID)
	if err != nil {
		return Snapshot{}, err
	}
	if snapshot.ChatID != chatID || !snapshot.PluginBaselineKnown || !snapshot.ReasoningEffortKnown || snapshot.Model == "" {
		return Snapshot{}, fmt.Errorf("desktop settings baseline unavailable")
	}
	selection := policy.Resolve(snapshot.Model, snapshot.Cwd, chatID)
	minimal := selection.Managed && selection.Profile == Minimal
	if !minimal && errors.Is(receiptErr, os.ErrNotExist) {
		return snapshot, nil
	}
	receipt := previous
	if errors.Is(receiptErr, os.ErrNotExist) || previous.State == "restored" {
		if !minimal {
			return snapshot, nil
		}
		receipt = Receipt{Version: 1, ChatID: chatID, Mode: "plugins", BaselinePlugins: append([]string{}, snapshot.DisabledPluginIDs...), AppliedPlugins: append([]string{}, snapshot.DisabledPluginIDs...)}
	} else if previous.State == "diverged" {
		if !minimal {
			receipt.Profile, receipt.Source, receipt.State = selection.Profile, selection.Source, "restored"
			receipt.BaselinePlugins = append([]string{}, snapshot.DisabledPluginIDs...)
			receipt.AppliedPlugins, receipt.PendingPlugins = append([]string{}, snapshot.DisabledPluginIDs...), nil
			return snapshot, controller.Receipts.Save(receipt)
		}
		return snapshot, nil
	} else if !samePlugins(snapshot.DisabledPluginIDs, previous.AppliedPlugins) && !samePlugins(snapshot.DisabledPluginIDs, previous.BaselinePlugins) && !(previous.State == "prepared" && samePlugins(snapshot.DisabledPluginIDs, previous.PendingPlugins)) {
		receipt.State = "diverged"
		return snapshot, controller.Receipts.Save(receipt)
	}
	desired := append([]string{}, receipt.BaselinePlugins...)
	if minimal {
		installed, err := controller.Plugins.Installed(ctx, snapshot.Cwd)
		if err != nil {
			return snapshot, err
		}
		desired = append(desired, installed...)
	}
	sort.Strings(desired)
	desired = slices.Compact(desired)
	receipt.Mode, receipt.Profile, receipt.Source = "plugins", selection.Profile, selection.Source
	receipt.PendingPlugins, receipt.State = desired, "prepared"
	if err := controller.Receipts.Save(receipt); err != nil {
		return snapshot, err
	}
	if !samePlugins(snapshot.DisabledPluginIDs, desired) {
		// Inventory discovery can take time. Check the real baseline again before
		// replacing a whole list; Desktop has model/effort guards but no list CAS.
		fresh, err := controller.Desktop.Inspect(ctx, chatID)
		if err != nil {
			return snapshot, err
		}
		if fresh.OwnerID != snapshot.OwnerID || fresh.Model != snapshot.Model || !fresh.PluginBaselineKnown || !samePlugins(fresh.DisabledPluginIDs, snapshot.DisabledPluginIDs) {
			receipt.State = "diverged"
			return snapshot, controller.Receipts.Save(receipt)
		}
		applied, err := controller.Desktop.UpdatePlugins(ctx, fresh, desired)
		if err != nil {
			return snapshot, err
		}
		if !applied {
			return snapshot, fmt.Errorf("desktop declined harness settings condition")
		}
	}
	if minimal {
		receipt.State = "applied"
	} else {
		receipt.State = "restored"
	}
	receipt.AppliedPlugins, receipt.PendingPlugins = desired, nil
	return snapshot, controller.Receipts.Save(receipt)
}

func samePlugins(left, right []string) bool {
	left, right = append([]string{}, left...), append([]string{}, right...)
	sort.Strings(left)
	sort.Strings(right)
	return slices.Equal(slices.Compact(left), slices.Compact(right))
}
