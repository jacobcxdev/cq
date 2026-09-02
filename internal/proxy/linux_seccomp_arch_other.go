//go:build linux && !amd64 && !arm64

package proxy

func linuxSeccompAuditArchitecture() uint32 { return 0 }

func linuxSeccompX32SyscallBit() uint32 { return 0 }
