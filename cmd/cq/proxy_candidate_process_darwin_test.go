//go:build darwin

package main

import (
	"context"
	"errors"
	"io"
	"net"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/jacobcxdev/cq/internal/proxy"
	"golang.org/x/sys/unix"
)

// This hermetic child exercises native exit observation, not release startup
// qualification. Public candidate start remains unavailable in every fixture.
func TestCLIV2CandidateNativeChildStopThenRemove(t *testing.T) {
	a, d := candidateV2Inputs(t)
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	a.Port = listener.Addr().(*net.TCPAddr).Port
	_ = listener.Close()
	prepareCandidateV2Test(t, a, d)
	var command *exec.Cmd
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()
	store, _, err := proxy.OpenCandidateLifecycle(ctx, d.FS, a.InstanceStateRoot)
	if err != nil {
		t.Fatal(err)
	}
	token, err := store.RuntimeControlToken()
	if err != nil {
		t.Fatal(err)
	}
	defer zeroCandidateBytes(token)
	_, err = store.Apply(ctx, proxy.CandidateActionStart, func(state proxy.CandidateLifecycleStateV1) (string, error) {
		reader, writer, err := os.Pipe()
		if err != nil {
			return "", err
		}
		defer reader.Close()
		defer writer.Close()
		command = exec.Command(os.Args[0], append([]string{"-test.run=^TestCandidateRuntimeProcessHelper$", "--"}, candidateRuntimeArguments(state)...)...)
		command.Env = append(os.Environ(), "CQ_CANDIDATE_RUNTIME_HELPER=1")
		command.ExtraFiles = []*os.File{reader}
		if err = command.Start(); err != nil {
			return "", err
		}
		t.Cleanup(func() { _ = command.Process.Kill() })
		if _, err = writer.Write(token); err != nil {
			return "", err
		}
		_ = writer.Close()
		for {
			health, err := inspectCandidateRuntime(ctx, state.Port, token)
			if err == nil {
				return proxy.CandidateEffectReceiptDigest(proxy.CandidateActionStart, candidateRuntimeReceipt("running", health)), nil
			}
			select {
			case <-ctx.Done():
				return "", ctx.Err()
			case <-time.After(20 * time.Millisecond):
			}
		}
	})
	if closeErr := store.Close(); err != nil || closeErr != nil {
		t.Fatalf("fixture start=%v close=%v", err, closeErr)
	}
	done := make(chan error, 1)
	go func() {
		defer func() {
			if recover() != nil {
				done <- context.Canceled
			}
		}()
		done <- command.Wait()
	}()
	t.Cleanup(func() {
		_ = command.Process.Kill()
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Error("fixture child was not reaped")
		}
	})
	d.PrepareStop = func(ctx context.Context, state proxy.CandidateLifecycleStateV1, token []byte) (*candidateStopOperation, error) {
		op, err := prepareCandidateRuntimeStop(ctx, state, token)
		if err != nil {
			t.Logf("native admission: %v", err)
			return nil, err
		}
		run := op.Run
		op.Run = func(work, cleanup context.Context, state proxy.CandidateLifecycleStateV1) ([]byte, error) {
			body, err := run(work, cleanup, state)
			if err != nil {
				t.Logf("native effect: %v", err)
			}
			return body, err
		}
		return op, nil
	}
	d.Absent = candidateListenerAbsent
	exit, out := runCandidateV2(t, ctx, []string{"proxy", "candidate", "stop", "--state-dir", a.InstanceStateRoot, "--confirm-client-stopped", "--json"}, &d)
	if exit != 0 || decodeCandidateV2(t, out).Phase != proxy.CandidatePhaseStopped {
		t.Fatalf("native stop=%d %+v", exit, out)
	}
	// A fresh handler invocation independently authenticates the retained marker.
	fresh, _ := prepareV2CandidateDependencies(context.Background())
	exit, out = runCandidateV2(t, ctx, []string{"proxy", "candidate", "remove", "--state-dir", a.InstanceStateRoot, "--confirm-candidate-state-loss", "--json"}, &fresh)
	if exit != 0 || decodeCandidateV2(t, out).Phase != proxy.CandidatePhaseRemoved {
		t.Fatalf("native stopped removal=%d %+v", exit, out)
	}
}

func TestCandidateRuntimeDarwinListenerRecords(t *testing.T) {
	for _, tc := range []struct {
		body string
		want int
	}{
		{"p123\nf3\n", 123}, {"p123\nf3\nf4\n", 123}, {"p123\np123\nf3\n", 123},
		{"", 0}, {"p123\np124\nf3\n", 0}, {"f3\np123\n", 0}, {"p123\nfgarbage\n", 0}, {"p123\fn-1\n", 0}, {"p123\nn127.0.0.1\n", 0},
	} {
		t.Run(tc.body, func(t *testing.T) {
			pid, err := parseCandidateDarwinListenerPID(tc.body)
			if tc.want == 0 {
				if err == nil {
					t.Fatalf("accepted %q", tc.body)
				}
			} else if err != nil || pid != tc.want {
				t.Fatalf("pid=%d err=%v", pid, err)
			}
		})
	}
}

func TestCandidateRuntimeDarwinExitObservation(t *testing.T) {
	old := candidateDarwinProcesses
	defer func() { candidateDarwinProcesses = old }()
	p := &darwinCandidateProcess{pid: 123, started: unix.Timeval{Sec: 1}}
	live := unix.KinfoProc{}
	live.Proc.P_pid = 123
	live.Proc.P_starttime = p.started
	live.Eproc.Ucred.Uid = uint32(os.Geteuid())
	reused := live
	reused.Proc.P_starttime.Sec++
	for _, tc := range []struct {
		name    string
		items   []unix.KinfoProc
		err     error
		want    bool
		wantErr bool
	}{
		{"absent", nil, nil, true, false}, {"live", []unix.KinfoProc{live}, nil, false, false}, {"reused", []unix.KinfoProc{reused}, nil, false, true},
		{"denied", nil, unix.EPERM, false, true}, {"io_error", nil, unix.EIO, false, true}, {"timeout", nil, context.DeadlineExceeded, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			candidateDarwinProcesses = func(string, ...int) ([]unix.KinfoProc, error) { return tc.items, tc.err }
			got, err := p.Exited(context.Background())
			if got != tc.want || (err != nil) != tc.wantErr {
				t.Fatalf("exit=%t error=%v", got, err)
			}
			if tc.err != nil && !errors.Is(err, tc.err) {
				t.Fatalf("lost observation error %v", err)
			}
		})
	}
}

func TestCandidateRuntimeDarwinOutputIsBounded(t *testing.T) {
	var output candidateDarwinOutput
	_, err := io.Copy(&output, io.LimitReader(strings.NewReader(strings.Repeat("x", 128<<10)), 128<<10))
	if err == nil || len(output.String()) > 64<<10 {
		t.Fatalf("size=%d err=%v", len(output.String()), err)
	}
}
