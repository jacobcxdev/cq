//go:build linux

package proxy

import (
	"testing"

	"golang.org/x/sys/unix"
)

func TestLinuxNamespaceHelperSealsAmbientDescriptors(t *testing.T) {
	sockets, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, fd := range sockets {
		t.Cleanup(func() { _ = unix.Close(fd) })
	}
	flags, _, errno := unix.Syscall(unix.SYS_FCNTL, uintptr(sockets[0]), unix.F_GETFD, 0)
	if errno != 0 {
		t.Fatal(errno)
	}
	if flags&unix.FD_CLOEXEC != 0 {
		t.Fatal("test descriptor unexpectedly close-on-exec")
	}
	if err := closeLinuxNamespaceDescriptorOnExec(sockets[0], flags); err != nil {
		t.Fatal(err)
	}
	flags, _, errno = unix.Syscall(unix.SYS_FCNTL, uintptr(sockets[0]), unix.F_GETFD, 0)
	if errno != 0 || flags&unix.FD_CLOEXEC == 0 {
		t.Fatalf("descriptor flags = %d, %v", flags, errno)
	}
}

func TestLinuxNamespaceConfigRejectsUnboundedAuthority(t *testing.T) {
	root := t.TempDir()
	valid := linuxNamespaceConfig{
		Version:    linuxNamespaceProtocolVersion,
		Executable: "/usr/bin/codex",
		Args:       []string{"--version"},
		WriteRoot:  root,
		Relays: []linuxRelayDefinition{{
			ID:     linuxRelayProxy,
			Port:   19431,
			Target: "127.0.0.1:19431",
		}},
	}
	if err := valid.validate(); err != nil {
		t.Fatalf("valid config rejected: %v", err)
	}
	tests := map[string]func(*linuxNamespaceConfig){
		"version":             func(config *linuxNamespaceConfig) { config.Version++ },
		"relative root":       func(config *linuxNamespaceConfig) { config.WriteRoot = "relative" },
		"root authority":      func(config *linuxNamespaceConfig) { config.WriteRoot = "/" },
		"unknown relay":       func(config *linuxNamespaceConfig) { config.Relays[0].ID = 99 },
		"non-loopback target": func(config *linuxNamespaceConfig) { config.Relays[0].Target = "1.1.1.1:443" },
		"port mismatch":       func(config *linuxNamespaceConfig) { config.Relays[0].Port++ },
		"duplicate relay":     func(config *linuxNamespaceConfig) { config.Relays = append(config.Relays, config.Relays[0]) },
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			config := valid
			config.Args = append([]string(nil), valid.Args...)
			config.Relays = append([]linuxRelayDefinition(nil), valid.Relays...)
			mutate(&config)
			if err := config.validate(); err == nil {
				t.Fatal("accepted invalid namespace authority")
			}
		})
	}
}
