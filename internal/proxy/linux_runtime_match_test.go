package proxy

import "testing"

func TestLinuxProxyRuntimeRequiresExactCommandAndCgroup(t *testing.T) {
	uid := uint64(501)
	validCgroup := "/user.slice/user-501.slice/user@501.service/app.slice/cq-proxy.service"
	if !linuxProxyRuntimeArgumentsMatch([]string{"/home/test/bin/cq", "proxy", "start"}, "/home/test/bin/cq") {
		t.Fatal("exact proxy command rejected")
	}
	if !linuxProxyRuntimeCgroupMatches(validCgroup, uid) {
		t.Fatal("exact systemd user-service cgroup rejected")
	}

	for name, arguments := range map[string][]string{
		"shell":      {"/bin/sh", "-c", "/home/test/bin/cq proxy start"},
		"extra":      {"/home/test/bin/cq", "proxy", "start", "--port", "19280"},
		"near path":  {"/home/test/bin/cq-old", "proxy", "start"},
		"wrong verb": {"/home/test/bin/cq", "proxy", "status"},
	} {
		t.Run(name, func(t *testing.T) {
			if linuxProxyRuntimeArgumentsMatch(arguments, "/home/test/bin/cq") {
				t.Fatal("near-match command unexpectedly accepted")
			}
		})
	}

	for name, cgroup := range map[string]string{
		"system":    "/system.slice/cq-proxy.service",
		"wrong uid": "/user.slice/user-502.slice/user@502.service/app.slice/cq-proxy.service",
		"suffix":    "/user.slice/user-501.slice/user@501.service/app.slice/cq-proxy.service.old",
		"nested":    "/user.slice/user-501.slice/user@501.service/app.slice/cq-proxy.service/child",
	} {
		t.Run(name, func(t *testing.T) {
			if linuxProxyRuntimeCgroupMatches(cgroup, uid) {
				t.Fatal("near-match cgroup unexpectedly accepted")
			}
		})
	}
}

func TestSystemdUserInstalledProofIsPersistent(t *testing.T) {
	proof := codexInstalledProcessPlatformProof{
		pid:                   42,
		serviceKind:           codexInstalledListenerServiceSystemdUser,
		persistent:            true,
		executable:            validCodexInstalledExecutableProofForLinuxTest(),
		serviceIdentitySHA256: [32]byte{1},
	}
	if !proof.valid() {
		t.Fatal("systemd user proof rejected")
	}
}

func validCodexInstalledExecutableProofForLinuxTest() codexInstalledExecutableProof {
	return codexInstalledExecutableProof{
		path: "/home/test/bin/cq", device: 1, inode: 2, links: 1, owner: 501,
		size: 4, mode: 0o100755, sha256: [32]byte{1},
	}
}
