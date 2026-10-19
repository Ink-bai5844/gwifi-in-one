//go:build windows

package egress

import (
	"fmt"
	"math/bits"
	"syscall"
)

func bindInterface(index int) func(string, string, syscall.RawConn) error {
	return func(_, _ string, conn syscall.RawConn) error {
		var socketErr error
		err := conn.Control(func(fd uintptr) {
			// Windows IP_UNICAST_IF (31) expects the index in network byte order.
			socketErr = syscall.SetsockoptInt(syscall.Handle(fd), syscall.IPPROTO_IP, 31, int(bits.ReverseBytes32(uint32(index))))
		})
		if err != nil {
			return err
		}
		if socketErr != nil {
			return fmt.Errorf("固定出口网卡失败: %w", socketErr)
		}
		return nil
	}
}
