//go:build darwin

package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/jacobcxdev/cq/internal/fsutil"
	"golang.org/x/sys/unix"
)

func init() { startProxyServiceStderrRetention = startDarwinProxyStderrRetention }

// An exec must not strand an in-progress retained snapshot temporary. Exec
// failure releases this fence so the predecessor's monitor can continue.
var proxyStderrRetentionExecMu sync.Mutex

func startDarwinProxyStderrRetention(logs string) (func() error, error) {
	path := filepath.Join(logs, "proxy.log")
	file, err := openProxyServiceStderr(path, os.Stderr)
	if err != nil || file == nil {
		return func() error { return nil }, err
	}
	return monitorProxyServiceStderr(file, path), nil
}

func monitorProxyServiceStderr(file *os.File, path string) func() error {
	stop, done := make(chan struct{}), make(chan struct{})
	var closeErr error
	go func() {
		defer close(done)
		defer func() {
			closeErr = errors.Join(file.Sync(), file.Close(), (fsutil.OSFileSystem{}).SyncDir(filepath.Dir(path)))
		}()
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		failed := false
		defer func() {
			if recover() != nil {
				fmt.Fprintln(os.Stderr, "cq: service stderr retention stopped after panic")
			}
		}()
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				err := func() error {
					proxyStderrRetentionExecMu.Lock()
					defer proxyStderrRetentionExecMu.Unlock()
					return compactProxyServiceStderr(file, path, 64<<20, 4)
				}()
				if err != nil && !failed {
					fmt.Fprintf(os.Stderr, "cq: service stderr retention failed: %v\n", err)
				}
				failed = err != nil
			}
		}
	}()
	return func() error { close(stop); <-done; return closeErr }
}

func openProxyServiceStderr(path string, stderr *os.File) (*os.File, error) {
	current, err := stderr.Stat()
	if err != nil || !current.Mode().IsRegular() {
		return nil, err
	}
	named, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if !named.Mode().IsRegular() || !os.SameFile(current, named) {
		return nil, nil
	}
	fd, err := unix.Open(path, unix.O_RDWR|unix.O_APPEND|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), path)
	info, err := file.Stat()
	if err != nil || !os.SameFile(info, current) {
		_ = file.Close()
		return nil, errors.Join(errors.New("service stderr path changed"), err)
	}
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil || stat.Uid != uint32(os.Geteuid()) || stat.Nlink != 1 {
		_ = file.Close()
		return nil, errors.Join(errors.New("unsafe service stderr ownership"), err)
	}
	if err := file.Chmod(0o600); err != nil {
		_ = file.Close()
		return nil, err
	}
	flags, err := unix.FcntlInt(stderr.Fd(), unix.F_GETFL, 0)
	if err == nil {
		_, err = unix.FcntlInt(stderr.Fd(), unix.F_SETFL, flags|unix.O_APPEND)
	}
	if err != nil {
		_ = file.Close()
		return nil, err
	}
	return file, nil
}

// Keep the canonical inode: launchd, the worker, guards and an exec successor
// already hold it open. This is a periodic size policy, not an atomic record
// sink. Writers do not take this lock; the marker exposes the copy/truncate
// race, and a burst may exceed maxBytes until the next one-second check.
func compactProxyServiceStderr(file *os.File, path string, maxBytes int64, retained int) (result error) {
	if maxBytes < 256 || retained < 1 {
		return errors.New("invalid service stderr retention limits")
	}
	if err := unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		if errors.Is(err, unix.EWOULDBLOCK) {
			return nil
		}
		return err
	}
	defer func() { result = errors.Join(result, unix.Flock(int(file.Fd()), unix.LOCK_UN)) }()
	info, err := file.Stat()
	if err != nil {
		return err
	}
	verify := func() error {
		named, err := os.Lstat(path)
		if err != nil {
			return err
		}
		if !named.Mode().IsRegular() || !os.SameFile(info, named) {
			return errors.New("service stderr path changed")
		}
		return nil
	}
	if err := verify(); err != nil {
		return err
	}
	if info.Size() <= maxBytes {
		return nil
	}
	for index := 1; index <= retained; index++ {
		backup, err := os.Lstat(fmt.Sprintf("%s.%d", path, index))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		if !backup.Mode().IsRegular() {
			return errors.New("unsafe service stderr backup")
		}
	}
	temporary, err := os.CreateTemp(filepath.Dir(path), ".proxy-stderr-*")
	if err != nil {
		return err
	}
	defer os.Remove(temporary.Name())
	defer temporary.Close()
	header := fmt.Sprintf("cq: retained tail of %d-byte stderr snapshot; earlier bytes omitted\n", info.Size())
	if _, err := temporary.WriteString(header); err != nil {
		return err
	}
	length := min(info.Size(), maxBytes-int64(len(header)))
	if _, err := io.CopyN(temporary, io.NewSectionReader(file, info.Size()-length, length), length); err != nil {
		return err
	}
	if err := temporary.Sync(); err != nil {
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if err := verify(); err != nil {
		return err
	}
	if err := os.Remove(fmt.Sprintf("%s.%d", path, retained)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	for index := retained - 1; index >= 1; index-- {
		if err := os.Rename(fmt.Sprintf("%s.%d", path, index), fmt.Sprintf("%s.%d", path, index+1)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	if err := os.Rename(temporary.Name(), path+".1"); err != nil {
		return err
	}
	if err := verify(); err != nil {
		return err
	}
	if err := file.Truncate(0); err != nil {
		return err
	}
	_, err = file.WriteString("cq: stderr rotated; retained tail in proxy.log.1; writes concurrent with snapshot/truncate may be omitted\n")
	return err
}
