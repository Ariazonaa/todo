package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestRateLimiterBurstThenBlocks(t *testing.T) {
	rl := newRateLimiter(60, 5)
	now := time.Now()
	for i := range 5 {
		if !rl.allow("a", now) {
			t.Fatalf("Request %d hätte im Burst durchgehen müssen", i+1)
		}
	}
	if rl.allow("a", now) {
		t.Fatal("nach aufgebrauchtem Burst muss geblockt werden")
	}
	// 60/min = one token per second
	if !rl.allow("a", now.Add(time.Second)) {
		t.Fatal("nach einer Sekunde muss ein Token nachgewachsen sein")
	}
	if rl.allow("a", now.Add(time.Second)) {
		t.Fatal("es wächst nur ein Token pro Sekunde nach")
	}
}

func TestRateLimiterIsPerClient(t *testing.T) {
	rl := newRateLimiter(60, 2)
	now := time.Now()
	rl.allow("angreifer", now)
	rl.allow("angreifer", now)
	if rl.allow("angreifer", now) {
		t.Fatal("Angreifer sollte ausgebremst sein")
	}
	if !rl.allow("nutzer", now) {
		t.Fatal("ein anderer Client darf davon nichts merken")
	}
}

func TestRateLimiterTokensCapAtBurst(t *testing.T) {
	rl := newRateLimiter(60, 3)
	now := time.Now()
	rl.allow("a", now)
	// A long pause must not accumulate credit beyond the burst
	later := now.Add(time.Hour)
	for i := range 3 {
		if !rl.allow("a", later) {
			t.Fatalf("Request %d nach der Pause hätte durchgehen müssen", i+1)
		}
	}
	if rl.allow("a", later) {
		t.Fatal("Guthaben darf nicht über den Burst hinauswachsen")
	}
}

// The limiter must not itself become a memory leak — and whoever fills up
// the table must not thereby unblock themselves.
func TestRateLimiterBoundsMemory(t *testing.T) {
	rl := newRateLimiter(60, 1)
	rl.maxKeys = 32
	now := time.Now()
	for i := range 500 {
		rl.allow(string(rune('a'+i%26))+string(rune('a'+i/26)), now)
	}
	if len(rl.buckets) > rl.maxKeys {
		t.Fatalf("%d Buckets über dem Limit von %d", len(rl.buckets), rl.maxKeys)
	}
}

func TestRateLimiterMiddlewareReturns429(t *testing.T) {
	rl := newRateLimiter(60, 1)
	h := rl.middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	req := httptest.NewRequest(http.MethodPost, "/api/auth/login/begin", nil)
	req.Header.Set("X-Real-IP", "203.0.113.9")

	first := httptest.NewRecorder()
	h.ServeHTTP(first, req)
	if first.Code != http.StatusNoContent {
		t.Fatalf("erster Request: %d", first.Code)
	}
	second := httptest.NewRecorder()
	h.ServeHTTP(second, req)
	if second.Code != http.StatusTooManyRequests {
		t.Fatalf("zweiter Request: %d, erwartet 429", second.Code)
	}
	if second.Header().Get("Retry-After") == "" {
		t.Error("429 ohne Retry-After")
	}
}

// An IPv6 connection usually gets a whole /64. Counted per address,
// everyone would have billions of their own buckets and the limiter would
// be useless.
func TestClientIPGroupsIPv6By64(t *testing.T) {
	key := func(realIP, remote string) string {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.RemoteAddr = remote
		if realIP != "" {
			req.Header.Set("X-Real-IP", realIP)
		}
		return clientIP(req)
	}
	a := key("2001:db8:1:2:aaaa::1", "10.0.0.2:1")
	b := key("2001:db8:1:2:bbbb:cccc:dddd:2", "10.0.0.2:1")
	if a != b {
		t.Fatalf("Adressen aus demselben /64 zählen getrennt: %q vs %q", a, b)
	}
	if other := key("2001:db8:1:3::1", "10.0.0.2:1"); other == a {
		t.Fatalf("ein anderes /64 darf nicht mitgezählt werden: %q", other)
	}
	if got := key("", "[2001:db8:5::7]:443"); got != "2001:db8:5::/64" {
		t.Fatalf("RemoteAddr ohne Proxy: %q", got)
	}
	if got := key("::ffff:203.0.113.9", "10.0.0.2:1"); got != "::ffff:203.0.113.9" {
		t.Fatalf("IPv4-in-IPv6 ist eine einzelne IPv4-Adresse: %q", got)
	}
}

func TestClientIPPrefersRealIPHeader(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "10.0.0.2:54321"
	if got := clientIP(req); got != "10.0.0.2" {
		t.Fatalf("ohne Proxy-Header: %q", got)
	}
	req.Header.Set("X-Real-IP", "203.0.113.9")
	if got := clientIP(req); got != "203.0.113.9" {
		t.Fatalf("mit X-Real-IP: %q", got)
	}
}

// X-Real-IP is set by nginx. If the connection doesn't come from a private
// or local address, there's no proxy in front of it (e.g. the port was
// accidentally exposed publicly) — then the header is made up and doesn't
// count.
func TestClientIPIgnoresRealIPFromPublicPeer(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "203.0.113.50:5555"
	req.Header.Set("X-Real-IP", "198.51.100.1")
	if got := clientIP(req); got != "203.0.113.50" {
		t.Fatalf("X-Real-IP von öffentlicher Gegenstelle übernommen: %q", got)
	}
	for _, proxy := range []string{"127.0.0.1:1", "[::1]:1", "10.0.0.2:1", "172.17.0.1:1", "192.168.1.2:1", "[fd00::1]:1"} {
		req.RemoteAddr = proxy
		if got := clientIP(req); got != "198.51.100.1" {
			t.Fatalf("Proxy %s: X-Real-IP ignoriert (%q)", proxy, got)
		}
	}
}
