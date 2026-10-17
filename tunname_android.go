//go:build android

package main

import (
	"fmt"
	"syscall"
	"unsafe"

	"golang.org/x/sys/unix"
)

// tunName asks the kernel what the TUN behind fd is called (tun0, tun1, …).
func tunName(fd int) (string, error) {
	var ifr [unix.IFNAMSIZ + 64]byte
	_, _, errno := unix.Syscall(unix.SYS_IOCTL, uintptr(fd), uintptr(unix.TUNGETIFF), uintptr(unsafe.Pointer(&ifr[0])))
	if errno != 0 {
		return "", fmt.Errorf("имя TUN не узнать: %w", syscall.Errno(errno))
	}
	return unix.ByteSliceToString(ifr[:]), nil
}
