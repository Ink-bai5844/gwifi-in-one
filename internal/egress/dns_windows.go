//go:build windows

package egress

import (
	"fmt"
	"net"
	"runtime"
	"unsafe"

	"golang.org/x/sys/windows"
)

// interfaceDNS reads the selected adapter's configured DNS, without querying
// another adapter or falling back to a public/system resolver.
func interfaceDNS(index int) (string, error) {
	size := uint32(15 * 1024)
	const flags = windows.GAA_FLAG_SKIP_UNICAST | windows.GAA_FLAG_SKIP_ANYCAST | windows.GAA_FLAG_SKIP_MULTICAST
	for attempt := 0; attempt < 3; attempt++ {
		if size == 0 || size > 1<<20 {
			return "", fmt.Errorf("网卡信息缓冲区大小异常")
		}
		buffer := make([]byte, size)
		first := (*windows.IpAdapterAddresses)(unsafe.Pointer(&buffer[0]))
		err := windows.GetAdaptersAddresses(windows.AF_INET, flags, 0, first, &size)
		if err == windows.ERROR_BUFFER_OVERFLOW {
			continue
		}
		if err != nil {
			return "", fmt.Errorf("GetAdaptersAddresses: %w", err)
		}
		dns, err := dnsFromAdapters(first, index)
		runtime.KeepAlive(buffer)
		return dns, err
	}
	return "", fmt.Errorf("读取网卡 DNS 时配置持续变化，请重试")
}

func dnsFromAdapters(first *windows.IpAdapterAddresses, index int) (string, error) {
	for adapter := first; adapter != nil; adapter = adapter.Next {
		if adapter.IfIndex != uint32(index) {
			continue
		}
		for server := adapter.FirstDnsServerAddress; server != nil; server = server.Next {
			if server.Address.Sockaddr == nil || server.Address.SockaddrLength <= 0 {
				continue
			}
			ip := server.Address.IP()
			if usableDNSIP(ip) {
				return net.JoinHostPort(ip.String(), "53"), nil
			}
		}
		return "", fmt.Errorf("此网卡未配置可用的非回环 IPv4 DNS")
	}
	return "", fmt.Errorf("找不到索引为 %d 的网卡 DNS", index)
}
