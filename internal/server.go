package internal

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	appctx "github.com/brave-intl/bat-go/libs/context"
	"github.com/brave-intl/bat-go/libs/logging"
	"github.com/brave/go-sync/cache"
	syncContext "github.com/brave/go-sync/context"
	"github.com/brave/go-sync/controller"
	syncMiddleware "github.com/brave/go-sync/middleware"
	"github.com/go-chi/chi/v5"
	chiware "github.com/go-chi/chi/v5/middleware"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/hlog"
	zlog "github.com/rs/zerolog/log"
)

const (
	defaultTimeout  = 60 * time.Second
	shutdownTimeout = 30 * time.Second
)

// StartServer initializes and starts the HTTP server with graceful shutdown handling.
func StartServer(bindAddr, dbPath string) error {
	ctx := context.Background()
	ctx, logger := setupLogger(ctx)

	rateLimiter := newRateLimiter(loadRateLimitConfig(), logger)
	defer rateLimiter.close()

	ctx, router, err := setupRouter(ctx, logger, dbPath, rateLimiter)
	if err != nil {
		return fmt.Errorf("failed to setup router: %w", err)
	}

	server := &http.Server{
		Addr:    bindAddr,
		Handler: router,
		BaseContext: func(net.Listener) context.Context {
			return ctx
		},
	}

	// Set up signal handling for graceful shutdown
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

	// Bind the listener first so we can confirm the port is open before logging ready
	listener, err := net.Listen("tcp", bindAddr)
	if err != nil {
		return fmt.Errorf("failed to bind %s: %w", bindAddr, err)
	}

	addr := listener.Addr().(*net.TCPAddr)
	host := addr.IP.String()
	if host == "::" || host == "0.0.0.0" {
		host = "localhost"
	}
	port := addr.Port

	// Start server in a goroutine so we can listen for signals concurrently
	errChan := make(chan error, 1)
	go func() {
		errChan <- server.Serve(listener)
	}()

	logger.Info().
		Str("address", bindAddr).
		Str("db", dbPath).
		Msg("litesync server started")
	logger.Info().
		Str("sync_url", fmt.Sprintf("http://%s:%d/litesync", host, port)).
		Msg("Brave sync URL")
	logger.Info().
		Str("brave_flag", fmt.Sprintf("--sync-url=http://%s:%d/litesync", host, port)).
		Msg("Launch Brave with this flag to use this server")

	// Wait for either server error or shutdown signal
	select {
	case err := <-errChan:
		if errors.Is(err, http.ErrServerClosed) {
			logger.Info().Msg("HTTP server closed gracefully")
			return nil
		}
		return fmt.Errorf("HTTP server error: %w", err)
	case sig := <-sigChan:
		logger.Info().Str("signal", sig.String()).Msg("Received shutdown signal")

		// Create shutdown context with timeout
		shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancel()

		// Attempt graceful shutdown
		if err := server.Shutdown(shutdownCtx); err != nil {
			logger.Error().Err(err).Msg("Failed to shutdown server gracefully, forcing close")
			return server.Close()
		}

		logger.Info().Msg("HTTP server shutdown complete")
		return nil
	}
}

// setupLogger configures the application logger with environment-specific settings.
func setupLogger(ctx context.Context) (context.Context, *zerolog.Logger) {
	ctx = context.WithValue(ctx, appctx.EnvironmentCTXKey, os.Getenv("ENV"))
	ctx = context.WithValue(ctx, appctx.LogLevelCTXKey, loadLogLevel())
	ctx, logger := logging.SetupLogger(ctx)

	// The upstream go-sync controller and middleware log through the zerolog
	// global logger, which ignores the request context. Adopt our configured
	// logger there too, so those lines share one stream, format and level
	// instead of going to stderr as unformatted JSON.
	zlog.Logger = *logger

	return ctx, logger
}

