package proxy

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestResetSchedulerRequiresActivationAndCountsUpgradeWork(t *testing.T) {
	var calls atomic.Int32
	started := make(chan struct{})
	release := make(chan struct{})
	scheduler := NewCodexResetScheduler(func(ctx context.Context) error {
		if calls.Add(1) == 1 {
			close(started)
		}
		select {
		case <-release:
		case <-ctx.Done():
		}
		return nil
	}, time.Now)
	gate := NewRuntimeUpgradeAdmission()
	if scheduler.Status().Running || calls.Load() != 0 {
		t.Fatal("inactive scheduler ran")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := scheduler.Activate(ctx, gate); err != nil {
		t.Fatal(err)
	}
	defer scheduler.Stop()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("active scheduler did not start")
	}
	if !scheduler.Status().Running {
		t.Fatal("fresh executor missing")
	}
	if err := gate.Pause(ctx); err != nil {
		t.Fatal(err)
	}
	probe, stop := context.WithTimeout(ctx, 20*time.Millisecond)
	defer stop()
	if !errors.Is(gate.AwaitQuiescence(probe), context.DeadlineExceeded) {
		t.Fatal("upgrade ignored running reset")
	}
	close(release)
	probe2, stop2 := context.WithTimeout(ctx, time.Second)
	defer stop2()
	if err := gate.AwaitQuiescence(probe2); err != nil {
		t.Fatal(err)
	}
	scheduler.Stop()
	if scheduler.Status().Running {
		t.Fatal("stopped executor advertised running")
	}
	if err := scheduler.Activate(ctx, gate); err == nil {
		t.Fatal("drained scheduler restarted")
	}
}

func TestResetSchedulerPauseFencesPreflightClaimsAndDrainsClaimedWork(t *testing.T) {
	for _, pauseBeforeClaim := range []bool{true, false} {
		name := "after_claim"
		if pauseBeforeClaim {
			name = "during_preflight"
		}
		t.Run(name, func(t *testing.T) {
			preflight := make(chan struct{})
			continueClaim := make(chan struct{})
			claimed := make(chan struct{})
			complete := make(chan struct{})
			finished := make(chan error, 1)
			var scheduler *CodexResetScheduler
			scheduler = NewCodexResetScheduler(func(ctx context.Context) error {
				close(preflight)
				select {
				case <-continueClaim:
				case <-ctx.Done():
					return ctx.Err()
				}
				err := scheduler.Claim(ctx, func() error {
					close(claimed)
					return nil
				})
				if err == nil {
					select {
					case <-complete:
					case <-ctx.Done():
						err = ctx.Err()
					}
				}
				finished <- err
				return err
			}, time.Now)
			gate := NewRuntimeUpgradeAdmission()
			if err := scheduler.Activate(context.Background(), gate); err != nil {
				t.Fatal(err)
			}
			defer scheduler.Stop()
			select {
			case <-preflight:
			case <-time.After(time.Second):
				t.Fatal("preflight did not start")
			}
			if pauseBeforeClaim {
				if err := gate.Pause(context.Background()); err != nil {
					t.Fatal(err)
				}
			}
			close(continueClaim)
			if !pauseBeforeClaim {
				select {
				case <-claimed:
				case <-time.After(time.Second):
					t.Fatal("active reset was not claimed")
				}
				if err := gate.Pause(context.Background()); err != nil {
					t.Fatal(err)
				}
				probe, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
				if err := gate.AwaitQuiescence(probe); !errors.Is(err, context.DeadlineExceeded) {
					t.Fatalf("claimed reset was not counted for drain: %v", err)
				}
				cancel()
				close(complete)
			}
			select {
			case err := <-finished:
				if pauseBeforeClaim && !errors.Is(err, ErrRuntimeUpgradePaused) || !pauseBeforeClaim && err != nil {
					t.Fatalf("reset step = %v", err)
				}
			case <-time.After(time.Second):
				t.Fatal("reset step did not finish")
			}
			if pauseBeforeClaim {
				select {
				case <-claimed:
					t.Fatal("preflight claimed after admissions paused")
				default:
				}
			}
			probe, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			if err := gate.AwaitQuiescence(probe); err != nil {
				t.Fatalf("finished reset prevented drain: %v", err)
			}
		})
	}
}

