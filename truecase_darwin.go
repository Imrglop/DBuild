package main

import (
	"os"
	"syscall"
	"unsafe"
)

const fGetPath = 50 // F_GETPATH

// diskPath returns the path the filesystem stores for p (on case-insensitive volumes this
// fixes the case of every component; symlinks are resolved too).
func diskPath(p string) (string, error) {
	f, err := os.Open(p)
	if err != nil {
		return "", err
	}
	defer f.Close()
	var buf [1024]byte // MAXPATHLEN
	if _, _, e := syscall.Syscall(syscall.SYS_FCNTL, f.Fd(), fGetPath, uintptr(unsafe.Pointer(&buf[0]))); e != 0 {
		return "", e
	}
	for i, b := range buf {
		if b == 0 {
			return string(buf[:i]), nil
		}
	}
	return "", syscall.ENAMETOOLONG
}
