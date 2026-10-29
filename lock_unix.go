//go:build unix

package main

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
)

// lockData takes <data>/server.lock for this process (the running server holds it; restore wants it free).
func lockData(data string) (func(), error) {
	if err := os.MkdirAll(data, 0o750); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(data, lockName), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		return nil, errors.New("the server is running on " + data + " (it holds server.lock): stop it first (systemctl stop spectrevoice)")
	}
	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
	}, nil
}

// freeBytes: room left for an unprivileged user on dir's file system.
func freeBytes(dir string) (int64, bool) {
	var st syscall.Statfs_t
	if syscall.Statfs(dir, &st) != nil {
		return 0, false
	}
	return int64(st.Bavail) * int64(st.Bsize), true
}
