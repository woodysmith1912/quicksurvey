package web

import (
	"net"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"time"

	"golang.org/x/time/rate"
)

func netipParse(s string) (netip.Addr, error) { return netip.ParseAddr(s) }

// limiter is a per-client token bucket set.
//
// The algorithm is x/time/rate rather than something written here. The part
// that does need care is eviction: a plain map from client key to limiter is
// itself an unbounded allocation reachable by an anonymous caller, which is the
// problem this type exists to solve. Buckets idle longer than ttl are swept,
// and the map has a hard ceiling beyond which new keys are refused rather than
// admitted — under a flood from many addresses, failing closed for newcomers is
// better than growing without limit.
type limiter struct {
	every rate.Limit
	burst int
	ttl   time.Duration
	max   int

	mu      sync.Mutex
	buckets map[string]*bucket
	swept   time.Time
}

type bucket struct {
	lim  *rate.Limiter
	seen time.Time
}

const (
	limiterTTL = 15 * time.Minute
	limiterMax = 20_000 // ~2MB of buckets, far beyond any real client count
	sweepEvery = 2 * time.Minute
)

// newLimiter allows n events per window per client, with a burst of n.
func newLimiter(n int, window time.Duration) *limiter {
	return &limiter{
		every:   rate.Every(window / time.Duration(n)),
		burst:   n,
		ttl:     limiterTTL,
		max:     limiterMax,
		buckets: make(map[string]*bucket),
		swept:   time.Now(),
	}
}

// ok reports whether this client has budget left, without spending any.
//
// Splitting the check from the charge is what lets the sign-in path bill only
// failures. Charging every attempt would throttle a legitimate office where
// several people sign in from one address, while an attacker — who produces
// nothing but failures — is billed either way.
func (l *limiter) ok(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	b, exists := l.buckets[key]
	if !exists {
		return len(l.buckets) < l.max
	}
	b.seen = time.Now()
	return b.lim.Tokens() >= 1
}

// spend charges one event against this client, creating the bucket if needed.
func (l *limiter) spend(key string) { l.allow(key) }

// allow reports whether this client may proceed now, and charges for it.
func (l *limiter) allow(key string) bool {
	now := time.Now()
	l.mu.Lock()
	defer l.mu.Unlock()

	if now.Sub(l.swept) > sweepEvery {
		for k, b := range l.buckets {
			if now.Sub(b.seen) > l.ttl {
				delete(l.buckets, k)
			}
		}
		l.swept = now
	}

	b, ok := l.buckets[key]
	if !ok {
		if len(l.buckets) >= l.max {
			// Refuse rather than grow. A client turned away here retries
			// after the next sweep; the alternative is an anonymous caller
			// choosing how much memory this process uses.
			return false
		}
		b = &bucket{lim: rate.NewLimiter(l.every, l.burst)}
		l.buckets[key] = b
	}
	b.seen = now
	return b.lim.Allow()
}

// clientKey identifies the caller for rate-limiting purposes.
//
// Behind a proxy every request arrives from the proxy's address, so limiting on
// RemoteAddr would put the whole internet in one bucket. The forwarded headers
// are therefore used — but only when trustProxy says something in front is
// overwriting them. A caller can otherwise set X-Forwarded-For themselves and
// get a fresh bucket per request, which is worse than no limit at all because
// it looks like one.
func clientKey(r *http.Request, trustProxy bool) string {
	if trustProxy {
		if ip := r.Header.Get("X-Real-Ip"); ip != "" {
			return canonicalIP(ip)
		}
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			return canonicalIP(strings.Split(xff, ",")[0])
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return canonicalIP(r.RemoteAddr)
	}
	return canonicalIP(host)
}

// canonicalIP normalises an address so that two spellings of one client cannot
// buy two buckets. IPv6 clients are grouped by /64, because a single subscriber
// is routinely handed that whole range and would otherwise have 2^64 of them.
func canonicalIP(s string) string {
	s = strings.TrimSpace(s)
	ip, err := netipParse(s)
	if err != nil {
		return s
	}
	if ip.Is4() || ip.Is4In6() {
		return ip.Unmap().String()
	}
	pref, err := ip.Prefix(64)
	if err != nil {
		return ip.String()
	}
	return pref.String()
}

// tooMany writes the refusal. Retry-After is advisory but tells a well-behaved
// client what to do, and the message says which limit was hit.
func (s *Server) tooMany(w http.ResponseWriter, r *http.Request, msg string) {
	w.Header().Set("Retry-After", "60")
	s.fail(w, r, http.StatusTooManyRequests, errTooMany(msg))
}

type errTooMany string

func (e errTooMany) Error() string { return string(e) }
