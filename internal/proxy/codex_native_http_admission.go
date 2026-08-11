package proxy

import (
	"errors"
	"sync/atomic"
	"time"
)

// codexNativeHTTPAdmissionObservation contains only durable journal-owned
// generations. It cannot carry route, account, request, response, credential,
// header, body, or caller-supplied gate material.
type codexNativeHTTPAdmissionObservation struct {
	RequestGeneration uint64
	AttemptGeneration uint64
}

// The unexported method keeps implementations package-owned.
type codexNativeHTTPAdmissionSink interface {
	observeCodexNativeHTTPFirstAdmission(codexNativeHTTPAdmissionObservation) error
}

type codexNativeHTTPAdmissionOwner struct {
	sink    codexNativeHTTPAdmissionSink
	blocked atomic.Bool
}

type codexCanaryNativeHTTPAdmissionSink struct {
	recorder *CodexCanaryRecorder
}

func (sink codexCanaryNativeHTTPAdmissionSink) observeCodexNativeHTTPFirstAdmission(codexNativeHTTPAdmissionObservation) error {
	if sink.recorder == nil {
		return errors.New("Codex canary admission authority unavailable")
	}
	return sink.recorder.RecordAdmitted(time.Now())
}

type codexInstalledNativeHTTPAdmissionCounter struct {
	firstAuthoritative atomic.Uint64
}

type codexInstalledNativeHTTPAdmissionSnapshot struct {
	FirstAuthoritative uint64
	PromotionBlocked   bool
}

type codexInstalledNativeHTTPAdmissionAuthority interface {
	nativeHTTPAdmissionSnapshot() codexInstalledNativeHTTPAdmissionSnapshot
}

func (counter *codexInstalledNativeHTTPAdmissionCounter) observeCodexNativeHTTPFirstAdmission(codexNativeHTTPAdmissionObservation) error {
	if counter == nil {
		return errCodexInstalledListenerAcceptance
	}
	counter.firstAuthoritative.Add(1)
	return nil
}

func (counter *codexInstalledNativeHTTPAdmissionCounter) snapshot() uint64 {
	if counter == nil {
		return 0
	}
	return counter.firstAuthoritative.Load()
}

func newCodexNativeHTTPAdmissionOwner(sink codexNativeHTTPAdmissionSink) *codexNativeHTTPAdmissionOwner {
	if sink == nil {
		return nil
	}
	return &codexNativeHTTPAdmissionOwner{sink: sink}
}

func (owner *codexNativeHTTPAdmissionOwner) observe(observation codexNativeHTTPAdmissionObservation) {
	if owner == nil || owner.sink == nil {
		return
	}
	func() {
		defer func() {
			if recover() != nil {
				owner.blocked.Store(true)
			}
		}()
		if err := owner.sink.observeCodexNativeHTTPFirstAdmission(observation); err != nil {
			owner.blocked.Store(true)
		}
	}()
}

func (owner *codexNativeHTTPAdmissionOwner) promotionBlocked() bool {
	return owner != nil && owner.blocked.Load()
}
