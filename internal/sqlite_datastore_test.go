package internal_test

import (
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/brave/go-sync/datastore"
	"github.com/mikaelhg/litesync/internal"
	"github.com/stretchr/testify/assert"
)

func TestInsertion(t *testing.T) {
	ds, err := internal.NewSqliteDatastore(":memory:")
	assert.NoError(t, err, "can't have an error here")
	err = ds.CreateTable()
	assert.NoError(t, err, "can't have an error here")

	one := int64(1)

	se := datastore.SyncEntity{
		ClientID: "foobar",
		ID:       "xyzzy",
		Version:  &one,
	}

	ok, err := ds.InsertSyncEntity(&se)
	assert.NoError(t, err, "can't have an error here")
	assert.True(t, ok)

	count, err := ds.GetClientItemCount(se.ClientID)
	assert.NoError(t, err, "can't have an error here")
	assert.Equal(t, count, 1, "we inserted a single row")
}

func TestInsertSyncEntity(t *testing.T) {
	ds, err := internal.NewSqliteDatastore(":memory:")
	assert.NoError(t, err, "can't have an error here")
	err = ds.CreateTable()
	assert.NoError(t, err, "can't have an error here")

	entity1 := datastore.SyncEntity{
		ClientID:      "client1",
		ID:            "id1",
		Version:       aws.Int64(1),
		Ctime:         aws.Int64(12345678),
		Mtime:         aws.Int64(12345678),
		DataType:      aws.Int(123),
		Folder:        aws.Bool(false),
		Deleted:       aws.Bool(false),
		DataTypeMtime: aws.String("123#12345678"),
	}
	entity2 := entity1
	entity2.ID = "id2"
	_, err = ds.InsertSyncEntity(&entity1)
	assert.NoError(t, err, "InsertSyncEntity should succeed")
	_, err = ds.InsertSyncEntity(&entity2)
	assert.NoError(t, err, "InsertSyncEntity with other ID should succeed")
	_, err = ds.InsertSyncEntity(&entity1)
	assert.Error(t, err, "InsertSyncEntity with the same ClientID and ID should fail")

	// Insert entity with client tag should result in one sync item and one tag
	// item saved.
	entity3 := entity1
	entity3.ID = "id3"
	entity3.ClientDefinedUniqueTag = aws.String("tag1")
	_, err = ds.InsertSyncEntity(&entity3)
	assert.NoError(t, err, "InsertSyncEntity should succeed")

	// Insert entity with different tag for same ClientID should succeed.
	entity4 := entity3
	entity4.ID = "id4"
	entity4.ClientDefinedUniqueTag = aws.String("tag2")
	_, err = ds.InsertSyncEntity(&entity4)
	assert.NoError(t, err, "InsertSyncEntity with different server tag should succeed")

	// Insert entity with the same client tag and ClientID should fail with conflict.
	entity4Copy := entity4
	entity4Copy.ID = "id4_copy"
	conflict, err := ds.InsertSyncEntity(&entity4Copy)
	assert.Error(t, err, "InsertSyncEntity with the same client tag and ClientID should fail")
	assert.True(t, conflict, "Return conflict for duplicate client tag")
	var orphanExists bool
	err = ds.Db.QueryRow(
		"SELECT EXISTS(SELECT 1 FROM sync_entities WHERE client_id = ? AND id = ?)",
		entity4Copy.ClientID, entity4Copy.ID).Scan(&orphanExists)
	assert.NoError(t, err)
	assert.False(t, orphanExists, "conflicting insert must roll back the entity row")

	// Insert entity with the same client tag for other client should not fail.
	entity5 := entity3
	entity5.ClientID = "client2"
	entity5.ID = "id5"
	_, err = ds.InsertSyncEntity(&entity5)
	assert.NoError(t, err,
		"InsertSyncEntity with the same client tag for another client should succeed")
}

func TestCreateTableMigratesClientTagIndex(t *testing.T) {
	ds, err := internal.NewSqliteDatastore(":memory:")
	assert.NoError(t, err)

	_, err = ds.Db.Exec(`
		DROP INDEX idx_unique_client_tag;
		CREATE UNIQUE INDEX idx_unique_client_tag
		ON sync_entities (client_id, client_defined_unique_tag)
		WHERE client_defined_unique_tag IS NOT NULL`)
	assert.NoError(t, err)
	assert.NoError(t, ds.CreateTable())

	var definition string
	err = ds.Db.QueryRow(
		"SELECT sql FROM sqlite_master WHERE type = 'index' AND name = 'idx_unique_client_tag'",
	).Scan(&definition)
	assert.NoError(t, err)
	assert.Contains(t, definition, "COALESCE(deleted, 0) = 0")
}

