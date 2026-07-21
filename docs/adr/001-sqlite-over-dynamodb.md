# ADR 001 — Replace DynamoDB + Redis with SQLite3 + In-Process LRU Cache

| Field        | Value              |
| ------------ | ------------------ |
| **Status**   | Accepted           |
| **Date**     | 2024               |
| **Deciders** | Project maintainer |

---

## Context

The upstream [Brave sync server (`go-sync`)](https://github.com/brave/go-sync) is
designed to run as a managed cloud service. Its two infrastructure dependencies
reflect that origin:

- **AWS DynamoDB** — a fully managed, serverless NoSQL database. Requires an AWS
  account, IAM credentials, a provisioned or on-demand table, and ongoing cost
  proportional to read/write capacity.
- **Redis** — an in-memory data structure store used for short-lived session tokens
  and rate-limiting counters. Requires a running Redis process (or a managed
  service such as ElastiCache) reachable from the sync server.

For a **self-hosted, single-user or small-group** deployment these dependencies are
disproportionately heavy:

- A personal sync server handles at most a handful of Brave browser clients.
  DynamoDB's pricing model and operational overhead are designed for millions of
  requests per second.
- Running a Redis process (or paying for ElastiCache) solely to cache a few dozen
  tokens is wasteful.
- Both services require network access, credentials management, and ongoing
  maintenance — all of which raise the barrier to self-hosting.

---

## Decision

Replace the two external infrastructure dependencies with local equivalents that
require zero external services:

| Upstream dependency | litesync replacement                              | Location                        |
| ------------------- | ------------------------------------------------- | ------------------------------- |
| AWS DynamoDB        | SQLite3 (via `mattn/go-sqlite3`)                  | `internal/sqlite_datastore.go`  |
| Redis               | In-process LRU cache (via `hashicorp/golang-lru`) | `internal/fake_redis_client.go` |

Both replacements implement the same Go interfaces defined by `go-sync`, so the
upstream controllers and middleware are consumed **unchanged as a library**.

### SQLite3 as the datastore

`SqliteDatastore` implements `github.com/brave/go-sync/datastore.Datastore`.

Key design choices:

- **Single file on disk.** The entire sync state is one `.sqlite` file. Backup =
  copy one file. Migration = copy one file. Uninstall = delete one file.
- **Schema mirrors DynamoDB's access patterns.** The `sync_entities` table uses a
  composite primary key `(client_id, id)` that matches DynamoDB's partition key +
  sort key model. Tag sentinel rows (prefixed `Server#` / `Client#`) are stored in
  the same table, matching the upstream DynamoDB layout.
- **Optimistic concurrency via version check.** `UpdateSyncEntity` uses a
  `WHERE version = <oldVersion>` clause; zero rows affected signals a conflict,
  exactly as the DynamoDB conditional expression did.
- **Transactions for atomicity.** Multi-step operations (e.g. inserting an entity
  plus its tag sentinel) are wrapped in a single SQLite transaction so partial
  writes cannot occur.
- **No schema migrations needed (yet).** The table is created with
  `CREATE TABLE IF NOT EXISTS` on startup. For a personal server with a small
  dataset, dropping and recreating the database is an acceptable migration strategy
  for now.

### In-process LRU as the cache

`FakeRedisClient` implements `github.com/brave/go-sync/cache.RedisClient`.

Key design choices:

- **No persistence required.** The upstream cache stores short-lived tokens and
  counters. Losing them on restart is harmless — clients simply re-authenticate.
- **Bounded memory.** The LRU is capped at 1 024 entries. For a personal server
  this is more than sufficient and prevents unbounded memory growth.
- **TTL is accepted but not enforced.** The `Set` method accepts a `ttl` parameter
  (to satisfy the interface) but does not schedule expiry. Entries are evicted by
  LRU pressure instead. This is acceptable because the upstream code uses short
  TTLs (seconds to minutes) and the cache is small enough that LRU eviction
  naturally expires old entries.
- **Thread safety.** `Incr` and `FlushAll` are protected by a `sync.Mutex`.
  `Set`/`Get`/`Del` rely on `hashicorp/golang-lru`'s built-in concurrency safety.

---

## Consequences

### Positive

- **Zero external dependencies.** A single binary + a single file is all that is
  needed to run a fully functional Brave sync server.
- **Trivial deployment.** Download binary, run it, point Brave at it. No Docker,
  no Kubernetes, no cloud account.
- **Trivial backup.** `cp litesync.sqlite litesync.sqlite.bak` (or use SQLite's
  online backup API for a hot copy).
- **Upstream logic unchanged.** Because both replacements implement the same
  interfaces, all sync business logic, authentication, and protocol handling come
  directly from the well-tested `go-sync` library.
- **Fast tests.** SQLite's `:memory:` mode gives each test a fresh, isolated
  database with no I/O overhead.

### Negative / Trade-offs

- **Not horizontally scalable.** SQLite does not support concurrent writers from
  multiple processes. litesync is intentionally single-process; this is not a
  concern for its target use case.
- **No TTL enforcement in cache.** Tokens that should expire after N seconds will
  linger until evicted by LRU pressure. In practice this is harmless for a
  personal server but would be a correctness issue at scale.
- **CGo dependency.** `mattn/go-sqlite3` requires a C compiler at build time and
  produces a CGo binary. Pure-Go SQLite drivers exist (e.g. `modernc.org/sqlite`)
  but were not evaluated at the time of this decision.
- **No DynamoDB compatibility.** If you want to migrate back to the upstream
  cloud deployment, you would need to export the SQLite data and import it into
  DynamoDB. No migration tooling exists for this.
- **Schema evolution is manual.** There is no migration framework. Schema changes
  require either a manual `ALTER TABLE` or a database rebuild.

---

## Alternatives Considered

### Keep DynamoDB + Redis, document local setup with LocalStack

Running [LocalStack](https://localstack.cloud/) emulates AWS services locally.
This would preserve full upstream compatibility but adds Docker as a hard
dependency and significant operational complexity for what is meant to be a
"download and run" binary.

**Rejected** — too heavy for the target use case.

### Use PostgreSQL or MySQL

A traditional RDBMS would support concurrent writers and TTL-based expiry.
However, it requires a running database server, credentials, and network
configuration — the same class of problem as DynamoDB/Redis.

**Rejected** — does not meet the "zero external dependencies" goal.

### Use a pure-Go SQLite driver (`modernc.org/sqlite`)

Would eliminate the CGo build requirement and simplify cross-compilation.

**Not evaluated at decision time** — remains a viable future improvement.
See the project issue tracker for status.

### Use `bbolt` or `badger` (embedded key-value stores)

These are pure-Go embedded databases with no CGo requirement. However, they are
key-value stores, not relational databases. Mapping the `go-sync` datastore
interface (which has relational query patterns like "get all entities of type X
with mtime > Y") onto a key-value store would require significant custom indexing
logic.

**Rejected** — the implementation complexity outweighs the CGo-free benefit.

---

## References

- [`internal/sqlite_datastore.go`](../../internal/sqlite_datastore.go) — implementation
- [`internal/fake_redis_client.go`](../../internal/fake_redis_client.go) — implementation
- [`docs/data-model.md`](../data-model.md) — full schema documentation
- [`github.com/brave/go-sync`](https://github.com/brave/go-sync) — upstream server
- [`github.com/mattn/go-sqlite3`](https://github.com/mattn/go-sqlite3) — SQLite3 driver
- [`github.com/hashicorp/golang-lru`](https://github.com/hashicorp/golang-lru) — LRU cache
