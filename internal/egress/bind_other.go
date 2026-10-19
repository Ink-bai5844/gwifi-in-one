//go:build !windows

package egress

import (
	"fmt"
	"syscall"
)

func bindInterface(index int) func(string, string, syscall.RawConn) error {
	return func(_, _ string, _ syscall.RawConn) error {
		return fmt.Errorf("此原型的网卡固定功能仅支持 Windows")
	}
}
