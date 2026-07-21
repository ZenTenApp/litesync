# Data Model

litesync stores all state in a single SQLite3 file. There is exactly one table:
`sync_entities`. It serves triple duty — holding regular sync items, client-tag
sentinel rows, and server-tag sentinel rows — mirroring the DynamoDB design of the
upstream `go-sync` server.

---

## Table: `sync_entities`

```sql
CREATE TABLE IF NOT EXISTS sync_entities (
    client_id                  TEXT     NOT NULL,
    id                         TEXT     NOT NULL,
    parent_id                  TEXT,
    version                    INTEGER,
    mtime                      INTEGER,
    ctime                      INTEGER,
    name                       TEXT,
    non_unique_name            TEXT,
    server_defined_unique_tag  TEXT,
    deleted                    BOOLEAN,
    originator_cache_guid      TEXT,
    originator_client_item_id  TEXT,
    specifics                  BLOB,
    data_type                  INTEGER,
    folder                     BOOLEAN,
    client_defined_unique_tag  TEXT,
    unique_position            BLOB,
    data_type_mtime            TEXT,
    expiration_time            INTEGER,
    PRIMARY KEY (client_id, id)
);

CREATE UNIQUE INDEX IF NOT EXISTS idx_unique_client_tag
    ON sync_entities (client_id, client_defined_unique_tag)
    WHERE client_defined_unique_tag IS NOT NULL;
```

---

## Column Reference

| Column                      | Type    | Nullable | Description                                                                                                                   |
| --------------------------- | ------- | -------- | ----------------------------------------------------------------------------------------------------------------------------- |
| `client_id`                 | TEXT    | NO       | Opaque identifier for the Brave browser client (sync chain member). Part of the composite PK.                                 |
| `id`                        | TEXT    | NO       | Server-assigned entity ID. For tag sentinel rows the value is prefixed with `Server#` or `Client#`. Part of the composite PK. |
| `parent_id`                 | TEXT    | YES      | ID of the parent entity (used for hierarchical data types such as bookmarks).                                                 |
| `version`                   | INTEGER | YES      | Monotonically increasing version counter. `NULL` on tag sentinel rows.                                                        |
| `mtime`                     | INTEGER | YES      | Last-modified time in Unix milliseconds (set/updated by the server).                                                          |
| `ctime`                     | INTEGER | YES      | Creation time in Unix milliseconds (set once by the server on first insert).                                                  |
| `name`                      | TEXT    | YES      | Human-readable name (e.g. bookmark title).                                                                                    |
| `non_unique_name`           | TEXT    | YES      | Non-unique display name (mirrors the protobuf field).                                                                         |
| `server_defined_unique_tag` | TEXT    | YES      | Tag assigned by the server to identify permanent/singleton items.                                                             |
| `deleted`                   | BOOLEAN | YES      | Soft-delete flag. Deleted entities are retained until the client acknowledges.                                                |
| `originator_cache_guid`     | TEXT    | YES      | Cache GUID of the client that originally created this entity.                                                                 |
| `originator_client_item_id` | TEXT    | YES      | Client-side item ID at the time of creation.                                                                                  |
| `specifics`                 | BLOB    | YES      | Protobuf-encoded `EntitySpecifics` payload (the actual sync data).                                                            |
| `data_type`                 | INTEGER | YES      | Numeric data type ID (e.g. `47745` = Nigori, `963985` = History).                                                             |
| `folder`                    | BOOLEAN | YES      | Whether this entity is a container/folder.                                                                                    |
| `client_defined_unique_tag` | TEXT    | YES      | Client-supplied tag that must be unique per `client_id`. Enforced by `idx_unique_client_tag`.                                 |
| `unique_position`           | BLOB    | YES      | Protobuf-encoded `UniquePosition` for ordering siblings.                                                                      |
| `data_type_mtime`           | TEXT    | YES      | Composite string `"<data_type>#<mtime>"` used for efficient per-type change queries.                                          |
| `expiration_time`           | INTEGER | YES      | Unix timestamp after which the entity may be purged (used for History items). `NULL` means no expiry.                         |

