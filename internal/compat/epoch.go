package compat

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/jacobcxdev/cq/internal/fsutil"
)

const CurrentEpoch = 4

var ErrIncompatibleEpoch = errors.New("CQ binary predates persisted compatibility epoch")

func EnsureEpoch(fs fsutil.FileSystem, path string, binaryEpoch int) error {
	if fs == nil || path == "" || binaryEpoch <= 0 {
		return fmt.Errorf("invalid compatibility epoch configuration")
	}
	data, err := readEpoch(fs, path)
	stored := 0
	if err == nil {
		stored, err = strconv.Atoi(strings.TrimSpace(string(data)))
		if err != nil || stored <= 0 {
			return fmt.Errorf("parse compatibility epoch: invalid state")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("read compatibility epoch: %w", err)
	}
	if stored > binaryEpoch {
		return fmt.Errorf("%w: binary=%d stored=%d", ErrIncompatibleEpoch, binaryEpoch, stored)
	}
	if stored == binaryEpoch {
		return nil
	}
	if err := fsutil.SecureAtomicWrite(fs, path, []byte(strconv.Itoa(binaryEpoch)+"\n")); err != nil {
		return fmt.Errorf("commit compatibility epoch: %w", err)
	}
	return nil
}

func readEpoch(fs fsutil.FileSystem, path string) ([]byte, error) {
	if err := fsutil.EnsureSecureDirectory(fs, filepath.Dir(path)); err != nil {
		return nil, err
	}
	return fsutil.ReadSecureFile(fs, path, 64)
}
