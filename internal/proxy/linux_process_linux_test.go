//go:build linux

package proxy

import (
	"fmt"
	"net"
	"os"
	"strings"
	"testing"
)

func TestLinuxProcCapturesLiveProcessAndListener(t *testing.T) {
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().(*net.TCPAddr)

	process, err := CaptureLinuxProcess(os.Getpid())
	if err != nil {
		listener.Close()
		t.Fatal(err)
	}
	if process.PID != os.Getpid() || process.UID != uint64(os.Geteuid()) || !process.Valid() {
		listener.Close()
		t.Fatalf("unexpected process identity: %+v", process)
	}
	identity, err := CaptureLinuxListener(os.Getpid(), address.Port)
	if err != nil {
		listener.Close()
		t.Fatal(err)
	}
	if identity.Address != listener.Addr().String() || !identity.Valid() {
		listener.Close()
		t.Fatalf("unexpected listener identity: %+v", identity)
	}
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	if err := RevalidateLinuxListener(identity); err == nil {
		t.Fatal("closed listener unexpectedly revalidated")
	}
}

func TestLinuxProcRejectsProcessStartTimeChange(t *testing.T) {
	statCalls := 0
	executable := LinuxExecutableIdentity{
		Path: "/usr/bin/cq", Device: 1, Inode: 2, Links: 1, Owner: 501, Size: 4,
		Mode: 0o100755, SHA256: [32]byte{1},
	}
	operations := linuxProcOperations{
		readFile: func(path string, _ int64) ([]byte, error) {
			switch {
			case strings.HasSuffix(path, "/stat"):
				statCalls++
				start := 100
				if statCalls == 2 {
					start = 101
				}
				return linuxTestStat(42, 1, start), nil
			case strings.HasSuffix(path, "/status"):
				return []byte("Uid:\t501\t501\t501\t501\n"), nil
			case strings.HasSuffix(path, "/cmdline"):
				return []byte("/usr/bin/cq\x00proxy\x00start\x00"), nil
			case strings.HasSuffix(path, "/cgroup"):
				return []byte("0::/user.slice/cq-proxy.service\n"), nil
			default:
				return nil, os.ErrNotExist
			}
		},
		captureExecutable: func(string) (LinuxExecutableIdentity, error) { return executable, nil },
	}
	if _, err := captureLinuxProcessWithOperations(42, operations); err == nil {
		t.Fatal("reused pid unexpectedly accepted")
	}
}

func TestLinuxProcessMatchFactsRejectStartTimeChangeBeforeExecutableCapture(t *testing.T) {
	statCalls := 0
	operations := linuxProcOperations{readFile: func(path string, _ int64) ([]byte, error) {
		switch {
		case strings.HasSuffix(path, "/stat"):
			statCalls++
			return linuxTestStat(42, 1, 100+statCalls-1), nil
		case strings.HasSuffix(path, "/status"):
			return []byte("Uid:\t501\t501\t501\t501\n"), nil
		case strings.HasSuffix(path, "/cmdline"):
			return []byte("/usr/bin/cq\x00proxy\x00start\x00"), nil
		case strings.HasSuffix(path, "/cgroup"):
			return []byte("0::/user.slice/cq-proxy.service\n"), nil
		default:
			return nil, os.ErrNotExist
		}
	}}
	if _, err := captureLinuxProcessMatchFacts(42, operations); err == nil {
		t.Fatal("reused PID metadata unexpectedly passed prefilter")
	}
}

func linuxTestStat(pid, parent, start int) []byte {
	fields := []string{"S", fmt.Sprint(parent)}
	for len(fields) < 19 {
		fields = append(fields, "0")
	}
	fields = append(fields, fmt.Sprint(start))
	return []byte(fmt.Sprintf("%d (cq) %s\n", pid, strings.Join(fields, " ")))
}
