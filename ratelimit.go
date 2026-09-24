package main

import (
	"net"
	"net/http"
	"net/netip"
	"sync"
	"time"
)

// rateLimiter is a token bucket per client IP. The goal isn't to defend
// against a botnet attack, but to keep a single source from choking the
// app with requests — with SetMaxOpenConns(1) it otherwise doesn't take
// much, and every request behind requireAuth makes a session query.
type rateLimiter struct {
	mu      sync.Mutex
	buckets map[string]*bucket
	refill  time.Duration // time until a token regenerates
	burst   float64
	maxKeys int
}

type bucket struct {
	tokens float64
	last   time.Time
}

func newRateLimiter(perMinute, burst int) *rateLimiter {
	return &rateLimiter{
		buckets: map[string]*bucket{},
		refill:  time.Minute / time.Duration(perMinute),
		burst:   float64(burst),
		// Cap against the limiter itself becoming a memory leak
		maxKeys: 4096,
	}
}

func (rl *rateLimiter) allow(key string, now time.Time) bool {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	b, ok := rl.buckets[key]
	if !ok {
		if len(rl.buckets) >= rl.maxKeys {
			rl.evict(now)
		}
		b = &bucket{tokens: rl.burst, last: now}
		rl.buckets[key] = b
	}
	b.tokens = min(rl.burst, b.tokens+now.Sub(b.last).Seconds()/rl.refill.Seconds())
	b.last = now
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

// evict cleans up: first anything that would be full again anyway,
// otherwise the oldest entry. Never clear the whole table — that would be
// a free pass for whoever just filled it up.
func (rl *rateLimiter) evict(now time.Time) {
	full := time.Duration(rl.burst) * rl.refill
	for k, b := range rl.buckets {
		if now.Sub(b.last) >= full {
			delete(rl.buckets, k)
		}
	}
	if len(rl.buckets) < rl.maxKeys {
		return
	}
	var oldestKey string
	var oldest time.Time
	for k, b := range rl.buckets {
		if oldestKey == "" || b.last.Before(oldest) {
			oldestKey, oldest = k, b.last
		}
	}
	delete(rl.buckets, oldestKey)
}

func (rl *rateLimiter) middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !rl.allow(clientIP(r), time.Now()) {
			w.Header().Set("Retry-After", "60")
			writeError(w, r, http.StatusTooManyRequests, "error.rate_limited")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// limit is the shortcut for individual routes.
func (rl *rateLimiter) limit(h http.HandlerFunc) http.HandlerFunc {
	return rl.middleware(h).ServeHTTP
}

// clientIP: nginx sets X-Real-IP to $remote_addr, overwriting any header
// sent along with the request. It's trusted only if the connection comes
// from a private or local address — that's how nginx on the host reaches
// the container. If the port is accidentally exposed publicly, otherwise
// everyone could decide for themselves whose rate limit they consume.
//
// IPv6 is counted per /64: that's the size of network a single connection
// typically gets. Counting per address would give everyone billions of
// their own buckets — pointless.
func clientIP(r *http.Request) string {
	ip := r.RemoteAddr
	if host, _, err := net.SplitHostPort(ip); err == nil {
		ip = host
	}
	if real := r.Header.Get("X-Real-IP"); real != "" {
		if peer, err := netip.ParseAddr(ip); err == nil && (peer.IsLoopback() || peer.IsPrivate()) {
			ip = real
		}
	}
	if a, err := netip.ParseAddr(ip); err == nil && a.Is6() && !a.Is4In6() {
		if p, err := a.Prefix(64); err == nil {
			return p.String()
		}
	}
	return ip
}
