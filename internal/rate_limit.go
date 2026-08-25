package internal

import (
	"net"
	"net/http"
	"os"
	"strconv"
	"sync"
	"time"

	syncContext "github.com/brave/go-sync/context"
	"github.com/rs/zerolog"
)

// ----------------------------------------
// Environment configuration
// ----------------------------------------

// rateLimitConfig carries the tunable limits, parsed from LITESYNC_* env vars.
// A rate <= 0 disables that dimension (convenient for local dev).
type rateLimitConfig struct {
	ipRate      float64
	ipBurst     float64
	clientRate  float64
	clientBurst float64
	idleTTL     time.Duration
	sweepTick   time.Duration
}

const (
	sweepTick      = 1 * time.Minute
	idleTTL        = 10 * time.Minute
	defaultIPRate  = 30.0 // sustained req/sec per source IP
	defaultIPBurst = 90.0 // burst per source IP
	defaultClRate  = 5.0  // sustained req/sec per client_id (seed)
	defaultClBurst = 20.0 // burst per client_id
)

func envFloat(name string, def float64) float64 {
	raw, ok := os.LookupEnv(name)
	if !ok || raw == "" {
		return def
	}
	f, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		return def
	}
	return f
}

func loadRateLimitConfig() rateLimitConfig {
	return rateLimitConfig{
		ipRate:      envFloat("LITESYNC_IP_RATE", defaultIPRate),
		ipBurst:     envFloat("LITESYNC_IP_BURST", defaultIPBurst),
		clientRate:  envFloat("LITESYNC_CLIENT_RATE", defaultClRate),
		clientBurst: envFloat("LITESYNC_CLIENT_BURST", defaultClBurst),
		idleTTL:     idleTTL,
		sweepTick:   sweepTick,
	}
}

// ----------------------------------------
// Token bucket
// ----------------------------------------

type tokenBucket struct {
	mu     sync.Mutex
	rate   float64
	burst  float64
	tokens float64
	last   time.Time
}

func newTokenBucket(rate, burst float64) *tokenBucket {
	return &tokenBucket{rate: rate, burst: burst, tokens: burst, last: time.Now()}
}

// take consumes one token if available. rate<=0 disables (always allows).
func (b *tokenBucket) take() bool {
	if b.rate <= 0 {
		return true
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	now := time.Now()
	b.tokens += now.Sub(b.last).Seconds() * b.rate
	if b.tokens > b.burst {
		b.tokens = b.burst
	}
	b.last = now
	if b.tokens >= 1 {
		b.tokens--
		return true
	}
	return false
}

// ---------------------------------------------
// Keyed limiter registry
// ---------------------------------------------

type keyedEntry struct {
	bucket   *tokenBucket
	lastSeen time.Time
}

type keyedLimiter struct {
	mu        sync.Mutex
	entries   map[string]*keyedEntry
	rate      float64
	burst     float64
	idleTTL   time.Duration
	sweepTick time.Duration
	closeCh   chan struct{}
	sweepWg   sync.WaitGroup
	started   bool
}

func newKeyedLimiter(rate, burst float64, idleTTL, sweepTick time.Duration) *keyedLimiter {
	return &keyedLimiter{
		entries:   make(map[string]*keyedEntry),
		rate:      rate,
		burst:     burst,
		idleTTL:   idleTTL,
		sweepTick: sweepTick,
		closeCh:   make(chan struct{}),
	}
}

func (l *keyedLimiter) start() {
	if l.started {
		return
	}
	l.started = true
	if l.sweepTick <= 0 {
		return
	}
	l.sweepWg.Add(1)
	go func() {
		defer l.sweepWg.Done()
		t := time.NewTicker(l.sweepTick)
		defer t.Stop()
		for {
			select {
			case <-l.closeCh:
				return
			case now := <-t.C:
				l.sweep(now)
			}
		}
	}()
}

func (l *keyedLimiter) stop() {
	if !l.started {
		return
	}
	close(l.closeCh)
	l.sweepWg.Wait()
}

func (l *keyedLimiter) sweep(now time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for k, e := range l.entries {
		if now.Sub(e.lastSeen) > l.idleTTL {
			delete(l.entries, k)
		}
	}
}

// allow checks a key, creating its bucket on first use. rate<=0 disables.
func (l *keyedLimiter) allow(key string) bool {
	if l.rate <= 0 {
		return true
	}
	l.mu.Lock()
	e, ok := l.entries[key]
	if !ok {
		e = &keyedEntry{bucket: newTokenBucket(l.rate, l.burst)}
		l.entries[key] = e
	}
	e.lastSeen = time.Now()
	allowed := e.bucket.take()
	l.mu.Unlock()
	return allowed
}

// ---------------------------------------------
// Middleware
// ---------------------------------------------

type rateLimiter struct {
	ip     *keyedLimiter
	client *keyedLimiter // keyed by Brave client_id (seed identity)
	logger *zerolog.Logger
}

func newRateLimiter(cfg rateLimitConfig, logger *zerolog.Logger) *rateLimiter {
	ip := newKeyedLimiter(cfg.ipRate, cfg.ipBurst, cfg.idleTTL, cfg.sweepTick)
	client := newKeyedLimiter(cfg.clientRate, cfg.clientBurst, cfg.idleTTL, cfg.sweepTick)
	ip.start()
	client.start()
	return &rateLimiter{ip: ip, client: client, logger: logger}
}

// middleware returns a chi-compatible handler enforcing per-IP and per-client_id
// limits. The client_id comes from the context set by the Brave sync Auth
// middleware, so this must run AFTER syncMiddleware.Auth. Denied -> HTTP 429.
// close stops the background sweepers. Safe to call on shutdown.
func (rl *rateLimiter) close() {
	rl.ip.stop()
	rl.client.stop()
}

func (rl *rateLimiter) middleware() func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ip := clientIP(r)

			if !rl.ip.allow(ip) {
				if rl.logger != nil {
					rl.logger.Warn().Str("ip", ip).Msg("rate limited by IP")
				}
				writeTooMany(w)
				return
			}

			// Per-identity limit. Fall back to the IP when no authenticated
			// client id is available (unauthenticated requests are rejected by
			// Auth before reaching here in practice).
			key := ip
			if id, ok := r.Context().Value(syncContext.ContextKeyClientID).(string); ok && id != "" {
				key = id
			}
			if !rl.client.allow(key) {
				if rl.logger != nil {
					rl.logger.Warn().Str("client_id", key).Msg("rate limited by client_id")
				}
				writeTooMany(w)
				return
			}

			next.ServeHTTP(w, r)
		})
	}
}

func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func writeTooMany(w http.ResponseWriter) {
	w.Header().Set("Retry-After", "1")
	http.Error(w, http.StatusText(http.StatusTooManyRequests), http.StatusTooManyRequests)
}
