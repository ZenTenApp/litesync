package internal

import (
	"bytes"
	"compress/gzip"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"

	"github.com/brave/go-sync/schema/protobuf/sync_pb"
	"google.golang.org/protobuf/proto"
)

// Entity data-type whitelist for this server.
//
// The server intentionally behaves as a "passwords-only" sync store:
//   - PASSWORD (data type 45873) is the product surface we store/sync.
//   - NIGORI (data type 47745) MUST be allowed too: it carries the chain's
//     encryption settings and is created by every client on connect(). Without
//     it, clients cannot initialise their sync chain at all.
//
// Every other Brave data type (bookmarks, history, autofill, 2FA/authenticator,
// preferences, ...) is rejected on commit. This shrinks the attack surface and
// storage for a password-focused server, and it blocks accidental or malicious
// upload of unrelated data types.
const (
	// entityPolicyMaxBody limits how much of the request we read for
	// validation; mirrors the controller's own 10MB cap so behaviour is
	// consistent.
	entityPolicyMaxBody = 10 * 1024 * 1024

	// defaultMaxPasswordSize is the upper bound (bytes) for a stored password
	// entity's marshalled `Specifics` blob — the object actually persisted.
	defaultMaxPasswordSize = 1024
)

// entityPolicyConfig is tunable via env.
type entityPolicyConfig struct {
	// maxPasswordSize caps the marshalled size of a password entity's
	// Specifics. 0 falls back to the default; a negative value disables the cap.
	maxPasswordSize int
}

func loadEntityPolicyConfig() entityPolicyConfig {
	return entityPolicyConfig{
		maxPasswordSize: envInt("LITESYNC_MAX_PASSWORD_SIZE", defaultMaxPasswordSize),
	}
}

func envInt(name string, def int) int {
	raw, ok := os.LookupEnv(name)
	if !ok || raw == "" {
		return def
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return def
	}
	return n
}

// entityPolicy enforces the passwords-only + size-capped commit policy.
type entityPolicy struct {
	cfg entityPolicyConfig
}

func newEntityPolicy(cfg entityPolicyConfig) *entityPolicy {
	return &entityPolicy{cfg: cfg}
}

// validateEntries inspects a commit's entities and returns an error describing
// the first policy violation. It runs BEFORE anything touches the database.
func (p *entityPolicy) validateEntries(entries []*sync_pb.SyncEntity) error {
	for _, e := range entries {
		if e == nil {
			continue
		}
		spec := e.GetSpecifics()
		if spec == nil || spec.GetSpecificsVariant() == nil {
			return fmt.Errorf("rejected: entity without specifics (only password sync allowed)")
		}

		switch spec.GetSpecificsVariant().(type) {
		case *sync_pb.EntitySpecifics_Password:
			if p.cfg.maxPasswordSize >= 0 {
				blob, err := proto.Marshal(spec)
				if err != nil {
					return fmt.Errorf("rejected: failed to measure password entity: %w", err)
				}
				if len(blob) > p.cfg.maxPasswordSize {
					return fmt.Errorf(
						"rejected: password entity too large (%d bytes > %d byte limit)",
						len(blob), p.cfg.maxPasswordSize)
				}
			}
		case *sync_pb.EntitySpecifics_Nigori:
			// Allowed: required for sync-chain initialisation.
		default:
			return fmt.Errorf(
				"rejected: unsupported data type (only password sync allowed)")
		}
	}
	return nil
}

// middleware returns a chi-compatible handler that validates COMMIT bodies
// before the sync controller processes them. Valid requests are passed through
// untouched (the original body bytes are re-served); rejected requests get 400.
func (p *entityPolicy) middleware() func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodPost {
				next.ServeHTTP(w, r)
				return
			}

			// Buffer the raw body so we can re-serve it verbatim to the
			// downstream controller (including its original gzip framing).
			raw, err := io.ReadAll(io.LimitReader(r.Body, entityPolicyMaxBody))
			if err != nil {
				http.Error(w, "failed to read request body", http.StatusBadRequest)
				return
			}
			_ = r.Body.Close()
			r.Body = io.NopCloser(bytes.NewReader(raw))
			r.ContentLength = int64(len(raw))

			// Parse (decompressing if needed) purely for validation.
			data := raw
			if r.Header.Get("Content-Encoding") == "gzip" {
				if gz, err := gzip.NewReader(bytes.NewReader(raw)); err == nil {
					if dec, err := io.ReadAll(io.LimitReader(gz, entityPolicyMaxBody)); err == nil {
						data = dec
					}
					_ = gz.Close()
				}
			}

			msg := &sync_pb.ClientToServerMessage{}
			if err := proto.Unmarshal(data, msg); err != nil {
				// Not our job to error on malformed bodies; let the controller
				// produce its standard error.
				next.ServeHTTP(w, r)
				return
			}

			// Only COMMIT messages carry entities to validate.
			if commit := msg.GetCommit(); commit != nil {
				if entries := commit.GetEntries(); len(entries) > 0 {
					if err := p.validateEntries(entries); err != nil {
						http.Error(w, err.Error(), http.StatusBadRequest)
						return
					}
				}
			}

			next.ServeHTTP(w, r)
		})
	}
}
