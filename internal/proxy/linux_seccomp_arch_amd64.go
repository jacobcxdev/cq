//go:build linux && amd64

package proxy

func linuxSeccompAuditArchitecture() uint32 { return 0xc000003e }

func linuxSeccompX32SyscallBit() uint32 { return 0x40000000 }
