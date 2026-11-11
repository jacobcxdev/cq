package proxy

import (
	"errors"
	"fmt"
	"reflect"
	"testing"

	codex "github.com/jacobcxdev/cq/internal/provider/codex"
)

func TestCodexProvisionalCountsFindCanonicalLaneEdges(t *testing.T) {
	for _, sharedSession := range []bool{false, true} {
		for _, position := range []int{0, 16, 32} {
			for _, current := range []bool{false, true} {
				store, envelope := codexProxyBenchmarkLargeJournal(t, 33)
				if sharedSession {
					for index := range envelope.Lanes {
						envelope.Lanes[index].SessionHash = store.hash("session", "shared-session")
						envelope.Records[index].SessionHash = envelope.Lanes[index].SessionHash
					}
				}
				canonicaliseCodexLeaseV2(&envelope)
				for index := range envelope.Records {
					envelope.Records[index].RoutingRefs = 0
				}
				record := &envelope.Records[position]
				record.RoutingRefs = 1
				if !current {
					envelope.Lanes[position].CurrentTurnHash = ""
				}
				store.v2 = &envelope
				store.generation = envelope.Generation
				store.modes = CodexModeAuthoritySnapshot{RecognisedAuthoritativeEpochs: []uint64{1}}
				store.owner = testCodexLeaseOwner{}
				accounts := []codex.AccountKey{"account-0", "account-1"}
				want := map[codex.AccountKey]int{}
				if current {
					for _, account := range accounts {
						if record.AccountHash == store.hash("account", string(account)) {
							want[account] = 1
						}
					}
				}
				got, matched, err := store.loadCodexLeaseProvisionalRouteCounts(envelope.Generation, accounts)
				if err != nil || !matched || !reflect.DeepEqual(got, want) {
					t.Fatalf("lane %d,current=%v: counts=%v,matched=%v,error=%v; want%v", position, current, got, matched, err, want)
				}
				if _, matched, err := store.loadCodexLeaseProvisionalRouteCounts(envelope.Generation+1, accounts); err != nil || matched {
					t.Fatalf("lane%d: stale generation accepted: matched=%v,error=%v", position, matched, err)
				}
			}
		}
	}
}

func TestCodexLoadLaneCanonicalScopeEdgesRetainHistory(t *testing.T) {
	for _, sharedSession := range []bool{false, true} {
		for _, position := range []int{0, 16, 32} {
			store, envelope := codexProxyBenchmarkLargeJournal(t, 33)
			keys := make(map[string]LeaseKey, 33)
			for index := range envelope.Lanes {
				session, thread, turn := fmt.Sprintf("session-%04d", index), fmt.Sprintf("thread-%04d", index), fmt.Sprintf("turn-%04d", index)
				if sharedSession {
					session = "shared-session"
					envelope.Lanes[index].SessionHash = store.hash("session", session)
					envelope.Records[index].SessionHash = envelope.Lanes[index].SessionHash
				}
				keys[envelope.Lanes[index].ThreadHash] = LeaseKey{Lane: LaneKey{Session: session, Thread: thread, Namespace: CodexResponsesNamespace}, Turn: turn}
			}
			canonicaliseCodexLeaseV2(&envelope)
			target := envelope.Lanes[position]
			key := keys[target.ThreadHash]
			for _, turn := range []string{"history-a", "history-b"} {
				history := cloneCodexJournalRecordV2(envelope.Records[position])
				history.TurnHash = store.hash("turn", turn)
				history.State = LeaseFailedUnadmitted
				history.AccountHash = ""
				history.CodexCurrentRequest = CodexCurrentRequest{}
				envelope.Records = append(envelope.Records, history)
			}
			canonicaliseCodexLeaseV2(&envelope)
			store.v2 = &envelope
			store.generation = envelope.Generation
			store.owner = testCodexLeaseOwner{}
			store.modes = CodexModeAuthoritySnapshot{RecognisedAuthoritativeEpochs: []uint64{1}}
			accounts := []codex.AccountKey{"account-0", "account-1"}
			policy := CodexLeaseAuthorityPolicy{ModeEpoch: 1, Authoritative: true}
			restored, err := store.loadLaneSnapshot(key, accounts, policy)
			if err != nil || restored.Classification != CodexRestoredLaneCurrent || restored.Fence.Current != restored.RequestedIdentity || len(restored.Records) != 3 || len(restored.ResolvedRecords) != 3 {
				t.Fatalf("scope%d,shared=%v lost current/history: %#v,error%v", position, sharedSession, restored, err)
			}
			for _, record := range restored.Records {
				if record.SessionHash != target.SessionHash || record.ThreadHash != target.ThreadHash || record.NamespaceHash != target.NamespaceHash {
					t.Fatal("scope leaked neighbouring history")
				}
			}
			missing := key
			missing.Lane.Thread = "absent-thread"
			absent, err := store.loadLaneSnapshot(missing, accounts, policy)
			if err != nil || absent.Classification != CodexRestoredLaneUnseen || len(absent.Records) != 0 {
				t.Fatalf("missing scope leaked authority: %#v,error%v", absent, err)
			}
			envelope.Lanes = append(envelope.Lanes, target)
			canonicaliseCodexLeaseV2(&envelope)
			if _, err := store.loadLaneSnapshot(key, accounts, policy); !errors.Is(err, ErrCodexLeaseTrustLost) {
				t.Fatalf("duplicate matching scope accepted: %v", err)
			}
		}
	}
}
