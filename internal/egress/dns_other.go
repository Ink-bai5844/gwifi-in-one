//go:build !windows

package egress

import "fmt"

func interfaceDNS(index int) (string, error) {
	return "", fmt.Errorf("自动读取网卡 DNS 仅支持 Windows")
}
