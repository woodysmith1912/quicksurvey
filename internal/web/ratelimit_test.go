package web

import (
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/woodysmith1912/quicksurvey/internal/store"
)

func TestLimiterAllowsBurstThenRefuses(t *testing.T) {
	l := newLimiter(5, time.Minute)
	for i := range 5 {
		if !l.allow("a") {
			t.Fatalf("request %d of the burst was refused", i+1)
		}
	}
	if l.allow("a") {
		t.Error("the sixth request in a 5-per-minute window was allowed")
	}
	// A different client is unaffected.
	if !l.allow("b") {
		t.Error("one client exhausting its bucket blocked another")
	}
}

// The map of buckets is itself reachable by an anonymous caller, so it has to
// be bounded. This is the bug the type exists to avoid, not a nicety.
func TestLimiterMemoryIsBounded(t *testing.T) {
	l := newLimiter(1, time.Minute)
	l.max = 100
	for i := range 500 {
		l.allow(fmt.Sprintf("client-%d", i))
	}
	l.mu.Lock()
	n := len(l.buckets)
	l.mu.Unlock()
	if n > l.max {
		t.Errorf("buckets = %d, want at most %d — the map grows without limit", n, l.max)
	}
}

func TestLimiterSweepsIdleBuckets(t *testing.T) {
	l := newLimiter(5, time.Minute)
	l.ttl = 10 * time.Millisecond
	l.allow("old")
	time.Sleep(20 * time.Millisecond)
	l.swept = time.Now().Add(-2 * sweepEvery) // force a sweep on the next call
	l.allow("new")

	l.mu.Lock()
	_, stillThere := l.buckets["old"]
	l.mu.Unlock()
	if stillThere {
		t.Error("an idle bucket survived the sweep")
	}
}

func TestClientKeyHonoursTrustProxy(t *testing.T) {
	req := func(hdr map[string]string) *http.Request {
		r := httptest.NewRequest("POST", "/login", nil)
		r.RemoteAddr = "10.0.0.1:1234"
		for k, v := range hdr {
			r.Header.Set(k, v)
		}
		return r
	}
	// Trusted: the forwarded address wins, so clients behind a proxy get
	// separate buckets.
	if got := clientKey(req(map[string]string{"X-Real-Ip": "203.0.113.7"}), true); got != "203.0.113.7" {
		t.Errorf("trusted X-Real-Ip = %q", got)
	}
	if got := clientKey(req(map[string]string{"X-Forwarded-For": "203.0.113.7, 10.0.0.9"}), true); got != "203.0.113.7" {
		t.Errorf("trusted X-Forwarded-For = %q", got)
	}
	// Untrusted: the header is ignored, or a caller would mint a fresh bucket
	// per request and the limit would be theatre.
	if got := clientKey(req(map[string]string{"X-Forwarded-For": "203.0.113.7"}), false); got != "10.0.0.1" {
		t.Errorf("untrusted forwarded header was believed: %q", got)
	}
}

// A single IPv6 subscriber is routinely handed a /64. Keying on the full
// address would give one client 2^64 buckets.
func TestIPv6ClientsAreGroupedBySubnet(t *testing.T) {
	a := canonicalIP("2001:db8:1:2::1")
	b := canonicalIP("2001:db8:1:2::dead:beef")
	if a != b {
		t.Errorf("addresses in one /64 got different keys: %q vs %q", a, b)
	}
	if c := canonicalIP("2001:db8:1:3::1"); c == a {
		t.Error("different /64s collapsed to one key")
	}
	// IPv4 is exact, including when written as v4-in-v6.
	if got := canonicalIP("::ffff:203.0.113.7"); got != "203.0.113.7" {
		t.Errorf("v4-in-v6 = %q, want 203.0.113.7", got)
	}
}

// A load test from one address is indistinguishable from an attack, so there
// has to be a way to take the limiter out of the measurement.
func TestLimitersCanBeDisabled(t *testing.T) {
	var l *limiter = newLimiter(-1, time.Minute)
	if l != nil {
		t.Fatal("a negative rate should produce no limiter at all")
	}
	// Every method has to be nil-safe, or disabling one panics at the first
	// request rather than at start-up.
	for i := range 10_000 {
		if !l.allow("anyone") {
			t.Fatalf("a disabled limiter refused request %d", i)
		}
		if !l.ok("anyone") {
			t.Fatalf("a disabled limiter reported no budget at %d", i)
		}
		l.spend("anyone")
	}
}

func TestZeroRateMeansUnsetNotUnlimited(t *testing.T) {
	// Leaving a field at its zero value must not silently disable the limit;
	// disabling has to be asked for.
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	s, err := New(st, Config{Location: time.UTC, Logger: slog.New(slog.DiscardHandler)})
	if err != nil {
		t.Fatal(err)
	}
	if s.loginLimit == nil || s.voterLimit == nil {
		t.Fatal("an unconfigured Config disabled rate limiting")
	}
	if s.cfg.LoginRate != DefaultLoginRate || s.cfg.VoterRate != DefaultVoterRate {
		t.Errorf("defaults not applied: login=%d voter=%d", s.cfg.LoginRate, s.cfg.VoterRate)
	}

	off, err := New(st, Config{LoginRate: -1, VoterRate: -1, Location: time.UTC,
		Logger: slog.New(slog.DiscardHandler)})
	if err != nil {
		t.Fatal(err)
	}
	if off.loginLimit != nil || off.voterLimit != nil {
		t.Error("-1 did not disable the limiters")
	}
}