// loadLogLevel reads LITESYNC_LOG_LEVEL (debug, info, warn, error, ...) and
// defaults to info when it is unset, empty or unparseable.
func loadLogLevel() zerolog.Level {
	raw := strings.ToLower(strings.TrimSpace(os.Getenv("LITESYNC_LOG_LEVEL")))
	if raw == "" {
		return zerolog.InfoLevel
	}
	level, err := zerolog.ParseLevel(raw)
	if err != nil {
		return zerolog.InfoLevel
	}
	return level
}

// setupRouter configures the HTTP router with middleware and routes.
func setupRouter(ctx context.Context, logger *zerolog.Logger, dbPath string, rateLimiter *rateLimiter) (context.Context, chi.Router, error) {
	router := chi.NewRouter()

	// Middleware setup
	router.Use(chiware.RealIP)
	router.Use(chiware.Heartbeat("/"))

	if logger != nil {
		router.Use(hlog.NewHandler(*logger))
		router.Use(hlog.UserAgentHandler("user_agent"))
		router.Use(hlog.RequestIDHandler("req_id", "Request-Id"))
		// Access logging must wrap the rest of the stack so it can record the
		// final status and duration of every request.
		router.Use(hlog.AccessHandler(logRequest))
	}

	// Recoverer sits inside the access logger so a panic is turned into a 500
	// that the access line records, instead of an unlogged connection drop.
	router.Use(chiware.Recoverer)
	router.Use(chiware.Timeout(defaultTimeout))
	router.Use(bearerToken)
	router.Use(syncMiddleware.CommonResponseHeaders)

	// Data store initialization
	sqliteStore, err := NewSqliteDatastore(dbPath)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to create sqlite datastore: %w", err)
	}

	// Cache initialization
	cacheInstance := cache.NewCache(NewFakeRedisClient())

	// Context value injection
	ctx = context.WithValue(ctx, syncContext.ContextKeyDatastore, sqliteStore)
	ctx = context.WithValue(ctx, syncContext.ContextKeyCache, &cacheInstance)

	// Rate limiting middleware: per-IP and per-client_id token buckets, tunable
	// via LITESYNC_IP_RATE / LITESYNC_CLIENT_RATE env vars (rate_limit.go). It
	// MUST run after Auth so the authenticated client_id is in context.
	r := chi.NewRouter()
	r.Use(syncMiddleware.Auth)
	if rateLimiter != nil {
		r.Use(rateLimiter.middleware())
	}
	r.Use(syncMiddleware.DisabledChain)
	// Passwords-only + per-entity size policy. Runs before the controller so
	// rejected data never touches the database. It reads and re-serves the
	// request body, so it must sit before the terminal controller handler.
	r.Use(newEntityPolicy(loadEntityPolicyConfig()).middleware())
	r.Method("POST", "/command/", controller.Command(cacheInstance, sqliteStore))
	router.Mount("/litesync", r)

	return ctx, router, nil
}

// logRequest emits one structured access-log line per completed request. It
// uses the request-scoped logger installed by hlog, so fields added by earlier
// middleware (req_id, user_agent) are already attached.
func logRequest(r *http.Request, status, size int, duration time.Duration) {
	// A handler that writes no explicit status sends 200; hlog reports 0 there.
	if status == 0 {
		status = http.StatusOK
	}

	logger := hlog.FromRequest(r)

	var event *zerolog.Event
	switch {
	case status >= 500:
		event = logger.Error()
	case status >= 400:
		event = logger.Warn()
	default:
		event = logger.Info()
	}

	event.
		Str("method", r.Method).
		Str("path", r.URL.Path).
		Str("client_ip", clientIP(r)).
		Int("status", status).
		Int("bytes", size).
		Dur("duration", duration).
		Msg("request")
}

type bearerTokenKey struct{}

// BearerToken is a middleware that adds the bearer token included in a request's headers to context
func bearerToken(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var token string

		bearer := r.Header.Get("Authorization")

		if len(bearer) > 7 && strings.ToUpper(bearer[0:6]) == "BEARER" {
			token = bearer[7:]
		}
		ctx := context.WithValue(r.Context(), bearerTokenKey{}, token)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}
