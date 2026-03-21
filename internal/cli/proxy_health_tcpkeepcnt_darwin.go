//go:build darwin

package cli

import (
	"fmt"
	"syscall"
)

func setTCPKeepCnt(raw syscall.RawConn, count int) error {
	return fmt.Errorf("TCP_KEEPCNT unsupported on darwin")
}
