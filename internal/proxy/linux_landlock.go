//go:build linux

package proxy

import (
	"errors"
	"os"
	"unsafe"

	"golang.org/x/sys/unix"
)

type linuxLandlockRulesetAttr struct {
	HandledAccessFS uint64
}

type linuxLandlockPathBeneathAttr struct {
	AllowedAccess uint64
	ParentFD      int32
	Reserved      uint32
}

func linuxLandlockABI() (int, error) {
	version, _, errno := unix.Syscall6(
		unix.SYS_LANDLOCK_CREATE_RULESET,
		0,
		0,
		unix.LANDLOCK_CREATE_RULESET_VERSION,
		0,
		0,
		0,
	)
	if errno != 0 || version < 1 {
		return 0, errors.New("Landlock unavailable")
	}
	return int(version), nil
}

func linuxLandlockWriteRights(abi int) uint64 {
	rights := uint64(unix.LANDLOCK_ACCESS_FS_WRITE_FILE |
		unix.LANDLOCK_ACCESS_FS_REMOVE_DIR |
		unix.LANDLOCK_ACCESS_FS_REMOVE_FILE |
		unix.LANDLOCK_ACCESS_FS_MAKE_CHAR |
		unix.LANDLOCK_ACCESS_FS_MAKE_DIR |
		unix.LANDLOCK_ACCESS_FS_MAKE_REG |
		unix.LANDLOCK_ACCESS_FS_MAKE_SOCK |
		unix.LANDLOCK_ACCESS_FS_MAKE_FIFO |
		unix.LANDLOCK_ACCESS_FS_MAKE_BLOCK |
		unix.LANDLOCK_ACCESS_FS_MAKE_SYM)
	if abi >= 2 {
		rights |= unix.LANDLOCK_ACCESS_FS_REFER
	}
	if abi >= 3 {
		rights |= unix.LANDLOCK_ACCESS_FS_TRUNCATE
	}
	if abi >= 5 {
		rights |= unix.LANDLOCK_ACCESS_FS_IOCTL_DEV
	}
	return rights
}

func applyLinuxLandlockWriteConfinement(writeRoot string) error {
	abi, err := linuxLandlockABI()
	if err != nil {
		return err
	}
	if abi < 3 {
		return errors.New("Landlock truncate confinement unavailable")
	}
	rights := linuxLandlockWriteRights(abi)
	attr := linuxLandlockRulesetAttr{HandledAccessFS: rights}
	ruleset, _, errno := unix.Syscall6(
		unix.SYS_LANDLOCK_CREATE_RULESET,
		uintptr(unsafe.Pointer(&attr)),
		unsafe.Sizeof(attr),
		0,
		0,
		0,
		0,
	)
	if errno != 0 {
		return errors.New("create Landlock ruleset")
	}
	rulesetFD := int(ruleset)
	defer unix.Close(rulesetFD)
	if writeRoot != "" {
		if err := addLinuxLandlockPathRule(rulesetFD, writeRoot, rights); err != nil {
			return err
		}
	}
	nullRights := rights & uint64(unix.LANDLOCK_ACCESS_FS_WRITE_FILE|unix.LANDLOCK_ACCESS_FS_TRUNCATE|unix.LANDLOCK_ACCESS_FS_IOCTL_DEV)
	if nullRights != 0 {
		if err := addLinuxLandlockPathRule(rulesetFD, "/dev/null", nullRights); err != nil {
			return err
		}
	}
	if err := unix.Prctl(unix.PR_SET_NO_NEW_PRIVS, 1, 0, 0, 0); err != nil {
		return errors.New("set no_new_privs")
	}
	_, _, errno = unix.Syscall(unix.SYS_LANDLOCK_RESTRICT_SELF, uintptr(rulesetFD), 0, 0)
	if errno != 0 {
		return errors.New("apply Landlock ruleset")
	}
	return nil
}

func addLinuxLandlockPathRule(rulesetFD int, path string, allowed uint64) error {
	file, err := os.OpenFile(path, unix.O_PATH|unix.O_CLOEXEC, 0)
	if err != nil {
		return errors.New("open Landlock path")
	}
	defer file.Close()
	attr := linuxLandlockPathBeneathAttr{AllowedAccess: allowed, ParentFD: int32(file.Fd())}
	_, _, errno := unix.Syscall6(
		unix.SYS_LANDLOCK_ADD_RULE,
		uintptr(rulesetFD),
		1,
		uintptr(unsafe.Pointer(&attr)),
		0,
		0,
		0,
	)
	if errno != 0 {
		return errors.New("add Landlock path rule")
	}
	return nil
}
