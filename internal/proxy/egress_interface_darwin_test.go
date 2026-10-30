package proxy

import (
	"errors"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

func TestDarwinInterfaceSocketControlUsesFamilyOptions(t *testing.T) {
	for _, test := range []struct {
		network string
		level   int
		option  int
	}{
		{"udp4", unix.IPPROTO_IP, unix.IP_BOUND_IF},
		{"udp6", unix.IPPROTO_IPV6, unix.IPV6_BOUND_IF},
	} {
		t.Run(test.network, func(t *testing.T) {
			sets, gets := 0, 0
			set := func(fd, level, option, index int) error {
				sets++
				if fd != 42 || level != test.level || option != test.option || index != 4 {
					t.Fatalf("wrong socket binding: fd=%d level=%d option=%d index=%d", fd, level, option, index)
				}
				return nil
			}
			get := func(fd, level, option int) (int, error) {
				gets++
				if sets != 1 || fd != 42 || level != test.level || option != test.option {
					t.Fatal("readback did not verify the binding")
				}
				return 4, nil
			}
			if err := darwinInterfaceSocketControl(4, set, get)(test.network, ":0", syntheticInterfaceRawConn{}); err != nil || sets != 1 || gets != 1 {
				t.Fatalf("control: sets=%d gets=%d err=%v", sets, gets, err)
			}
		})
	}
}

func TestDarwinInterfaceSocketControlFailsClosed(t *testing.T) {
	for _, test := range []string{"zero", "negative", "network", "raw", "set", "get", "readback"} {
		t.Run(test, func(t *testing.T) {
			index, network := 4, "udp4"
			raw := syntheticInterfaceRawConn{}
			set := func(int, int, int, int) error { return nil }
			get := func(int, int, int) (int, error) { return 4, nil }
			switch test {
			case "zero":
				index = 0
			case "negative":
				index = -1
			case "network":
				network = "udp"
			case "raw":
				raw.err = errors.New("sensitive raw error")
			case "set":
				set = func(int, int, int, int) error { return errors.New("sensitive set error") }
			case "get":
				get = func(int, int, int) (int, error) { return 0, errors.New("sensitive get error") }
			case "readback":
				get = func(int, int, int) (int, error) { return 0, nil }
			}
			err := darwinInterfaceSocketControl(index, set, get)(network, ":0", raw)
			if err == nil || strings.Contains(err.Error(), "sensitive") {
				t.Fatalf("unsafe control result: %v", err)
			}
		})
	}
}
