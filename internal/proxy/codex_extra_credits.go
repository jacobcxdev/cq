package proxy

import (
	"context"
	"math"
	"strings"
	"time"

	"github.com/gorilla/websocket"
	codex "github.com/jacobcxdev/cq/internal/provider/codex"
	"github.com/jacobcxdev/cq/internal/quota"
)

// CodexExtraCreditsGuard checks included allowance independently of ordinary
// capacity, which intentionally permits paid fallback. Policy is read on every
// dispatch so a toggle also applies to retries and already-open connections.
type CodexExtraCreditsGuard struct {
	Policy interface {
		Allowed(codex.AccountKey) (bool, error)
	}
	Usage     CodexPrimerUsageReaderAPI
	Now       func() time.Time
	Inventory codex.CredentialInventory
}

type codexExtraCreditsLimitError struct {
	*codexReserveLimitError
}

func (e *codexExtraCreditsLimitError) Unwrap() error { return e.codexReserveLimitError }

func (*codexExtraCreditsLimitError) Error() string {
	return "Codex extra credits disabled; fresh included allowance required"
}

// Check never performs inference. Its usage reader must use the exempt GET
// path, not another inference admission, to avoid recursion.
func (g *CodexExtraCreditsGuard) Check(ctx context.Context, choice RouteChoice) error {
	if g == nil {
		return nil
	}
	blocked := func() error { return &codexExtraCreditsLimitError{&codexReserveLimitError{&CachedUsageLimitError{}}} }
	if g.Policy == nil {
		return blocked()
	}
	policyKey := choice.AccountKey
	if g.Inventory != nil {
		inventory, err := g.Inventory.List(ctx)
		if err != nil {
			return blocked()
		}
		found := false
		for _, account := range inventory.Accounts {
			if account.Key != choice.AccountKey {
				continue
			}
			if found || account.Unstable {
				return blocked()
			}
			policyKey, err = codex.ExtraCreditsIdentityKey(account.Identity)
			if err != nil {
				return blocked()
			}
			found = true
		}
		if !found {
			return blocked()
		}
	}
	allowed, err := g.Policy.Allowed(policyKey)
	if err != nil {
		return blocked()
	}
	if allowed {
		return nil
	}
	if g.Usage == nil || choice.AccountKey == "" {
		return blocked()
	}
	observation, err := g.Usage.Read(ctx, choice.AccountKey)
	if err != nil || !observation.IncludedUsageValid || !observation.Result.IsUsable() || observation.Result.CacheAge != 0 ||
		observation.IncludedUsageAllowed == nil || !*observation.IncludedUsageAllowed ||
		(observation.LimitReached != nil && *observation.LimitReached) {
		return blocked()
	}
	now := time.Now()
	if g.Now != nil {
		now = g.Now()
	}
	model := strings.ToLower(ParseModel(choice.EffectiveModel))
	if model == "" {
		model = strings.ToLower(ParseModel(choice.RequestedModel))
	}
	if model == "" {
		return blocked()
	}
	for scope, usage := range observation.ModelUsage {
		if strings.ToLower(ParseModel(scope)) != model && (CapacityBucketForModel(scope) == CapacityBucketBase || CapacityBucketForModel(scope) != CapacityBucketForModel(model)) {
			continue
		}
		if usage.CreditsWouldEnable || (usage.Available != nil && !*usage.Available) ||
			(usage.IncludedUsageAllowed != nil && !*usage.IncludedUsageAllowed) || (usage.LimitReached != nil && *usage.LimitReached) {
			return blocked()
		}
	}
	shared := false
	for name, window := range observation.Result.Windows {
		bucket := quota.WindowBucket(name)
		if bucket != "" && strings.ToLower(ParseModel(bucket)) != model && (CapacityBucketForModel(bucket) == CapacityBucketBase || CapacityBucketForModel(bucket) != CapacityBucketForModel(model)) {
			continue
		}
		if bucket == "" {
			shared = true
		}
		remaining := windowRemaining(window)
		if math.IsNaN(remaining) || math.IsInf(remaining, 0) || remaining <= 0 || remaining > 100 || window.ResetAtUnix <= now.Unix() {
			return blocked()
		}
	}
	if !shared {
		return blocked()
	}
	return nil
}

func (e *CodexAttemptExecutor) admissionError(ctx context.Context, choice RouteChoice) error {
	if e == nil {
		return nil
	}
	if err := reserveDispatchError(e.Reserve, choice.AccountKey); err != nil {
		return err
	}
	return e.ExtraCredits.Check(ctx, choice)
}

func (e *CodexWebSocketAttemptExecutor) extraCreditsDispatchError(ctx context.Context, choice RouteChoice) error {
	if e == nil {
		return nil
	}
	return e.ExtraCredits.Check(ctx, choice)
}

func (dialer codexExplicitWSUpstreamDialer) extraCreditsDispatchError(ctx context.Context, choice RouteChoice) error {
	if checker, ok := dialer.executor.(interface {
		extraCreditsDispatchError(context.Context, RouteChoice) error
	}); ok {
		return checker.extraCreditsDispatchError(ctx, choice)
	}
	return nil
}

func (broker *codexTerminatingWSBroker) extraCreditsDispatchError(ctx context.Context, choice RouteChoice) error {
	if checker, ok := broker.config.Upstream.(interface {
		extraCreditsDispatchError(context.Context, RouteChoice) error
	}); ok {
		return checker.extraCreditsDispatchError(ctx, choice)
	}
	return nil
}

// Legacy relay frames may override the connection model at top level or in
// params. Recheck the actual frame model before forwarding on an open socket.
func codexExtraCreditsFrameGuard(ctx context.Context, capacity *CodexCapacityLedger, choice RouteChoice) func(int, []byte) error {
	return func(messageType int, frame []byte) error {
		if capacity == nil {
			return nil
		}
		if err := reserveDispatchError(capacity.Reserve, choice.AccountKey); err != nil {
			return err
		}
		frameChoice := choice
		frameChoice.EffectiveModel = ""
		frameChoice.RequestedModel = ""
		if messageType == websocket.TextMessage || messageType == websocket.BinaryMessage {
			frameChoice.EffectiveModel = extractCodexWebSocketFrameModel(frame)
		}
		return capacity.ExtraCredits.Check(ctx, frameChoice)
	}
}
