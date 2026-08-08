package proxy

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	codex "github.com/jacobcxdev/cq/internal/provider/codex"
)

type CodexHTTPEnforcer struct {
	Router   *CodexRequestRouter
	Leases   *CodexTurnLeaseManager
	Store    *CodexLeaseStore
	Observer *CodexTurnObserver
	Canary   *CodexCanaryRecorder
}

func NewCodexHTTPEnforcer(router *CodexRequestRouter, modeEpoch uint64, store *CodexLeaseStore) (*CodexHTTPEnforcer, error) {
	if router == nil || store == nil || modeEpoch == 0 {
		return nil, errors.New("Codex HTTP enforcement dependencies unavailable")
	}
	leases := NewCodexTurnLeaseManager(modeEpoch, true, nil)
	observer, err := NewCodexTurnObserver(leases, store)
	if err != nil {
		return nil, err
	}
	return &CodexHTTPEnforcer{Router: router, Leases: leases, Store: store, Observer: observer}, nil
}

func (enforcer *CodexHTTPEnforcer) Parse(body []byte, header http.Header) (CodexProtocolRequest, bool, error) {
	if enforcer == nil {
		return CodexProtocolRequest{}, false, nil
	}
	signalled := header.Get(codexTurnMetadataKey) != "" || bytes.Contains(body, []byte(codexTurnMetadataKey))
	decoded, err := DecodeCodexRequest(body, header.Get("Content-Encoding"), DefaultCodexZstdLimits)
	if err != nil {
		if signalled {
			return CodexProtocolRequest{}, false, err
		}
		return CodexProtocolRequest{}, false, nil
	}
	request, err := ParseCodexProtocolRequest(decoded.Decoded(), header.Get(codexTurnMetadataKey), nil)
	if err != nil {
		if signalled {
			return CodexProtocolRequest{}, false, err
		}
		return CodexProtocolRequest{}, false, nil
	}
	if !request.Metadata.Found || !request.Metadata.Strong {
		return request, false, nil
	}
	metadata := request.Metadata.Metadata
	if metadata.RequestKind == CodexRequestPrewarm {
		return CodexProtocolRequest{}, false, fmt.Errorf("%w: HTTP prewarm has no live WebSocket lineage", ErrCodexContinuity)
	}
	request.TurnState, request.HasTurnState, err = ParseCodexTurnStateHeader(header)
	if err != nil {
		return CodexProtocolRequest{}, false, err
	}
	return request, metadata.TurnID != "" && (metadata.RequestKind == CodexRequestTurn || metadata.RequestKind == CodexRequestCompaction), nil
}

func (enforcer *CodexHTTPEnforcer) Do(ctx context.Context, requirements CodexRouteRequirements, request CodexProtocolRequest, upstream *http.Request) (*http.Response, RouteChoice, CandidateAttempt, error) {
	if enforcer == nil || enforcer.Router == nil || enforcer.Leases == nil {
		return nil, RouteChoice{}, CandidateAttempt{}, errors.New("Codex HTTP enforcer unavailable")
	}
	enforcer.Observer.requests.Add(1)
	enforcer.Observer.strongKeys.Add(1)
	ctx = withCodexObservation(ctx, enforcer.Observer)
	key := NewCodexLeaseKey(request.Metadata.Metadata)
	if request.Metadata.Metadata.RequestKind == CodexRequestCompaction && request.Metadata.Metadata.CompactionPhase == CodexCompactionPreTurn {
		requirements.RequiredModels = append(requirements.RequiredModels, "gpt-5.4", codexSparkModel)
	}
	if err := enforcer.restore(ctx, key); err != nil {
		return nil, RouteChoice{}, CandidateAttempt{}, err
	}
	if request.Metadata.Metadata.RequestKind == CodexRequestCompaction &&
		request.Metadata.Metadata.CompactionPhase != CodexCompactionStandalone &&
		request.Metadata.Metadata.CompactionPhase != CodexCompactionPreTurn &&
		request.Metadata.Metadata.CompactionPhase != CodexCompactionMidTurn {
		if _, found := enforcer.Leases.Get(key); !found {
			return nil, RouteChoice{}, CandidateAttempt{}, fmt.Errorf("%w: compaction phase cannot establish a new lease", ErrCodexContinuity)
		}
	}
	var excluded []codex.SelectionExclusion
	lease, err := enforcer.Leases.AcquireRoute(ctx, key, func(ctx context.Context) (RouteChoice, error) {
		plan, err := enforcer.Router.Plan(ctx, requirements, "")
		return plan.Choice, err
	})
	if err != nil {
		return nil, RouteChoice{}, CandidateAttempt{}, err
	}
	choice := lease.Choice
	if choice.AccountKey == "" {
		choice = RouteChoice{AccountKey: lease.AccountKey, RequestedModel: requirements.RequestedModel, EffectiveModel: requirements.RequestedModel}
	} else if choice.RequestedModel == "" {
		choice.RequestedModel = requirements.RequestedModel
		choice.EffectiveModel = requirements.RequestedModel
	}
	if lease.TurnStateUnavailable {
		_ = enforcer.Leases.ReleaseRouting(key)
		return nil, choice, CandidateAttempt{}, fmt.Errorf("%w: persisted turn state unavailable after restart", ErrCodexContinuity)
	}
	if lease.TurnState != "" {
		if request.HasTurnState && request.TurnState != lease.TurnState {
			_ = enforcer.Leases.ReleaseRouting(key)
			return nil, choice, CandidateAttempt{}, fmt.Errorf("%w: request turn state mismatch", ErrCodexContinuity)
		}
		upstream.Header.Set("x-codex-turn-state", lease.TurnState)
	} else if request.HasTurnState {
		_ = enforcer.Leases.ReleaseRouting(key)
		return nil, choice, CandidateAttempt{}, fmt.Errorf("%w: unexpected request turn state", ErrCodexContinuity)
	}
	if err := lease.CheckContinuation(choice.AccountKey, 0, request.PreviousResponseID, request.HasEncryptedState); err != nil {
		_ = enforcer.Leases.ReleaseRouting(key)
		return nil, choice, CandidateAttempt{}, err
	}
	mutable := lease.State == LeaseProvisional
	for {
		response, attempt, failure, err := enforcer.Router.DoPinned(ctx, choice, upstream)
		if err != nil {
			enforcer.finishUnadmitted(key)
			return nil, choice, attempt, err
		}
		if failure == CodexPinnedAccepted {
			if err := enforcer.admitOrFinish(ctx, key, choice, response, request); err != nil {
				closeResponse(response)
				return nil, choice, attempt, err
			}
			return response, choice, attempt, nil
		}
		if !mutable {
			enforcer.finishUnadmitted(key)
			if response != nil {
				return response, choice, attempt, nil
			}
			return nil, choice, attempt, errors.New("bound Codex account authentication failed")
		}
		excluded = append(excluded, codex.SelectionExclusion{AccountKey: choice.AccountKey})
		plan, selectErr := enforcer.Router.Plan(ctx, requirements, "", excluded...)
		if selectErr != nil {
			enforcer.finishUnadmitted(key)
			if response != nil {
				return response, choice, attempt, nil
			}
			return nil, choice, attempt, errors.New("Codex authentication failed and no alternate account is available")
		}
		if response != nil {
			closeResponse(response)
		}
		lease, err = enforcer.Leases.ReplaceProvisionalRoute(key, plan.Choice)
		if err != nil {
			enforcer.finishUnadmitted(key)
			return nil, choice, attempt, err
		}
		choice = lease.Choice
		enforcer.Observer.failovers.Add(1)
		noteRouteAccount(ctx, redactedAccountHint("codex", string(choice.AccountKey)), true)
	}
}

