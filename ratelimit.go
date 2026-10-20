package main

import (
	"context"
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

// userSlots caps how many long-running requests of one kind (uploads,
// imports, SSE streams) a person may have open at the same time — one
// account must not be able to tie up memory or goroutines for everyone.
type userSlots struct {
	mu  sync.Mutex
	n   map[int64]int
	max int
}

func newUserSlots(max int) *userSlots { return &userSlots{n: map[int64]int{}, max: max} }

func (u *userSlots) acquire(uid int64) bool {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.n[uid] >= u.max {
		return false
	}
	u.n[uid]++
	return true
}

func (u *userSlots) release(uid int64) {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.n[uid]--; u.n[uid] <= 0 {
		delete(u.n, uid)
	}
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
			// Only an address counts — otherwise a key as large as the header
			// limit. A proxy that passes the client's header through unchanged
			// (Caddy without header_up, see README) still lets every client
			// pick its own address; only the proxy can fix that.
			if a, err := netip.ParseAddr(real); err == nil {
				ip = a.String()
			}
		}
	}
	if a, err := netip.ParseAddr(ip); err == nil && a.Is6() && !a.Is4In6() {
		if p, err := a.Prefix(64); err == nil {
			return p.String()
		}
	}
	return ip
}

// waitSlots: like userSlots, but a request waits for its turn instead of
// being turned away — for what the app requests on its own (the list after
// every change, the image the lightbox shows), where a 429 would only
// break the page. perUser per person, total across everyone; the wait ends
// with the request's context (client gone).
type waitSlots struct {
	mu      sync.Mutex
	users   map[int64]*userQueue
	perUser int
	total   chan struct{}
}

type userQueue struct {
	slots chan struct{}
	refs  int // holding or waiting — the entry goes once nobody is left
}

func newWaitSlots(perUser, total int) *waitSlots {
	return &waitSlots{users: map[int64]*userQueue{}, perUser: perUser, total: make(chan struct{}, total)}
}

// acquire waits for a slot of uid, then for one overall. If ok, release
// must be called exactly once.
func (ws *waitSlots) acquire(ctx context.Context, uid int64) (release func(), ok bool) {
	// select picks at random among ready cases: a request whose client is
	// already gone could otherwise still take a free slot.
	if ctx.Err() != nil {
		return nil, false
	}
	ws.mu.Lock()
	q := ws.users[uid]
	if q == nil {
		q = &userQueue{slots: make(chan struct{}, ws.perUser)}
		ws.users[uid] = q
	}
	q.refs++
	ws.mu.Unlock()
	leave := func() {
		ws.mu.Lock()
		if q.refs--; q.refs == 0 {
			delete(ws.users, uid)
		}
		ws.mu.Unlock()
	}
	select {
	case q.slots <- struct{}{}:
	case <-ctx.Done():
		leave()
		return nil, false
	}
	select {
	case ws.total <- struct{}{}:
	case <-ctx.Done():
		<-q.slots
		leave()
		return nil, false
	}
	return func() {
		<-ws.total
		<-q.slots
		leave()
	}, true
}