func TestResetSchedulerPauseWaitsForDurableClaim(t *testing.T) {
	scheduler := NewCodexResetScheduler(func(context.Context) error { return nil }, time.Now)
	gate := NewRuntimeUpgradeAdmission()
	if err := scheduler.Activate(context.Background(), gate); err != nil {
		t.Fatal(err)
	}
	defer scheduler.Stop()
	claimStarted := make(chan struct{})
	finishClaim := make(chan struct{})
	claimDone := make(chan error, 1)
	go func() {
		claimDone <- scheduler.Claim(context.Background(), func() error {
			close(claimStarted)
			<-finishClaim
			return nil
		})
	}()
	<-claimStarted
	pauseStarted := make(chan struct{})
	paused := make(chan error, 1)
	go func() {
		close(pauseStarted)
		paused <- gate.Pause(context.Background())
	}()
	<-pauseStarted
	select {
	case err := <-paused:
		close(finishClaim)
		t.Fatalf("pause completed before durable claim: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	close(finishClaim)
	if err := <-claimDone; err != nil {
		t.Fatal(err)
	}
	if err := <-paused; err != nil {
		t.Fatal(err)
	}
	if err := scheduler.Claim(context.Background(), func() error { t.Error("claimed while paused"); return nil }); !errors.Is(err, ErrRuntimeUpgradePaused) {
		t.Fatalf("paused claim = %v", err)
	}
}

func TestResetSchedulerControlRequiresLocalAuthority(t *testing.T) {
	for _, encoded := range []string{RuntimeCodexResetScheduleStatusPath, "/_cq/control/codex/resets/%73tatus"} {
		request := httptest.NewRequest(http.MethodPost, encoded, nil)
		if normalCallerPolicy(request) != normalCallerRouteLocal {
			t.Fatalf("status not local-only: %s", encoded)
		}
	}
	s := &Server{}
	response := httptest.NewRecorder()
	s.handleCodexResetScheduleStatus(response, httptest.NewRequest(http.MethodPost, RuntimeCodexResetScheduleStatusPath, nil))
	if response.Code != http.StatusServiceUnavailable {
		t.Fatal("missing executor advertised available")
	}
}
func TestResetSchedulerRecoversPanicWithoutClaimingAvailability(t *testing.T) {
	s := NewCodexResetScheduler(func(context.Context) error { panic("unsafe error detail") }, time.Now)
	if err := s.Activate(context.Background(), NewRuntimeUpgradeAdmission()); err != nil {
		t.Fatal(err)
	}
	defer s.Stop()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		status := s.Status()
		if status.LastError == "executor_panic" {
			if status.Running {
				t.Fatal("panic executor running")
			}
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("panic unreported")
}

// Selection is durable before background work starts; Boot alone stays inert.
func TestResetSchedulerActivationFollowsCheckpoint(t *testing.T) {
	events := []string{}
	worker := &backgroundTestWorker{runtimeTestWorker: runtimeTestWorker{holder: runtimeHolder("worker"), events: &events, bootAck: RuntimeBootAckV1{BackgroundWork: true}}}
	launcher := backgroundTestLauncher{worker: worker}
	supervisor, err := NewRuntimeSupervisor(&runtimeTestListener{}, runtimeHolder("supervisor"), launcher, &runtimeTestCheckpointStore{events: &events})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := supervisor.Boot(context.Background(), WorkerManifestV1{SchemaVersion: 1, WorkerArtifactDigest: "artifact"}); err != nil {
		t.Fatal(err)
	}
	if len(events) != 3 || events[0] != "boot:worker" || events[1] != "checkpoint:worker" || events[2] != "activate" {
		t.Fatalf("activation order: %v", events)
	}
}

type backgroundTestWorker struct{ runtimeTestWorker }

func (w *backgroundTestWorker) ActivateBackground(context.Context) error {
	*w.events = append(*w.events, "activate")
	return nil
}

type backgroundTestLauncher struct{ worker RuntimeWorkerProcess }

func (l backgroundTestLauncher) Launch(context.Context, WorkerManifestV1) (RuntimeWorkerProcess, error) {
	return l.worker, nil
}
