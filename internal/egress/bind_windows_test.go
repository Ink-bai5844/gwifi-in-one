//go:build windows

package egress

import (
	"context"
	"net"
	"syscall"
	"testing"
	"time"
)

// Exercise the real Winsock option, including its network byte order. This
// does not establish that any campus account has independent bandwidth.
func TestPinnedWindowsSocket(t *testing.T) {
	ifaces, err := net.Interfaces()
	if err != nil {
		t.Fatal(err)
	}
	index := 0
	for _, iface := range ifaces {
		addrs, _ := iface.Addrs()
		for _, a := range addrs {
			ip, _, _ := net.ParseCIDR(a.String())
			if ip.Equal(net.IPv4(127, 0, 0, 1)) {
				index = iface.Index
			}
		}
	}
	if index == 0 {
		t.Fatal("IPv4 loopback interface not found")
	}
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	accepted := make(chan error, 1)
	go func() {
		c, err := listener.Accept()
		if err == nil {
			c.Close()
		}
		accepted <- err
	}()
	d := net.Dialer{LocalAddr: &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)}, Control: bindInterface(index)}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	conn, err := d.DialContext(ctx, "tcp4", listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	raw, err := conn.(*net.TCPConn).SyscallConn()
	if err != nil {
		t.Fatal(err)
	}
	var option int
	var optionErr error
	if err = raw.Control(func(fd uintptr) {
		option, optionErr = syscall.GetsockoptInt(syscall.Handle(fd), syscall.IPPROTO_IP, 31)
	}); err != nil {
		t.Fatal(err)
	}
	if optionErr != nil {
		t.Fatal(optionErr)
	}
	// Winsock accepts network byte order on set, but returns host byte order.
	if option != index {
		t.Fatalf("IP_UNICAST_IF=%d, expected interface %d", option, index)
	}
	if err = <-accepted; err != nil {
		t.Fatal(err)
	}
}
