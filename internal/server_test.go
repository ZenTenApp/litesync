package internal

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
	chiware "github.com/go-chi/chi/v5/middleware"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/hlog"
)

func TestLoadLogLevel(t *testing.T) {
	tests := []struct {
		env  string
		want zerolog.Level
	}{
		{"", zerolog.InfoLevel},
		{"info", zerolog.InfoLevel},
		{"debug", zerolog.DebugLevel},
		{"DEBUG", zerolog.DebugLevel},
		{" warn ", zerolog.WarnLevel},
		{"error", zerolog.ErrorLevel},
		{"nonsense", zerolog.InfoLevel},
	}

	for _, tt := range tests {
		t.Run(tt.env, func(t *testing.T) {
			t.Setenv("LITESYNC_LOG_LEVEL", tt.env)
			if got := loadLogLevel(); got != tt.want {
				t.Fatalf("loadLogLevel() with %q = %v, want %v", tt.env, got, tt.want)
			}
		})
	}
}

// newLoggingTestRouter wires the same hlog/recoverer/access-log stack the real
// server uses, writing to buf, and exposes handlers for each status class.
func newLoggingTestRouter(buf *bytes.Buffer) http.Handler {
	router := chi.NewRouter()
	router.Use(hlog.NewHandler(zerolog.New(buf)))
	router.Use(hlog.UserAgentHandler("user_agent"))
	router.Use(hlog.RequestIDHandler("req_id", ""))
	router.Use(hlog.AccessHandler(logRequest))
	router.Use(chiware.Recoverer)

	router.HandleFunc("/ok", func(w http.ResponseWriter, _ *http.Request) {})
	router.HandleFunc("/denied", func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "denied", http.StatusUnauthorized)
	})
	router.HandleFunc("/broken", func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "broken", http.StatusInternalServerError)
	})
	router.HandleFunc("/panic", func(http.ResponseWriter, *http.Request) {
		panic("boom")
	})

	return router
}

// accessEntry performs a request and returns the decoded access-log line.
func accessEntry(t *testing.T, buf *bytes.Buffer, path string) map[string]any {
	t.Helper()
	buf.Reset()

	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.Header.Set("User-Agent", "test-agent")
	rec := httptest.NewRecorder()
	newLoggingTestRouter(buf).ServeHTTP(rec, req)

	var entry map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(buf.Bytes()), &entry); err != nil {
		t.Fatalf("access log line is not single-line JSON: %v (got %q)", err, buf.String())
	}
	return entry
}

func TestLogRequestFields(t *testing.T) {
	var buf bytes.Buffer
	entry := accessEntry(t, &buf, "/ok")

	want := map[string]any{
		"level":      "info",
		"message":    "request",
		"method":     "GET",
		"path":       "/ok",
		"status":     float64(http.StatusOK),
		"user_agent": "test-agent",
	}
	for key, value := range want {
		if entry[key] != value {
			t.Errorf("field %q = %v, want %v", key, entry[key], value)
		}
	}
	if entry["req_id"] == "" || entry["req_id"] == nil {
		t.Error("req_id should be generated for every request")
	}
	if _, ok := entry["duration"]; !ok {
		t.Error("duration missing from access log")
	}
	if _, ok := entry["client_ip"]; !ok {
		t.Error("client_ip missing from access log")
	}
}

func TestLogRequestLevelFollowsStatus(t *testing.T) {
	tests := []struct {
		path  string
		level string
	}{
		{"/ok", "info"},
		{"/denied", "warn"},
		{"/broken", "error"},
		// A panic is recovered into a 500, so it must be logged as an error.
		{"/panic", "error"},
	}

	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			var buf bytes.Buffer
			entry := accessEntry(t, &buf, tt.path)

			if entry["level"] != tt.level {
				t.Errorf("level = %v, want %v", entry["level"], tt.level)
			}

			wantStatus := float64(http.StatusOK)
			if tt.path != "/ok" {
				wantStatus = float64(statusFor(t, tt.path))
			}
			if entry["status"] != wantStatus {
				t.Errorf("status = %v, want %v", entry["status"], wantStatus)
			}
		})
	}
}

func statusFor(t *testing.T, path string) int {
	t.Helper()
	switch path {
	case "/denied":
		return http.StatusUnauthorized
	case "/broken", "/panic":
		return http.StatusInternalServerError
	}
	t.Fatalf("no expected status for %s", path)
	return 0
}
