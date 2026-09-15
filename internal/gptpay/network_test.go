package gptpay

import (
	"context"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
)

func TestPublicProxyIP(t *testing.T) {
	for _, raw := range []string{"127.0.0.1", "10.0.0.1", "172.16.0.1", "192.168.1.1", "169.254.169.254", "100.64.0.1", "0.0.0.0", "192.0.2.1", "198.18.1.1", "224.0.0.1", "255.255.255.255", "::1", "::ffff:127.0.0.1", "fc00::1", "fe80::1", "64:ff9b::7f00:1", "2002:7f00:1::", "2001:db8::1", "209.209.50.222"} {
		if publicProxyIP(netip.MustParseAddr(raw)) {
			t.Fatalf("unsafe IP accepted: %s", raw)
		}
	}
	for _, raw := range []string{"8.8.8.8", "1.1.1.1", "2606:4700:4700::1111"} {
		if !publicProxyIP(netip.MustParseAddr(raw)) {
			t.Fatalf("public IP rejected: %s", raw)
		}
	}
}
func TestProxyPinningAndNoDirectFallback(t *testing.T) {
	calls := 0
	lookup := func(context.Context, string, string) ([]netip.Addr, error) {
		calls++
		return []netip.Addr{netip.MustParseAddr("8.8.8.8")}, nil
	}
	p, err := pinProxy(context.Background(), "", lookup)
	if err != nil || p != "" || calls != 0 {
		t.Fatal("empty must mean direct")
	}
	p, err = pinProxy(context.Background(), "socks5://user:pass@proxy.example:1080", lookup)
	if err != nil || p != "socks5://user:pass@8.8.8.8:1080" || calls != 1 {
		t.Fatalf("pinning failed: %v", err)
	}
	mixed := func(context.Context, string, string) ([]netip.Addr, error) {
		return []netip.Addr{netip.MustParseAddr("8.8.8.8"), netip.MustParseAddr("127.0.0.1")}, nil
	}
	if _, err = pinProxy(context.Background(), "socks5://proxy.example:1080", mixed); err == nil {
		t.Fatal("mixed public/private DNS accepted")
	}
	for _, raw := range []string{"http://user:secret@8.8.8.8:1080", "socks5://user:secret@127.0.0.1:1080", "socks5://user:secret@[::1]:1080"} {
		_, err = pinProxy(context.Background(), raw, lookup)
		if err == nil || strings.Contains(err.Error(), "secret") {
			t.Fatal("unsafe URL accepted or leaked")
		}
	}
}
func TestPublicRateLimit(t *testing.T) {
	s, _ := newTestService(t)
	r := httptest.NewRequest("POST", "/api/create", nil)
	for range 3 {
		if !s.allow(r) {
			t.Fatal("early rate limit")
		}
	}
	if s.allow(r) {
		t.Fatal("create burst not limited")
	}
	r.URL.Path = "/api/status"
	if !s.allow(r) {
		t.Fatal("status recovery should remain available")
	}
	// A non-loopback caller cannot evade limits using forwarding headers.
	r.URL.Path = "/api/create"
	r.Header.Set("X-Forwarded-For", "8.8.8.8")
	if s.allow(r) {
		t.Fatal("trusted spoofed forwarding header")
	}
}
func TestFlowNetworkCannotChange(t *testing.T) {
	s, b := newTestService(t)
	h := NewHandler(s)
	req := requestFixture()
	req.Proxy = "socks5://8.8.8.8:1080"
	code, out := call(t, h, "/api/create", req, s.origin)
	if code != 200 {
		t.Fatalf("create: %d %v", code, out)
	}
	req.Flow = out["flow"].(string)
	req.Proxy = ""
	code, _ = call(t, h, "/api/quote", req, s.origin)
	if code != 409 || b.pays != 0 {
		t.Fatal("proxy-to-direct change accepted")
	}
	req.Proxy = "socks5://8.8.8.8:1080"
	code, _ = call(t, h, "/api/quote", req, s.origin)
	if code != 200 {
		t.Fatal("original route rejected")
	}
}
