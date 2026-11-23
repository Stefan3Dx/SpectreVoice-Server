//go:build windows

package main

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"unsafe"
)

// lockData takes <data>/server.lock for this process, opened with no sharing: while the running server holds it
// nobody else can open it (restore wants it free). Windows lets it go when the process ends, however it ends.
func lockData(data string) (func(), error) {
	if err := os.MkdirAll(data, 0o750); err != nil {
		return nil, err
	}
	name, err := syscall.UTF16PtrFromString(filepath.Join(data, lockName))
	if err != nil {
		return nil, err
	}
	h, err := syscall.CreateFile(name, syscall.GENERIC_READ|syscall.GENERIC_WRITE, 0, nil, syscall.OPEN_ALWAYS, syscall.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		if errors.Is(err, errSharingViolation) || errors.Is(err, syscall.ERROR_ACCESS_DENIED) {
			return nil, errors.New("the server is running on " + data + " (it holds server.lock): stop it first")
		}
		return nil, err
	}
	return func() { _ = syscall.CloseHandle(h) }, nil
}

// ERROR_SHARING_VIOLATION (32): the file is open elsewhere with no sharing
const errSharingViolation = syscall.Errno(32)

var procGetDiskFreeSpaceEx = syscall.NewLazyDLL("kernel32.dll").NewProc("GetDiskFreeSpaceExW")

// freeBytes: room left for this user on dir's volume.
func freeBytes(dir string) (int64, bool) {
	p, err := syscall.UTF16PtrFromString(dir)
	if err != nil {
		return 0, false
	}
	var avail uint64
	if r, _, _ := procGetDiskFreeSpaceEx.Call(uintptr(unsafe.Pointer(p)), uintptr(unsafe.Pointer(&avail)), 0, 0); r == 0 {
		return 0, false
	}
	return int64(avail), true
}
