package internal

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/brave/go-sync/cache"
	"github.com/brave/go-sync/command"
	syncContext "github.com/brave/go-sync/context"
	"github.com/brave/go-sync/controller"
	"github.com/brave/go-sync/schema/protobuf/sync_pb"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
)

// These tests drive go-sync's real commit handler against the SQLite store, so
// they pin the coupling to the upstream quota (constant and comparison).

const quotaTestClient = "seed-under-test"

type quotaHarness struct {
	t     *testing.T
	ds    *SqliteDatastore
	cache *cache.Cache
}

func newQuotaHarness(t *testing.T, maxPasswords int) *quotaHarness {
	t.Helper()
	ds, err := NewSqliteDatastore(":memory:")
	require.NoError(t, err)
	// :memory: is per-connection; keep the pool on a single connection.
	ds.Db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = ds.Db.Close() })
	ds.MaxPasswords = maxPasswords
	c := cache.NewCache(NewFakeRedisClient())
	return &quotaHarness{t: t, ds: ds, cache: c}
}

func quotaPassword(id string, version int64, deleted bool) *sync_pb.SyncEntity {
	return &sync_pb.SyncEntity{
		IdString: proto.String(id),
		Name:     proto.String(id),
		Version:  proto.Int64(version),
		Deleted:  proto.Bool(deleted),
		Folder:   proto.Bool(false),
		Specifics: &sync_pb.EntitySpecifics{
			SpecificsVariant: &sync_pb.EntitySpecifics_Password{
				Password: &sync_pb.PasswordSpecifics{
					Encrypted: &sync_pb.EncryptedData{Blob: proto.String(id)},
				},
			},
		},
	}
}

func quotaNewPasswords(prefix string, n int) []*sync_pb.SyncEntity {
	out := make([]*sync_pb.SyncEntity, n)
	for i := range out {
		out[i] = quotaPassword(fmt.Sprintf("%s-%d", prefix, i), 0, false)
	}
	return out
}

func (h *quotaHarness) commit(entries ...*sync_pb.SyncEntity) []*sync_pb.CommitResponse_EntryResponse {
	h.t.Helper()
	contents := sync_pb.ClientToServerMessage_COMMIT
	msg := &sync_pb.ClientToServerMessage{
		MessageContents: &contents,
		Commit: &sync_pb.CommitMessage{
			Entries:   entries,
			CacheGuid: proto.String("cache-guid"),
		},
	}
	rsp := &sync_pb.ClientToServerResponse{}
	require.NoError(h.t, command.HandleClientToServerMessage(h.cache, msg, rsp, h.ds, quotaTestClient))
	require.Equal(h.t, sync_pb.SyncEnums_SUCCESS, rsp.GetErrorCode())
	return rsp.GetCommit().GetEntryresponse()
}

func countResults(rsps []*sync_pb.CommitResponse_EntryResponse) (ok, over int) {
	for _, r := range rsps {
		switch r.GetResponseType() {
		case sync_pb.CommitResponse_SUCCESS:
			ok++
		case sync_pb.CommitResponse_OVER_QUOTA:
			over++
		}
	}
	return ok, over
}

func TestPasswordQuota_RejectsCreatesBeyondLimitInOneCommit(t *testing.T) {
	h := newQuotaHarness(t, 3)
	ok, over := countResults(h.commit(quotaNewPasswords("pw", 5)...))
	require.Equal(t, 3, ok)
	require.Equal(t, 2, over)
}

func TestPasswordQuota_CountsAcrossCommits(t *testing.T) {
	h := newQuotaHarness(t, 3)
	ok, over := countResults(h.commit(quotaNewPasswords("a", 2)...))
	require.Equal(t, 2, ok)
	require.Zero(t, over)

	ok, over = countResults(h.commit(quotaNewPasswords("b", 2)...))
	require.Equal(t, 1, ok)
	require.Equal(t, 1, over)
}

func TestPasswordQuota_AllowsUpdatesAndDeletesAtLimit(t *testing.T) {
	h := newQuotaHarness(t, 2)
	created := h.commit(quotaNewPasswords("pw", 2)...)
	ok, _ := countResults(created)
	require.Equal(t, 2, ok)

	// At the limit: edit one, delete the other.
	edit := quotaPassword(created[0].GetIdString(), created[0].GetVersion(), false)
	edit.Specifics.GetPassword().Encrypted.Blob = proto.String("edited")
	del := quotaPassword(created[1].GetIdString(), created[1].GetVersion(), true)
	ok, over := countResults(h.commit(edit, del))
	require.Equal(t, 2, ok)
	require.Zero(t, over)

	// The tombstone frees a slot: one create fits, the next does not.
	ok, over = countResults(h.commit(quotaNewPasswords("again", 2)...))
	require.Equal(t, 1, ok)
	require.Equal(t, 1, over)
}