func (enforcer *CodexHTTPEnforcer) admitOrFinish(ctx context.Context, key LeaseKey, choice RouteChoice, response *http.Response, request CodexProtocolRequest) error {
	if response == nil {
		enforcer.finishUnadmitted(key)
		return errors.New("Codex HTTP attempt returned no response")
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		enforcer.finishUnadmitted(key)
		return nil
	}
	if state, found, err := ParseCodexTurnStateHeader(response.Header); err != nil {
		enforcer.finishUnadmitted(key)
		return err
	} else if found {
		if err := enforcer.Leases.SetTurnState(key, state); err != nil {
			enforcer.finishUnadmitted(key)
			return err
		}
	}
	_, err := enforcer.Leases.Admit(key, choice.AccountKey, 0, enforcer.persist)
	_ = enforcer.Leases.ReleaseRouting(key)
	if err != nil {
		return err
	}
	if enforcer.Canary != nil {
		if err := enforcer.Canary.RecordAdmitted(time.Now()); err != nil {
			return fmt.Errorf("record Codex canary admission: %w", err)
		}
	}
	handle := &CodexTurnObservation{
		observer:        enforcer.Observer,
		ctx:             ctx,
		request:         request,
		key:             key,
		choice:          choice,
		leaseAcquired:   true,
		routingReleased: true,
		admitted:        true,
		parser:          NewCodexSSEParser(codexSSEDefaultMaxEventBytes),
	}
	handle.ResponseHeaders(response.StatusCode, response.Header)
	observeCodexResponseBody(response, handle)
	return nil
}

func (enforcer *CodexHTTPEnforcer) finishUnadmitted(key LeaseKey) {
	_ = enforcer.Leases.FailUnadmitted(key)
	_ = enforcer.Leases.ReleaseRouting(key)
}

func (enforcer *CodexHTTPEnforcer) persist(leases []CodexTurnLease) error {
	return enforcer.Store.CommitCurrentLeases(leases)
}

func (enforcer *CodexHTTPEnforcer) restore(ctx context.Context, key LeaseKey) error {
	if _, found := enforcer.Leases.Get(key); found {
		return nil
	}
	accounts, err := enforcer.Router.AccountKeys(ctx)
	if err != nil {
		return err
	}
	modeEpoch, authoritative := enforcer.Leases.Mode()
	record, account, found := enforcer.Store.LookupMode(key, accounts, modeEpoch, authoritative)
	if !found {
		return nil
	}
	lease := CodexTurnLease{
		Key:                  key,
		State:                record.State,
		AccountKey:           account,
		Choice:               RouteChoice{AccountKey: account},
		Generation:           record.LeaseGeneration,
		ModeEpoch:            record.ModeEpoch,
		Authoritative:        record.Authoritative,
		HasEncryptedState:    record.HasEncryptedState,
		TurnStateUnavailable: record.HasTurnState,
		NonMigratable:        record.NonMigratable || account == "",
		LastSeen:             record.LastSeen,
	}
	enforcer.Leases.Restore([]CodexTurnLease{lease})
	if account == "" {
		return fmt.Errorf("%w: persisted account no longer exists", ErrCodexContinuity)
	}
	return nil
}
