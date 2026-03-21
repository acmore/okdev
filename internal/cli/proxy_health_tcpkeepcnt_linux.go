//go:build linux

package cli

import "syscall"

func setTCPKeepCnt(raw syscall.RawConn, count int) error {
	var sysErr error
	err := raw.Control(func(fd uintptr) {
		sysErr = syscall.SetsockoptInt(int(fd), syscall.IPPROTO_TCP, syscall.TCP_KEEPCNT, count)
	})
	if err != nil {
		return err
	}
	return sysErr
}