func TestPasswordQuota_LimitIsPerSeed(t *testing.T) {
	h := newQuotaHarness(t, 1)
	ok, _ := countResults(h.commit(quotaNewPasswords("pw", 1)...))
	require.Equal(t, 1, ok)

	contents := sync_pb.ClientToServerMessage_COMMIT
	msg := &sync_pb.ClientToServerMessage{
		MessageContents: &contents,
		Commit: &sync_pb.CommitMessage{
			Entries:   quotaNewPasswords("other", 1),
			CacheGuid: proto.String("cache-guid-2"),
		},
	}
	rsp := &sync_pb.ClientToServerResponse{}
	require.NoError(t, command.HandleClientToServerMessage(h.cache, msg, rsp, h.ds, "another-seed"))
	ok, over := countResults(rsp.GetCommit().GetEntryresponse())
	require.Equal(t, 1, ok)
	require.Zero(t, over)
}

func TestPasswordQuota_DisabledAllowsCreates(t *testing.T) {
	h := newQuotaHarness(t, -1)
	ok, over := countResults(h.commit(quotaNewPasswords("pw", 10)...))
	require.Equal(t, 10, ok)
	require.Zero(t, over)
}

func TestPasswordQuota_ItemCountIgnoresTombstonesAndOtherTypes(t *testing.T) {
	h := newQuotaHarness(t, 120)
	created := h.commit(quotaNewPasswords("pw", 3)...)
	del := quotaPassword(created[0].GetIdString(), created[0].GetVersion(), true)
	h.commit(del)

	// A NIGORI row is live but is not a password.
	_, err := h.ds.Db.Exec(`INSERT INTO sync_entities (client_id, id, data_type, deleted) VALUES (?, 'nigori', 47745, 0)`, quotaTestClient)
	require.NoError(t, err)

	counts, err := h.ds.GetClientItemCount(quotaTestClient)
	require.NoError(t, err)
	require.Equal(t, 2+upstreamMaxClientObjectQuota-120, counts.ItemCount)

	h.ds.MaxPasswords = -1
	counts, err = h.ds.GetClientItemCount(quotaTestClient)
	require.NoError(t, err)
	require.Equal(t, 3, counts.ItemCount, "disabled: real live count of all types")
}

func TestLoadMaxPasswords(t *testing.T) {
	t.Setenv("LITESYNC_MAX_PASSWORDS", "")
	require.Equal(t, 120, loadMaxPasswords())
	t.Setenv("LITESYNC_MAX_PASSWORDS", "500")
	require.Equal(t, 500, loadMaxPasswords())
	t.Setenv("LITESYNC_MAX_PASSWORDS", "-1")
	require.Equal(t, -1, loadMaxPasswords())
}

// commitOverHTTP sends a commit through the production middleware order
// (entity policy -> quota messages -> go-sync controller).
func (h *quotaHarness) commitOverHTTP(entries ...*sync_pb.SyncEntity) []*sync_pb.CommitResponse_EntryResponse {
	h.t.Helper()
	contents := sync_pb.ClientToServerMessage_COMMIT
	raw, err := proto.Marshal(&sync_pb.ClientToServerMessage{
		Share:           proto.String(""),
		MessageContents: &contents,
		Commit:          &sync_pb.CommitMessage{Entries: entries, CacheGuid: proto.String("cache-guid")},
	})
	require.NoError(h.t, err)

	handler := newEntityPolicy(loadEntityPolicyConfig()).middleware()(
		passwordQuotaMessages(h.ds)(controller.Command(h.cache, h.ds)))

	req := httptest.NewRequest(http.MethodPost, "/command/", bytes.NewReader(raw))
	req = req.WithContext(context.WithValue(req.Context(), syncContext.ContextKeyClientID, quotaTestClient))
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	require.Equal(h.t, http.StatusOK, w.Code, w.Body.String())

	rsp := &sync_pb.ClientToServerResponse{}
	require.NoError(h.t, proto.Unmarshal(w.Body.Bytes(), rsp))
	return rsp.GetCommit().GetEntryresponse()
}

func TestPasswordQuota_OverQuotaMessageStatesRealCount(t *testing.T) {
	h := newQuotaHarness(t, 2)
	rsps := h.commitOverHTTP(quotaNewPasswords("pw", 3)...)
	require.Len(t, rsps, 3)
	require.Equal(t, sync_pb.CommitResponse_SUCCESS, rsps[0].GetResponseType())
	require.Equal(t, sync_pb.CommitResponse_SUCCESS, rsps[1].GetResponseType())
	require.Equal(t, sync_pb.CommitResponse_OVER_QUOTA, rsps[2].GetResponseType())
	require.Equal(t,
		"Password limit reached: this sync chain has 2 of 2 allowed passwords",
		rsps[2].GetErrorMessage())
}

func TestPasswordQuota_SuccessfulCommitResponseUnchanged(t *testing.T) {
	h := newQuotaHarness(t, 5)
	rsps := h.commitOverHTTP(quotaNewPasswords("pw", 2)...)
	require.Len(t, rsps, 2)
	for _, r := range rsps {
		require.Equal(t, sync_pb.CommitResponse_SUCCESS, r.GetResponseType())
		require.Empty(t, r.GetErrorMessage())
	}
}