---

## Row Kinds

The single table stores three logically distinct kinds of rows, distinguished by
their `id` prefix and the presence/absence of certain columns:

### 1. Regular Sync Entity

A normal sync item (bookmark, password, preference, etc.).

```
client_id = "abc123"
id        = "some-server-uuid"
version   = 5
data_type = 47745
specifics = <protobuf bytes>
...
```

### 2. Client-Tag Sentinel

Inserted alongside a regular entity that carries a `client_defined_unique_tag`.
Enforces the uniqueness constraint for that tag within a client.

```
client_id = "abc123"
id        = "Client#<client_defined_unique_tag>"
version   = NULL
mtime     = <unix ms>
ctime     = <unix ms>
-- all other columns NULL
```

### 3. Server-Tag Sentinel

Inserted by `InsertSyncEntitiesWithServerTags` to record that a server-defined
permanent item has been initialised for a client.

```
client_id = "abc123"
id        = "Server#<server_defined_unique_tag>"
version   = NULL (or 0)
mtime     = <unix ms>
-- all other columns NULL
```

---

## Indexes

| Index                   | Columns                                  | Condition                                     | Purpose                                                             |
| ----------------------- | ---------------------------------------- | --------------------------------------------- | ------------------------------------------------------------------- |
| `PRIMARY KEY`           | `(client_id, id)`                        | —                                             | Unique row identity; fast lookup by client + entity ID              |
| `idx_unique_client_tag` | `(client_id, client_defined_unique_tag)` | `WHERE client_defined_unique_tag IS NOT NULL` | Prevents two entities for the same client from sharing a client tag |

---

## Key Queries

### Insert a new entity

```sql
INSERT INTO sync_entities (
    client_id, id, parent_id, version, mtime, ctime, name, non_unique_name,
    server_defined_unique_tag, deleted, originator_cache_guid,
    originator_client_item_id, specifics, data_type, folder,
    client_defined_unique_tag, unique_position, data_type_mtime, expiration_time
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?);
```

### Optimistic-concurrency update (version check)

```sql
UPDATE sync_entities
SET version = ?, mtime = ?, specifics = ?, data_type_mtime = ?,
    unique_position = ?, parent_id = ?, name = ?, non_unique_name = ?,
    deleted = ?, folder = ?
WHERE client_id = ? AND id = ? AND version = ?;
-- rowsAffected == 0  →  conflict (version mismatch)
```

### Fetch incremental updates for a data type

```sql
SELECT ...
FROM sync_entities
WHERE client_id = ?
  AND data_type = ?
  AND mtime > ?          -- client token (last known mtime)
  [AND (folder IS NULL OR folder = 0)]
ORDER BY mtime ASC
LIMIT ?;                 -- maxSize + 1 to detect hasChangesRemaining
```

### Check for a server-tag sentinel

```sql
SELECT EXISTS(
    SELECT 1 FROM sync_entities
    WHERE client_id = ? AND id = ?   -- id = 'Server#<tag>'
);
```

### Count items for a client

```sql
SELECT COUNT(*) FROM sync_entities WHERE client_id = ?;
```

---

## In-Memory Cache

The `FakeRedisClient` provides a volatile, in-process LRU cache (capacity: 1 024
entries) that backs the `go-sync` `cache.Cache` layer. It is used by the upstream
controller for short-lived session tokens and rate-limiting counters.

**Important:** the cache is not persisted. It is lost on every process restart.
This is intentional — the upstream cache is designed to be ephemeral.

| Operation                  | Behaviour                                                                         |
| -------------------------- | --------------------------------------------------------------------------------- |
| `Set(key, val, ttl)`       | Adds/replaces entry; TTL is accepted but not enforced (LRU eviction only)         |
| `Get(key, deleteAfterGet)` | Returns value; optionally removes it (one-shot token pattern)                     |
| `Del(keys...)`             | Removes one or more keys                                                          |
| `FlushAll()`               | Replaces the entire LRU with a fresh empty one                                    |
| `Incr(key, subtract)`      | Atomically increments or decrements an integer counter stored as a decimal string |
