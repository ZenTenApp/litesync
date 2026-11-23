package internal

import (
	"bytes"
	"context"
	"fmt"
	"net/http"

	syncContext "github.com/brave/go-sync/context"
	"github.com/brave/go-sync/schema/protobuf/sync_pb"
	"google.golang.org/protobuf/proto"
)

// Per-seed (per Brave client_id) cap on the number of stored passwords.
//
// Rather than rejecting commits ourselves, we drive go-sync's own per-client
// object quota (command.go: maxClientObjectQuota). Before every create, the
// upstream commit handler checks
//
//	ItemCount + <creates so far in this commit> >= maxClientObjectQuota
//
// and answers that entry with CommitResponse_OVER_QUOTA, while still letting
// updates and deletes through. That quota is hard-coded upstream, so
// GetClientItemCount reports a shifted count:
//
//	ItemCount = livePasswords + (upstreamMaxClientObjectQuota - maxPasswords)
//
// which makes the upstream check trip exactly when a seed would exceed
// maxPasswords. The client gets a real OVER_QUOTA per entity instead of an
// HTTP error that would stall the whole commit.
//
// Upstream builds the OVER_QUOTA error message from that shifted count, so
// passwordQuotaMessages rewrites it with the real live password count.
//
// TestPasswordQuota_* pins this behaviour against the upstream handler, so a
// go-sync bump that changes the quota constant or its maths fails loudly.
const (
	// upstreamMaxClientObjectQuota mirrors the unexported
	// maxClientObjectQuota in github.com/brave/go-sync/command.
	upstreamMaxClientObjectQuota = 50000

	// defaultMaxPasswords is the default number of live (non-deleted)
	// passwords a single seed may store.
	defaultMaxPasswords = 120

	// passwordDataType is the EntitySpecifics field number for passwords,
	// which go-sync stores as the entity's data_type.
	passwordDataType = 45873
)

// loadMaxPasswords reads LITESYNC_MAX_PASSWORDS. A negative value disables the
// password cap (the upstream 50k object quota then applies on real counts).
func loadMaxPasswords() int {
	return envInt("LITESYNC_MAX_PASSWORDS", defaultMaxPasswords)
}

// quotaItemCount translates live counts into the ItemCount reported to the
// upstream quota check.
func quotaItemCount(maxPasswords, livePasswords, liveTotal int) int {
	if maxPasswords < 0 {
		return liveTotal
	}
	return livePasswords + (upstreamMaxClientObjectQuota - maxPasswords)
}

// commitRequestKey marks requests that carry a COMMIT with entries. The entity
// policy middleware sets it after parsing the body, so passwordQuotaMessages
// only buffers commit responses (never large GetUpdates responses).
type commitRequestKey struct{}

func markCommitRequest(ctx context.Context) context.Context {
	return context.WithValue(ctx, commitRequestKey{}, true)
}

func isCommitRequest(ctx context.Context) bool {
	v, _ := ctx.Value(commitRequestKey{}).(bool)
	return v
}

// passwordQuotaMessages rewrites the error message of OVER_QUOTA commit
// entries. Upstream builds that message from GetClientItemCount, which reports
// the shifted quota count ("There are already 50000 non-deleted objects in
// store"). The rewritten message states the seed's real live password count,
// read after the commit, and the configured limit.
//
// It must sit after the entity policy middleware, which marks commit requests.
func passwordQuotaMessages(ds *SqliteDatastore) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if ds.MaxPasswords < 0 || !isCommitRequest(r.Context()) {
				next.ServeHTTP(w, r)
				return
			}

			rec := &bufferedResponse{header: w.Header(), status: http.StatusOK}
			next.ServeHTTP(rec, r)

			body := rec.body.Bytes()
			if rec.status == http.StatusOK {
				clientID, _ := r.Context().Value(syncContext.ContextKeyClientID).(string)
				if rewritten, ok := rewriteOverQuotaMessages(ds, clientID, body); ok {
					body = rewritten
				}
			}

			w.WriteHeader(rec.status)
			_, _ = w.Write(body)
		})
	}
}

// rewriteOverQuotaMessages returns the re-marshalled response and true when it
// changed at least one OVER_QUOTA entry; otherwise the original is kept.
func rewriteOverQuotaMessages(ds *SqliteDatastore, clientID string, body []byte) ([]byte, bool) {
	rsp := &sync_pb.ClientToServerResponse{}
	if err := proto.Unmarshal(body, rsp); err != nil {
		return nil, false
	}

	var overQuota []*sync_pb.CommitResponse_EntryResponse
	for _, e := range rsp.GetCommit().GetEntryresponse() {
		if e.GetResponseType() == sync_pb.CommitResponse_OVER_QUOTA {
			overQuota = append(overQuota, e)
		}
	}
	if len(overQuota) == 0 {
		return nil, false
	}

	live, err := ds.CountLivePasswords(clientID)
	if err != nil {
		return nil, false
	}
	msg := fmt.Sprintf(
		"Password limit reached: this sync chain has %d of %d allowed passwords",
		live, ds.MaxPasswords)
	for _, e := range overQuota {
		e.ErrorMessage = proto.String(msg)
	}

	out, err := proto.Marshal(rsp)
	if err != nil {
		return nil, false
	}
	return out, true
}

// bufferedResponse captures a handler's status and body. Headers go straight
// to the real writer's header map.
type bufferedResponse struct {
	header http.Header
	status int
	body   bytes.Buffer
}

func (b *bufferedResponse) Header() http.Header         { return b.header }
func (b *bufferedResponse) Write(p []byte) (int, error) { return b.body.Write(p) }
func (b *bufferedResponse) WriteHeader(status int)      { b.status = status }
