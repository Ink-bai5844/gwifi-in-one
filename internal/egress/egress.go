// Package egress creates IPv4 HTTP clients pinned to one Windows interface.
package egress

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/cookiejar"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	Name      string `json:"name"`
	Interface string `json:"interface"`
	SourceIP  string `json:"source_ip"`
	DNS       string `json:"dns"`
	PortalURL string `json:"portal_url"`
}

type Bound struct {
	Config Config
	Index  int
	IP     net.IP
	Client *http.Client
}

func New(cfg Config) (*Bound, error) {
	if cfg.Name == "" || cfg.Interface == "" {
		return nil, fmt.Errorf("出口 name 和 interface 不能为空")
	}
	iface, err := net.InterfaceByName(cfg.Interface)
	if err != nil {
		return nil, fmt.Errorf("网卡 %q: %w", cfg.Interface, err)
	}
	if iface.Flags&net.FlagUp == 0 {
		return nil, fmt.Errorf("网卡 %q 未启用或未连接", cfg.Interface)
	}
	addrs, err := iface.Addrs()
	if err != nil {
		return nil, err
	}
	var candidates []net.IP
	for _, addr := range addrs {
		ip, _, err := net.ParseCIDR(addr.String())
		if err != nil || ip.To4() == nil || ip.IsLoopback() || ip.IsLinkLocalUnicast() {
			continue
		}
		if cfg.SourceIP == "" || ip.Equal(net.ParseIP(cfg.SourceIP)) {
			candidates = append(candidates, ip.To4())
		}
	}
	if len(candidates) != 1 {
		return nil, fmt.Errorf("网卡 %q 匹配到 %d 个可用 IPv4；先连接网络，多个地址时在 source_ip 指定实际地址", cfg.Interface, len(candidates))
	}
	ip := candidates[0]
	control := bindInterface(iface.Index)
	dns, err := chooseDNS(iface.Index, cfg.DNS, interfaceDNS)
	if err != nil {
		return nil, fmt.Errorf("网卡 %q: %w", cfg.Interface, err)
	}
	cfg.DNS = dns
	resolver := &net.Resolver{PreferGo: true, Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
		d := net.Dialer{Timeout: 5 * time.Second, Control: control}
		if network == "tcp" || network == "tcp4" {
			d.LocalAddr = &net.TCPAddr{IP: ip}
			return d.DialContext(ctx, "tcp4", dns)
		}
		d.LocalAddr = &net.UDPAddr{IP: ip}
		return d.DialContext(ctx, "udp4", dns)
	}}
	dialer := &net.Dialer{LocalAddr: &net.TCPAddr{IP: ip}, Control: control, Resolver: resolver, Timeout: 15 * time.Second, KeepAlive: 30 * time.Second}
	transport := &http.Transport{
		Proxy: nil, // Never inherit HTTP_PROXY or the system proxy for separate exits.
		DialContext: func(ctx context.Context, _, addr string) (net.Conn, error) {
			return dialer.DialContext(ctx, "tcp4", addr)
		},
		ForceAttemptHTTP2: false, DisableCompression: true,
		TLSHandshakeTimeout: 15 * time.Second, ResponseHeaderTimeout: 20 * time.Second,
		MaxIdleConnsPerHost: 32, IdleConnTimeout: 60 * time.Second,
	}
	jar, _ := cookiejar.New(nil)
	client := &http.Client{Transport: transport, Jar: jar, Timeout: 90 * time.Second, CheckRedirect: checkRedirect}
	return &Bound{Config: cfg, Index: iface.Index, IP: ip, Client: client}, nil
}

func (b *Bound) Close() { b.Client.CloseIdleConnections() }

func checkRedirect(req *http.Request, via []*http.Request) error {
	// Go otherwise copies the previous URL, including its signed query, into
	// Referer even when redirecting to another origin.
	req.Header.Del("Referer")
	if len(via) >= 10 {
		return fmt.Errorf("重定向次数过多")
	}
	if req.URL.Scheme != "http" && req.URL.Scheme != "https" {
		return fmt.Errorf("不支持的重定向协议")
	}
	if req.URL.User != nil {
		return fmt.Errorf("重定向 URL 不允许内嵌凭据")
	}
	if len(via) > 0 && via[len(via)-1].URL.Scheme == "https" && req.URL.Scheme != "https" {
		return fmt.Errorf("拒绝 HTTPS 降级重定向")
	}
	return nil
}

func chooseDNS(index int, configured string, lookup func(int) (string, error)) (string, error) {
	dns := strings.TrimSpace(configured)
	if dns == "" {
		var err error
		dns, err = lookup(index)
		if err != nil {
			return "", fmt.Errorf("读取此网卡的 IPv4 DNS 失败；请在配置 dns 填写该出口可访问的 IPv4:端口: %w", err)
		}
	}
	host, portText, err := net.SplitHostPort(dns)
	ip := net.ParseIP(host)
	port, portErr := strconv.Atoi(portText)
	if err != nil || !usableDNSIP(ip) || portErr != nil || port < 1 || port > 65535 {
		return "", fmt.Errorf("dns 必须为非回环单播 IPv4:端口，端口范围 1..65535")
	}
	return net.JoinHostPort(ip.String(), strconv.Itoa(port)), nil
}

func usableDNSIP(ip net.IP) bool {
	return ip.To4() != nil && !ip.IsLoopback() && !ip.IsUnspecified() && !ip.IsMulticast() && !ip.Equal(net.IPv4bcast)
}
