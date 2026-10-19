//go:build windows

package egress

import (
	"syscall"
	"testing"
	"unsafe"

	"golang.org/x/sys/windows"
)

func testDNSAddress(ip [4]byte) *windows.IpAdapterDnsServerAdapter {
	raw := &windows.RawSockaddrInet4{Family: windows.AF_INET, Addr: ip}
	return &windows.IpAdapterDnsServerAdapter{Address: windows.SocketAddress{
		Sockaddr:       (*syscall.RawSockaddrAny)(unsafe.Pointer(raw)),
		SockaddrLength: int32(unsafe.Sizeof(*raw)),
	}}
}

func TestDNSFromAdaptersDoesNotUseAnotherInterface(t *testing.T) {
	other := &windows.IpAdapterAddresses{IfIndex: 1, FirstDnsServerAddress: testDNSAddress([4]byte{8, 8, 8, 8})}
	selected := &windows.IpAdapterAddresses{IfIndex: 42, FirstDnsServerAddress: testDNSAddress([4]byte{10, 20, 0, 53})}
	other.Next = selected
	dns, err := dnsFromAdapters(other, 42)
	if err != nil || dns != "10.20.0.53:53" {
		t.Fatalf("selected DNS = %q, %v", dns, err)
	}
	selected.FirstDnsServerAddress = nil
	if _, err := dnsFromAdapters(other, 42); err == nil {
		t.Fatal("adapter without DNS used another adapter")
	}
	if _, err := dnsFromAdapters(other, 99); err == nil {
		t.Fatal("missing adapter used another adapter")
	}
}

func TestDNSFromAdaptersSkipsLocalProxyAndInvalidAddress(t *testing.T) {
	invalid := &windows.IpAdapterDnsServerAdapter{}
	local := testDNSAddress([4]byte{127, 0, 0, 1})
	valid := testDNSAddress([4]byte{10, 20, 0, 53})
	invalid.Next = local
	local.Next = valid
	selected := &windows.IpAdapterAddresses{IfIndex: 42, FirstDnsServerAddress: invalid}
	dns, err := dnsFromAdapters(selected, 42)
	if err != nil || dns != "10.20.0.53:53" {
		t.Fatalf("selected DNS = %q, %v", dns, err)
	}
	local.Next = nil
	if _, err := dnsFromAdapters(selected, 42); err == nil {
		t.Fatal("local DNS proxy accepted as independent-exit DNS")
	}
}
