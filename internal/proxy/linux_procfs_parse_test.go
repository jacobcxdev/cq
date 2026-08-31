package proxy

import (
	"strings"
	"testing"
)

func TestLinuxProcStatParsesStableIdentity(t *testing.T) {
	fields := []string{"S", "41"}
	for len(fields) < 19 {
		fields = append(fields, "0")
	}
	fields = append(fields, "987654")
	data := []byte("42 (cq worker ) name) " + strings.Join(fields, " ") + "\n")

	parent, start, err := parseLinuxProcStat(data, 42)
	if err != nil {
		t.Fatal(err)
	}
	if parent != 41 || start != 987654 {
		t.Fatalf("unexpected stat identity: parent=%d start=%d", parent, start)
	}
}

func TestLinuxProcStatRejectsMalformedOrMismatchedData(t *testing.T) {
	validFields := []string{"S", "41"}
	for len(validFields) < 19 {
		validFields = append(validFields, "0")
	}
	validFields = append(validFields, "987654")
	valid := "42 (cq) " + strings.Join(validFields, " ") + "\n"

	for name, data := range map[string]string{
		"pid":        strings.Replace(valid, "42 (", "43 (", 1),
		"truncated":  "42 (cq) S 41\n",
		"zero start": strings.Replace(valid, "987654", "0", 1),
		"trailing":   valid + "extra",
	} {
		t.Run(name, func(t *testing.T) {
			if _, _, err := parseLinuxProcStat([]byte(data), 42); err == nil {
				t.Fatal("malformed stat unexpectedly accepted")
			}
		})
	}
}

func TestLinuxProcStatusRequiresOneConsistentUID(t *testing.T) {
	uid, err := parseLinuxProcStatusUID([]byte("Name:\tcq\nUid:\t501\t501\t501\t501\nGid:\t20\t20\t20\t20\n"))
	if err != nil {
		t.Fatal(err)
	}
	if uid != 501 {
		t.Fatalf("unexpected uid %d", uid)
	}

	for name, data := range map[string]string{
		"missing":   "Name:\tcq\n",
		"duplicate": "Uid:\t501\t501\t501\t501\nUid:\t501\t501\t501\t501\n",
		"mismatch":  "Uid:\t501\t502\t501\t501\n",
		"overflow":  "Uid:\t18446744073709551616\t18446744073709551616\t18446744073709551616\t18446744073709551616\n",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := parseLinuxProcStatusUID([]byte(data)); err == nil {
				t.Fatal("unsafe uid data unexpectedly accepted")
			}
		})
	}
}

func TestLinuxProcCmdlineRequiresCanonicalArguments(t *testing.T) {
	arguments, err := parseLinuxProcCmdline([]byte("/usr/bin/cq\x00proxy\x00start\x00"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(arguments, " ") != "/usr/bin/cq proxy start" {
		t.Fatalf("unexpected arguments: %#v", arguments)
	}

	for name, data := range map[string][]byte{
		"missing terminator": []byte("/usr/bin/cq\x00proxy\x00start"),
		"empty argument":     []byte("/usr/bin/cq\x00\x00start\x00"),
		"empty":              nil,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := parseLinuxProcCmdline(data); err == nil {
				t.Fatal("unsafe command line unexpectedly accepted")
			}
		})
	}
}

func TestLinuxProcCgroupRequiresSingleV2Membership(t *testing.T) {
	path, err := parseLinuxProcCgroup([]byte("0::/user.slice/user-501.slice/user@501.service/app.slice/cq-proxy.service\n"))
	if err != nil {
		t.Fatal(err)
	}
	if path != "/user.slice/user-501.slice/user@501.service/app.slice/cq-proxy.service" {
		t.Fatalf("unexpected cgroup %q", path)
	}

	for name, data := range map[string]string{
		"legacy":    "2:cpu:/user.slice\n",
		"duplicate": "0::/one\n0::/two\n",
		"relative":  "0::relative\n",
		"traversal": "0::/user.slice/../system.slice\n",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := parseLinuxProcCgroup([]byte(data)); err == nil {
				t.Fatal("unsafe cgroup unexpectedly accepted")
			}
		})
	}
}

func TestLinuxProcTCPParsesLoopbackListener(t *testing.T) {
	data := []byte("  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode\n" +
		"   0: 0100007F:4B50 00000000:0000 0A 00000000:00000000 00:00000000 00000000  501        0 12345 1 0000000000000000 100 0 0 10 0\n")
	sockets, err := parseLinuxProcTCP(data, 19280, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(sockets) != 1 || sockets[0].Address != "127.0.0.1:19280" || sockets[0].Inode != 12345 || !sockets[0].LoopbackIPv4 {
		t.Fatalf("unexpected sockets: %#v", sockets)
	}
}

func TestLinuxProcTCPRejectsMalformedRows(t *testing.T) {
	header := "  sl  local_address rem_address st tx_queue rx_queue tr tm->when retrnsmt uid timeout inode\n"
	for name, row := range map[string]string{
		"short":     "0: 0100007F:4B50 00000000:0000 0A\n",
		"port":      "0: 0100007F:ZZZZ 00000000:0000 0A 0 0 0 501 0 1\n",
		"inode":     "0: 0100007F:4B50 00000000:0000 0A 0 0 0 501 0 nope\n",
		"duplicate": "0: 0100007F:4B50 00000000:0000 0A 0 0 0 501 0 1\n0: 0100007F:4B50 00000000:0000 0A 0 0 0 501 0 1\n",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := parseLinuxProcTCP([]byte(header+row), 19280, false); err == nil {
				t.Fatal("malformed TCP table unexpectedly accepted")
			}
		})
	}
}

func TestLinuxSocketInodeRequiresCanonicalLink(t *testing.T) {
	inode, ok := parseLinuxSocketInode("socket:[12345]")
	if !ok || inode != 12345 {
		t.Fatalf("unexpected socket inode: %d %t", inode, ok)
	}
	for _, value := range []string{"socket:12345", "socket:[0]", "socket:[123]extra", "pipe:[123]"} {
		if _, ok := parseLinuxSocketInode(value); ok {
			t.Fatalf("unsafe socket link %q accepted", value)
		}
	}
}
