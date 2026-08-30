//go:build linux

package proxy

import (
	"testing"

	"golang.org/x/sys/unix"
)

func TestLinuxLandlockWriteRightsFollowABI(t *testing.T) {
	base := uint64(unix.LANDLOCK_ACCESS_FS_WRITE_FILE |
		unix.LANDLOCK_ACCESS_FS_REMOVE_DIR |
		unix.LANDLOCK_ACCESS_FS_REMOVE_FILE |
		unix.LANDLOCK_ACCESS_FS_MAKE_CHAR |
		unix.LANDLOCK_ACCESS_FS_MAKE_DIR |
		unix.LANDLOCK_ACCESS_FS_MAKE_REG |
		unix.LANDLOCK_ACCESS_FS_MAKE_SOCK |
		unix.LANDLOCK_ACCESS_FS_MAKE_FIFO |
		unix.LANDLOCK_ACCESS_FS_MAKE_BLOCK |
		unix.LANDLOCK_ACCESS_FS_MAKE_SYM)
	if got := linuxLandlockWriteRights(1); got != base {
		t.Fatalf("ABI 1 rights = %#x, want %#x", got, base)
	}
	if got := linuxLandlockWriteRights(2); got != base|unix.LANDLOCK_ACCESS_FS_REFER {
		t.Fatalf("ABI 2 rights = %#x", got)
	}
	if got := linuxLandlockWriteRights(3); got != base|unix.LANDLOCK_ACCESS_FS_REFER|unix.LANDLOCK_ACCESS_FS_TRUNCATE {
		t.Fatalf("ABI 3 rights = %#x", got)
	}
	if got := linuxLandlockWriteRights(5); got != base|unix.LANDLOCK_ACCESS_FS_REFER|unix.LANDLOCK_ACCESS_FS_TRUNCATE|unix.LANDLOCK_ACCESS_FS_IOCTL_DEV {
		t.Fatalf("ABI 5 rights = %#x", got)
	}
}
