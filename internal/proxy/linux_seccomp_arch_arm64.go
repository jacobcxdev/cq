//go:build linux && arm64

package proxy

func linuxSeccompAuditArchitecture() uint32 { return 0xc00000b7 }

func linuxSeccompX32SyscallBit() uint32 { return 0 }
