package internal

import (
	"bytes"
	"compress/gzip"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/brave/go-sync/schema/protobuf/sync_pb"
	"google.golang.org/protobuf/proto"
)

func passwordEntity(blobLen int) *sync_pb.SyncEntity {
	return &sync_pb.SyncEntity{
		IdString: proto.String("pw-1"),
		Version:  proto.Int64(0),
		Specifics: &sync_pb.EntitySpecifics{
			SpecificsVariant: &sync_pb.EntitySpecifics_Password{
				Password: &sync_pb.PasswordSpecifics{
					Encrypted: &sync_pb.EncryptedData{
						Blob: proto.String(strings.Repeat("x", blobLen)),
					},
				},
			},
		},
	}
}

func bookmarkEntity() *sync_pb.SyncEntity {
	return &sync_pb.SyncEntity{
		IdString: proto.String("bm-1"),
		Version:  proto.Int64(0),
		Specifics: &sync_pb.EntitySpecifics{
			SpecificsVariant: &sync_pb.EntitySpecifics_Bookmark{
				Bookmark: &sync_pb.BookmarkSpecifics{},
			},
		},
	}
}

func nigoriEntity() *sync_pb.SyncEntity {
	return &sync_pb.SyncEntity{
		IdString: proto.String("nig-1"),
		Version:  proto.Int64(0),
		Specifics: &sync_pb.EntitySpecifics{
			SpecificsVariant: &sync_pb.EntitySpecifics_Nigori{
				Nigori: &sync_pb.NigoriSpecifics{},
			},
		},
	}
}

func noSpecificsEntity() *sync_pb.SyncEntity {
	return &sync_pb.SyncEntity{IdString: proto.String("none-1"), Version: proto.Int64(0)}
}

func commitMessage(entries ...*sync_pb.SyncEntity) *sync_pb.ClientToServerMessage {
	return &sync_pb.ClientToServerMessage{
		Share:           proto.String(""),
		MessageContents: sync_pb.ClientToServerMessage_COMMIT.Enum(),
		Commit: &sync_pb.CommitMessage{
			CacheGuid: proto.String("test-guid"),
			Entries:   entries,
		},
	}
}

// getUpdatesMessage builds a required-field-complete GetUpdates message.
func getUpdatesMessage() *sync_pb.ClientToServerMessage {
	return &sync_pb.ClientToServerMessage{
		Share:           proto.String(""),
		MessageContents: sync_pb.ClientToServerMessage_GET_UPDATES.Enum(),
		GetUpdates:      &sync_pb.GetUpdatesMessage{},
	}
}

func TestEntityPolicy_AllowsPassword(t *testing.T) {
	p := newEntityPolicy(entityPolicyConfig{maxPasswordSize: 1024})
	if err := p.validateEntries([]*sync_pb.SyncEntity{passwordEntity(64)}); err != nil {
		t.Fatalf("small password should be allowed, got: %v", err)
	}
}

func TestEntityPolicy_AllowsNigori(t *testing.T) {
	p := newEntityPolicy(entityPolicyConfig{maxPasswordSize: 1024})
	if err := p.validateEntries([]*sync_pb.SyncEntity{nigoriEntity()}); err != nil {
		t.Fatalf("nigori must be allowed (needed for chain init), got: %v", err)
	}
}

func TestEntityPolicy_RejectsBookmark(t *testing.T) {
	p := newEntityPolicy(entityPolicyConfig{maxPasswordSize: 1024})
	err := p.validateEntries([]*sync_pb.SyncEntity{bookmarkEntity()})
	if err == nil {
		t.Fatal("bookmark should be rejected")
	}
	if !strings.Contains(err.Error(), "unsupported") {
		t.Fatalf("expected unsupported-type error, got: %v", err)
	}
}

func TestEntityPolicy_RejectsNoSpecifics(t *testing.T) {
	p := newEntityPolicy(entityPolicyConfig{maxPasswordSize: 1024})
	if err := p.validateEntries([]*sync_pb.SyncEntity{noSpecificsEntity()}); err == nil {
		t.Fatal("entity without specifics should be rejected")
	}
}

func TestEntityPolicy_RejectsOversizePassword(t *testing.T) {
	p := newEntityPolicy(entityPolicyConfig{maxPasswordSize: 1024})
	err := p.validateEntries([]*sync_pb.SyncEntity{passwordEntity(5000)})
	if err == nil {
		t.Fatal("oversize password should be rejected")
	}
	if !strings.Contains(err.Error(), "too large") {
		t.Fatalf("expected size error, got: %v", err)
	}
}

func TestEntityPolicy_MixedEntriesRejectsIfAnyBad(t *testing.T) {
	p := newEntityPolicy(entityPolicyConfig{maxPasswordSize: 1024})
	err := p.validateEntries([]*sync_pb.SyncEntity{passwordEntity(32), bookmarkEntity()})
	if err == nil {
		t.Fatal("mixed commit with a bookmark must be rejected")
	}
}

func TestEntityPolicy_SizeCapDisabled(t *testing.T) {
	// negative maxPasswordSize disables the cap.
	p := newEntityPolicy(entityPolicyConfig{maxPasswordSize: -1})
	if err := p.validateEntries([]*sync_pb.SyncEntity{passwordEntity(5000)}); err != nil {
		t.Fatalf("oversize password should be allowed when cap disabled, got: %v", err)
	}
}

// --- middleware integration ------------------------------------------------

func gzipWriter(w *bytes.Buffer) *gzip.Writer {
	return gzip.NewWriter(w)
}

func doMiddleware(reqBody *sync_pb.ClientToServerMessage, gzipped bool) (int, bool) {
	raw, err := proto.Marshal(reqBody)
	if err != nil {
		panic("test marshal: " + err.Error())
	}
	if gzipped {
		var buf bytes.Buffer
		gz := gzipWriter(&buf)
		_, _ = gz.Write(raw)
		_ = gz.Close()
		raw = buf.Bytes()
	}

	p := newEntityPolicy(entityPolicyConfig{maxPasswordSize: 1024})
	reached := false
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { reached = true; w.WriteHeader(http.StatusOK) })
	h := p.middleware()(next)

	r := httptest.NewRequest(http.MethodPost, "/command/", bytes.NewReader(raw))
	if gzipped {
		r.Header.Set("Content-Encoding", "gzip")
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w.Code, reached
}

func TestMiddleware_PassesValidCommitAndReServesBody(t *testing.T) {
	code, reached := doMiddleware(commitMessage(passwordEntity(32)), false)
	if code != http.StatusOK || !reached {
		t.Fatalf("valid commit should reach next: code=%d reached=%v", code, reached)
	}
}

func TestMiddleware_PassesGetUpdates(t *testing.T) {
	// A GetUpdates message (no commit) must pass through untouched.
	code, reached := doMiddleware(getUpdatesMessage(), false)
	if code != http.StatusOK || !reached {
		t.Fatalf("get-updates should pass: code=%d reached=%v", code, reached)
	}
}

func TestMiddleware_RejectsBookmarkCommit(t *testing.T) {
	code, reached := doMiddleware(commitMessage(bookmarkEntity()), false)
	if code != http.StatusBadRequest || reached {
		t.Fatalf("bookmark commit should be rejected with 400: code=%d reached=%v", code, reached)
	}
}

func TestMiddleware_WorksWithGzippedBody(t *testing.T) {
	code, reached := doMiddleware(commitMessage(passwordEntity(32)), true)
	if code != http.StatusOK || !reached {
		t.Fatalf("gzipped valid commit should pass: code=%d reached=%v", code, reached)
	}
}
