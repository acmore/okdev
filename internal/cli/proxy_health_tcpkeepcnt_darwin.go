//go:build darwin

package cli

import (
	"syscall"

	"golang.org/x/sys/unix"
)

func setTCPKeepCnt(raw syscall.RawConn, count int) error {
	var sysErr error
	err := raw.Control(func(fd uintptr) {
		sysErr = unix.SetsockoptInt(int(fd), unix.IPPROTO_TCP, unix.TCP_KEEPCNT, count)
	})
	if err != nil {
		return err
	}
	return sysErr
}