func TestInsertSyncEntitiesWithServerTags(t *testing.T) {
	ds, err := internal.NewSqliteDatastore(":memory:")
	assert.NoError(t, err, "can't have an error here")
	err = ds.CreateTable()
	assert.NoError(t, err, "can't have an error here")

	entities := []*datastore.SyncEntity{
		{
			ClientID:               "client1",
			ID:                     "id1",
			Version:                aws.Int64(1),
			Ctime:                  aws.Int64(12345678),
			Mtime:                  aws.Int64(12345678),
			DataType:               aws.Int(123),
			Folder:                 aws.Bool(false),
			Deleted:                aws.Bool(false),
			DataTypeMtime:          aws.String("123#12345678"),
			ServerDefinedUniqueTag: aws.String("tag1"),
		},
		{
			ClientID:               "client1",
			ID:                     "id2",
			Version:                aws.Int64(2),
			Ctime:                  aws.Int64(12345679),
			Mtime:                  aws.Int64(12345679),
			DataType:               aws.Int(124),
			Folder:                 aws.Bool(true),
			Deleted:                aws.Bool(false),
			DataTypeMtime:          aws.String("124#12345679"),
			ServerDefinedUniqueTag: aws.String("tag2"),
		},
	}

	err = ds.InsertSyncEntitiesWithServerTags(entities)
	assert.NoError(t, err, "InsertSyncEntitiesWithServerTags should succeed")

	// Try inserting with duplicate server tags
	duplicateEntities := []*datastore.SyncEntity{
		{
			ClientID:               "client1",
			ID:                     "id3",
			Version:                aws.Int64(3),
			Ctime:                  aws.Int64(12345680),
			Mtime:                  aws.Int64(12345680),
			DataType:               aws.Int(125),
			Folder:                 aws.Bool(false),
			Deleted:                aws.Bool(true),
			DataTypeMtime:          aws.String("125#12345680"),
			ServerDefinedUniqueTag: aws.String("tag1"), // Duplicate tag
		},
	}

	err = ds.InsertSyncEntitiesWithServerTags(duplicateEntities)
	assert.Error(t, err, "InsertSyncEntitiesWithServerTags with duplicate server tags should fail")
}

func TestUpdateSyncEntity(t *testing.T) {
	ds, err := internal.NewSqliteDatastore(":memory:")
	assert.NoError(t, err)
	assert.NoError(t, ds.CreateTable())

	entity := datastore.SyncEntity{
		ClientID:      "client1",
		ID:            "id1",
		Version:       aws.Int64(1),
		Ctime:         aws.Int64(12345678),
		Mtime:         aws.Int64(12345678),
		DataType:      aws.Int(123),
		Folder:        aws.Bool(false),
		Deleted:       aws.Bool(false),
		DataTypeMtime: aws.String("123#12345678"),
	}
	_, err = ds.InsertSyncEntity(&entity)
	assert.NoError(t, err)

	// Update with correct oldVersion
	entity.Version = aws.Int64(2)
	conflict, deleted, err := ds.UpdateSyncEntity(&entity, 1)
	assert.NoError(t, err)
	assert.False(t, conflict)
	assert.False(t, deleted)

	var dataTypeMtime string
	err = ds.Db.QueryRow("SELECT data_type_mtime FROM sync_entities WHERE client_id = ? AND id = ?", entity.ClientID, entity.ID).Scan(&dataTypeMtime)
	assert.NoError(t, err)
	assert.Equal(t, "123#12345678", dataTypeMtime)

	// Update with incorrect oldVersion (conflict)
	entity.Version = aws.Int64(3)
	conflict, deleted, err = ds.UpdateSyncEntity(&entity, 1)
	assert.NoError(t, err)
	assert.True(t, conflict)
	assert.False(t, deleted)

	// Test deleting an entity with a client tag
	entityWithClientTag := datastore.SyncEntity{
		ClientID:               "client2",
		ID:                     "id2",
		Version:                aws.Int64(1),
		Ctime:                  aws.Int64(12345678),
		Mtime:                  aws.Int64(12345678),
		DataType:               aws.Int(123),
		Folder:                 aws.Bool(false),
		Deleted:                aws.Bool(false),
		DataTypeMtime:          aws.String("123#12345678"),
		ClientDefinedUniqueTag: aws.String("tag1"),
	}
	_, err = ds.InsertSyncEntity(&entityWithClientTag)
	assert.NoError(t, err)

	entityWithClientTag.Deleted = aws.Bool(true)
	entityWithClientTag.Version = aws.Int64(2)
	conflict, deleted, err = ds.UpdateSyncEntity(&entityWithClientTag, 1)
	assert.NoError(t, err)
	assert.False(t, conflict)
	assert.True(t, deleted)

	// Recreating the same logical credential uses the same client tag but a
	// fresh server ID. The deleted row remains as a sync tombstone and must not
	// block the replacement.
	recreated := entityWithClientTag
	recreated.ID = "id3"
	recreated.Version = aws.Int64(3)
	recreated.Deleted = aws.Bool(false)
	conflict, err = ds.InsertSyncEntity(&recreated)
	assert.NoError(t, err)
	assert.False(t, conflict)

	var active, tombstones int
	err = ds.Db.QueryRow(`
		SELECT
			SUM(CASE WHEN COALESCE(deleted, 0) = 0 THEN 1 ELSE 0 END),
			SUM(CASE WHEN deleted = 1 THEN 1 ELSE 0 END)
		FROM sync_entities
		WHERE client_id = ? AND client_defined_unique_tag = ?`,
		recreated.ClientID, *recreated.ClientDefinedUniqueTag).Scan(&active, &tombstones)
	assert.NoError(t, err)
	assert.Equal(t, 1, active)
	assert.Equal(t, 1, tombstones)
}
