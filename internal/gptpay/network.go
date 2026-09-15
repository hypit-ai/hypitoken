package gptpay

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"time"

	"github.com/wjsoj/cc-core/checkout"
)

var forbiddenNetworks = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"), netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("192.0.0.0/24"), netip.MustParsePrefix("192.0.2.0/24"),
	netip.MustParsePrefix("198.18.0.0/15"), netip.MustParsePrefix("198.51.100.0/24"),
	netip.MustParsePrefix("203.0.113.0/24"), netip.MustParsePrefix("240.0.0.0/4"),
	netip.MustParsePrefix("2001::/23"), netip.MustParsePrefix("2001:db8::/32"),
	netip.MustParsePrefix("2002::/16"), netip.MustParsePrefix("3fff::/20"),
}

func publicProxyIP(ip netip.Addr) bool {
	ip = ip.Unmap()
	if !ip.IsValid() || !ip.IsGlobalUnicast() || ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.Zone() != "" {
		return false
	}
	// Only currently allocated global IPv6 unicast space; excludes NAT64,
	// IPv4-compatible addresses, ULA, link-local and other translation prefixes.
	if ip.Is6() && !netip.MustParsePrefix("2000::/3").Contains(ip) {
		return false
	}
	for _, prefix := range forbiddenNetworks {
		if prefix.Contains(ip) {
			return false
		}
	}
	// This application's own production address is not a proxy target.
	return ip.String() != "209.209.50.222"
}

type lookupIP func(context.Context, string, string) ([]netip.Addr, error)

func pinProxy(ctx context.Context, raw string, lookup lookupIP) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", nil
	}
	if len(raw) > 2048 {
		return "", errors.New("代理地址过长")
	}
	// Validate URL/auth without making any connection or exposing parse errors.
	c, err := checkout.NewClient(raw)
	if err != nil {
		return "", err
	}
	c.Close()
	u, _ := url.Parse(raw)
	var ips []netip.Addr
	if ip, e := netip.ParseAddr(u.Hostname()); e == nil {
		ips = []netip.Addr{ip}
	} else {
		bounded, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		ips, err = lookup(bounded, "ip", u.Hostname())
		if err != nil {
			return "", errors.New("无法解析代理主机")
		}
	}
	if len(ips) == 0 {
		return "", errors.New("代理主机没有可用地址")
	}
	for _, ip := range ips {
		if !publicProxyIP(ip) {
			return "", errors.New("代理只允许公网地址，不能访问本站或内网")
		}
	}
	// Pin one validated address. Subsequent connections cannot re-resolve a
	// malicious hostname to localhost/private ranges (DNS rebinding).
	u.Host = net.JoinHostPort(ips[0].Unmap().String(), u.Port())
	return u.String(), nil
}
func proxyHash(raw string) string {
	v := sha256.Sum256([]byte(strings.TrimSpace(raw)))
	return hex.EncodeToString(v[:])
}
func (s *Service) client(proxy string) (backend, func(), error) {
	if s.backend != nil {
		return s.backend, func() {}, nil
	} // Test injection only.
	c, err := checkout.NewClient(proxy)
	if err != nil {
		return nil, func() {}, err
	}
	return c, c.Close, nil
}

type rateWindow struct {
	start          time.Time
	total, creates int
}

func (s *Service) allow(r *http.Request) bool {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	ip, err := netip.ParseAddr(host)
	if err == nil && ip.IsLoopback() {
		// Caddy appends the actual client last and doesn't trust forwarded headers
		// from public clients by default. Never trust these headers off-loopback.
		forwarded := strings.Split(r.Header.Get("X-Forwarded-For"), ",")
		if parsed, e := netip.ParseAddr(strings.TrimSpace(forwarded[len(forwarded)-1])); e == nil {
			ip = parsed
		}
	}
	if ip.Is6() {
		ip = netip.PrefixFrom(ip, 64).Masked().Addr()
	}
	key := ip.String()
	now := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.rates == nil {
		s.rates = make(map[string]*rateWindow)
	}
	w := s.rates[key]
	if w == nil || now.Sub(w.start) >= time.Minute {
		for k, v := range s.rates {
			if now.Sub(v.start) >= time.Minute {
				delete(s.rates, k)
			}
		}
		if len(s.rates) >= 4096 {
			return false
		}
		w = &rateWindow{start: now}
		s.rates[key] = w
	}
	if w.total >= 40 || r.URL.Path == "/api/create" && w.creates >= 3 {
		return false
	}
	w.total++
	if r.URL.Path == "/api/create" {
		w.creates++
	}
	return true
}
