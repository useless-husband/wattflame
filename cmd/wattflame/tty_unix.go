//go:build darwin || linux

package main

import (
	"os"
	"syscall"
	"unsafe"
)

// inForeground reports whether wattflame is in the foreground process group of
// its controlling terminal. If so, a Ctrl-C typed there was delivered by the
// terminal to every process in the group, the launched program included.
func inForeground() bool {
	tty, err := os.Open("/dev/tty")
	if err != nil {
		return false // no controlling terminal
	}
	defer tty.Close()
	var pgrp int32
	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, tty.Fd(), uintptr(syscall.TIOCGPGRP), uintptr(unsafe.Pointer(&pgrp))); errno != 0 {
		return false
	}
	return int(pgrp) == syscall.Getpgrp()
}
