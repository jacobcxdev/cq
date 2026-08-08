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

const CurrentEpoch = 1

var ErrIncompatibleEpoch = errors.New("CQ binary predates persisted compatibility epoch")

func DefaultEpochPath(fs fsutil.FileSystem, getenv func(string) string) (string, error) {
	if getenv != nil {
		if dir := getenv("XDG_CONFIG_HOME"); dir != "" && filepath.IsAbs(dir) {
			return filepath.Join(dir, "cq", "state", "compatibility_epoch"), nil
		}
	}
	home, err := fs.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home directory: %w", err)
	}
	return filepath.Join(home, ".config", "cq", "state", "compatibility_epoch"), nil
}

func EnsureEpoch(fs fsutil.FileSystem, path string, binaryEpoch int) error {
	if fs == nil || path == "" || binaryEpoch <= 0 {
		return fmt.Errorf("invalid compatibility epoch configuration")
	}
	data, err := fs.ReadFile(path)
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
	if err := fs.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create compatibility state directory: %w", err)
	}
	tmp := path + ".tmp"
	if err := fs.WriteFile(tmp, []byte(strconv.Itoa(binaryEpoch)+"\n"), 0o600); err != nil {
		return fmt.Errorf("write compatibility epoch: %w", err)
	}
	if err := fs.Rename(tmp, path); err != nil {
		_ = fs.Remove(tmp)
		return fmt.Errorf("commit compatibility epoch: %w", err)
	}
	return nil
}
