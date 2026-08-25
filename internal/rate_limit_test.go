package internal

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	syncContext "github.com/brave/go-sync/context"
)

// burstConfig returns a config whose IP and client dimensions share the same
// burst. Refill rate is negligible (1e-9 t/s), so within a fast test the bucket
// only ever drains and never refills — the burst is the governing limit.
func burstConfig(burst float64) rateLimitConfig {
	return rateLimitConfig{
		ipRate:      1e-9,
		ipBurst:     burst,
		clientRate:  1e-9,
		clientBurst: burst,
		idleTTL:     time.Minute,
		sweepTick:   10 * time.Minute,
	}
}

// clientOnlyConfig: IP dimension effectively unlimited, client dimension burst
// governs. Used to isolate the per-client_id behaviour.
func clientOnlyConfig(burst float64) rateLimitConfig {
	return rateLimitConfig{
		ipRate:      1e-9,
		ipBurst:     1e9, // effectively unlimited IP burst of 1e9
		clientRate:  1e-9,
		clientBurst: burst,
		idleTTL:     time.Minute,
		sweepTick:   10 * time.Minute,
	}
}

func TestKeyedLimiter_RespitsBurst(t *testing.T) {
	l := newKeyedLimiter(1e-9, 2, time.Minute, 10*time.Minute)
	l.start()
	defer l.stop()

	if !l.allow("k") || !l.allow("k") {
		t.Fatal("first two calls should be allowed")
	}
	if l.allow("k") {
		t.Fatal("third call should be denied")
	}
	if !l.allow("other") {
		t.Fatal("independent key should have its own bucket")
	}
}

func TestKeyedLimiter_DisabledWhenRateZero(t *testing.T) {
	l := newKeyedLimiter(0, 0, time.Minute, 10*time.Minute)
	l.start()
	defer l.stop()
	for i := 0; i < 50; i++ {
		if !l.allow("x") {
			t.Fatal("disabled limiter must allow")
		}
	}
}

func TestRateLimitMiddleware_EnforcesBurst(t *testing.T) {
	rl := newRateLimiter(burstConfig(2), nil)
	defer rl.close()

	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	h := rl.middleware()(next)

	mk := func() int {
		r, _ := http.NewRequest("POST", "/", nil)
		r.RemoteAddr = "203.0.113.7:1234"
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w.Code
	}

	if code := mk(); code != http.StatusOK {
		t.Fatalf("1st should be OK, got %d", code)
	}
	if code := mk(); code != http.StatusOK {
		t.Fatalf("2nd should be OK, got %d", code)
	}
	if code := mk(); code != http.StatusTooManyRequests {
		t.Fatalf("3rd should be 429, got %d", code)
	}
}

func TestRateLimitMiddleware_ClientIDDimension(t *testing.T) {
	rl := newRateLimiter(clientOnlyConfig(1), nil)
	defer rl.close()

	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	h := rl.middleware()(next)

	mk := func(clientID string) int {
		r, _ := http.NewRequest("POST", "/", nil)
		r.RemoteAddr = "10.0.0.1:1000"
		ctx := context.WithValue(r.Context(), syncContext.ContextKeyClientID, clientID)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r.WithContext(ctx))
		return w.Code
	}

	if code := mk("seed-1"); code != http.StatusOK {
		t.Fatalf("1st seed-1 should be OK, got %d", code)
	}
	if code := mk("seed-1"); code != http.StatusTooManyRequests {
		t.Fatalf("2nd seed-1 should be 429, got %d", code)
	}
	if code := mk("seed-2"); code != http.StatusOK {
		t.Fatalf("seed-2 should have its own bucket and be OK, got %d", code)
	}
}

func TestClientIDFromContext_EmptyFallsBackToIP(t *testing.T) {
	rl := newRateLimiter(burstConfig(1), nil)
	defer rl.close()

	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	h := rl.middleware()(next)

	r, _ := http.NewRequest("POST", "/", nil)
	r.RemoteAddr = "198.51.100.5:10" // no client id in context
	do := func() int {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w.Code
	}
	if do() != http.StatusOK {
		t.Fatal("1st should be OK")
	}
	if do() != http.StatusTooManyRequests {
		t.Fatal("2nd (same IP, no client) should be 429")
	}
}

func TestClientIP(t *testing.T) {
	// Real RemoteAddr always includes the port; IPv6 is always bracketed.
	r, _ := http.NewRequest("POST", "/", nil)
	r.RemoteAddr = "[2001:db8::1]:443"
	if ip := clientIP(r); ip != "2001:db8::1" {
		t.Fatalf("IPv6: got %q", ip)
	}
	r.RemoteAddr = "1.2.3.4:8080"
	if ip := clientIP(r); ip != "1.2.3.4" {
		t.Fatalf("IPv4: got %q", ip)
	}
	// Malformed input shouldn't panic.
	r.RemoteAddr = "not-a-host"
	if ip := clientIP(r); ip != "not-a-host" {
		t.Fatalf("malformed: got %q", ip)
	}
}
