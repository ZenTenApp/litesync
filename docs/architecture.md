# Architecture Overview

litesync is a self-hosted replacement for the [Brave sync server](https://github.com/brave/go-sync).
It preserves the upstream HTTP API and business logic wholesale, but swaps out the two
cloud-native infrastructure dependencies — AWS DynamoDB and Redis — for local equivalents
that require zero external services.

---

## Goals

| Goal                                         | How it is achieved                                                    |
| -------------------------------------------- | --------------------------------------------------------------------- |
| Drop-in API compatibility with Brave browser | Reuse `github.com/brave/go-sync` controllers and middleware unchanged |
| No cloud dependencies                        | SQLite3 replaces DynamoDB; an in-process LRU cache replaces Redis     |
| Single static binary                         | CGo-linked `go-sqlite3`; no runtime dependencies beyond the OS        |
| Easy self-hosting                            | One binary, one flag (`-db`), one file on disk                        |

---

## High-Level Component Diagram

```
┌─────────────────────────────────────────────────────────────────┐
│  Brave Browser                                                  │
│  brave-browser --sync-url=http://host:8295/litesync             │
└────────────────────────┬────────────────────────────────────────┘
                         │ HTTPS (via reverse proxy) or HTTP
                         ▼
┌─────────────────────────────────────────────────────────────────┐
│  litesync process                                               │
│                                                                 │
│  ┌──────────────────────────────────────────────────────────┐   │
│  │  cmd/litesync/litesync.go  (main)                        │   │
│  │  • Parses -bind / -db flags                              │   │
│  │  • Calls internal.StartServer()                          │   │
│  └──────────────────────┬───────────────────────────────────┘   │
│                         │                                       │
│  ┌──────────────────────▼───────────────────────────────────┐   │
│  │  internal/server.go  (HTTP layer)                        │   │
│  │                                                          │   │
│  │  Chi router  /litesync                                   │   │
│  │    middleware stack:                                     │   │
│  │      RealIP → Heartbeat(/) → hlog → access log          │   │
│  │      → Recoverer → Timeout(60s) → bearerToken           │   │
│  │      → CommonResponseHeaders → Auth → rate limiter      │   │
│  │      → DisabledChain → entity policy → controller       │   │
│  │                                                          │   │
│  │    POST /litesync/command/  ──► go-sync controller.Command│  │
│  └──────────┬──────────────────────────┬────────────────────┘   │
│             │                          │                        │
│  ┌──────────▼──────────┐   ┌───────────▼──────────────────┐    │
│  │  SqliteDatastore    │   │  FakeRedisClient             │    │
│  │  (internal/)        │   │  (internal/)                 │    │
│  │                     │   │                              │    │
│  │  Implements the     │   │  Implements cache.RedisClient│    │
│  │  go-sync Datastore  │   │  interface using a 1 024-    │    │
│  │  interface against  │   │  entry hashicorp/golang-lru  │    │
│  │  a local SQLite3    │   │  LRU cache (in-process,      │    │
│  │  file               │   │  no persistence)             │    │
│  └──────────┬──────────┘   └──────────────────────────────┘    │
│             │                                                   │
│  ┌──────────▼──────────┐                                        │
│  │  litesync.sqlite    │                                        │
│  │  (single file on    │                                        │
│  │   disk)             │                                        │
│  └─────────────────────┘                                        │
└─────────────────────────────────────────────────────────────────┘
```

---

## Package Layout

```
litesync/
├── cmd/
│   └── litesync/
│       ├── litesync.go        # Binary entry point; flag parsing; calls StartServer
│       └── litesync_test.go   # Placeholder integration test
└── internal/
    ├── server.go              # HTTP server lifecycle, router setup, middleware wiring
    ├── sqlite_datastore.go    # go-sync Datastore interface → SQLite3 implementation
    ├── fake_redis_client.go   # go-sync RedisClient interface → in-process LRU
    ├── create_table.sql       # Reference DDL (canonical schema lives in sqlite_datastore.go)
    ├── sqlite_datastore_test.go
    ├── sync_entity_test.go
    └── datastoretest/
        ├── dynamo.go          # Test helpers: ScanSyncEntities, ScanTagItems, ResetTable
        └── mock_datastore.go  # testify/mock implementation of the Datastore interface
```

---

## Request Lifecycle

```
Browser  →  POST /litesync/command/
              │
              ├─ RealIP middleware        (resolve client IP behind proxy)
              ├─ Heartbeat /              (health-check short-circuit, not logged)
              ├─ hlog                     (request-scoped zerolog logger + req_id)
              ├─ access log               (one line per request: status, bytes, duration)
              ├─ Recoverer                (panic → 500, recorded by the access line)
              ├─ Timeout(60s)             (context deadline)
              ├─ bearerToken              (extract Authorization: Bearer <token> → ctx)
              ├─ CommonResponseHeaders    (upstream go-sync headers)
              ├─ Auth                     (upstream go-sync auth middleware)
              ├─ rate limiter             (per-IP + per-client_id token buckets)
              ├─ DisabledChain            (upstream go-sync chain-disabled check)
              ├─ entity policy            (passwords-only + per-entity size cap)
              │
              └─ controller.Command(cache, datastore)
                    │
                    ├─ reads/writes via SqliteDatastore
                    └─ reads/writes via cache.Cache(FakeRedisClient)
```

---

## Dependency Relationships

```
litesync (this repo)
  └── github.com/brave/go-sync          # upstream sync logic (controllers, middleware,
  │                                     #   datastore interface, protobuf schema)
  ├── github.com/brave-intl/bat-go/libs # logging helpers (zerolog setup)
  ├── github.com/go-chi/chi/v5          # HTTP router
  ├── github.com/rs/zerolog             # structured logging
  ├── github.com/mattn/go-sqlite3       # CGo SQLite3 driver
  └── github.com/hashicorp/golang-lru   # LRU cache (FakeRedisClient backing store)
```

---

## Concurrency Model

- The HTTP server runs in a single goroutine via `server.Serve(listener)`.
- A second goroutine listens for `SIGINT`/`SIGTERM` and triggers a graceful shutdown
  with a 30-second timeout.
- `SqliteDatastore` uses `database/sql`'s built-in connection pool; each operation
  opens a transaction and commits/rolls back before returning.
- `FakeRedisClient` protects its LRU cache with a `sync.Mutex` for the `Incr` and
  `FlushAll` operations; `Set`/`Get`/`Del` rely on the thread-safety guarantees of
  `hashicorp/golang-lru`.

---

## Graceful Shutdown Sequence

```
SIGINT / SIGTERM received
  │
  ├─ context.WithTimeout(30s) created
  ├─ server.Shutdown(ctx) called
  │     └─ waits for in-flight requests to complete (up to 30s)
  └─ if timeout exceeded → server.Close() (hard close)
```

---

## Configuration

All configuration is via CLI flags (no config file, no environment variables for
core settings):

| Flag    | Default             | Description                       |
| ------- | ------------------- | --------------------------------- |
| `-bind` | `:8295`             | `host:port` the server listens on |
| `-db`   | `./litesync.sqlite` | Path to the SQLite database file  |
| `-help` | `false`             | Print usage and exit              |

The `ENV` environment variable is forwarded to the bat-go logger to switch between
development and production log formatting, and `LITESYNC_LOG_LEVEL` sets the
level. Both are optional:

| Env var              | Default | Meaning                                                       |
| -------------------- | ------- | ------------------------------------------------------------- |
| `ENV`                | unset   | `local` → human-readable console; anything else → JSON stdout |
| `LITESYNC_LOG_LEVEL` | `info`  | `debug`, `info`, `warn`, `error`, `fatal`, `panic`            |

---

## Logging

`internal/server.go` builds the logger once, in `setupLogger`, via
`brave-intl/bat-go/libs/logging.SetupLogger` (zerolog under the hood). In the
`local` environment it is a `zerolog.ConsoleWriter`; otherwise it is JSON to
stdout through a `diode` ring buffer that drops messages rather than blocking a
request under load. Every line carries `time`.

Two things are worth knowing about how it is wired:

- **The configured logger is adopted as the zerolog global.** The upstream
go-sync controller and middleware (`Auth`, `DisabledChain`) log through
`github.com/rs/zerolog/log`, which ignores the request context. `setupLogger`
assigns the configured logger to that global, so upstream lines share the same
stream, format and level instead of going to stderr as unformatted JSON. The
trade-off is that those lines carry no `req_id` or `user_agent` — upstream code
never reads the request logger.
- **One access line per request.** `hlog.NewHandler` installs a request-scoped
logger, `RequestIDHandler` generates a `req_id` (xid) and echoes it in a
`Request-Id` response header, and `hlog.AccessHandler` emits the line when the
handler returns:

  ```json
  {"level":"warn","user_agent":"...","req_id":"dc23upmdhr36lf0j7cd0","method":"POST","path":"/litesync/command/","client_ip":"198.51.100.7","status":401,"bytes":13,"duration":0.096625,"message":"request"}
  ```

  The level follows the status class: `info` for 2xx/3xx, `warn` for 4xx,
`error` for 5xx. `duration` is in seconds (zerolog's `Dur` encoding). The
`Heartbeat("/")` route short-circuits before the logging middleware, so health
checks do not flood the log; `chiware.Recoverer` sits inside the access logger so
a panic is turned into a 500 that the same access line records.

Denials are logged where they are decided rather than inferred from status
codes: `rate limited by IP` / `rate limited by client_id` (`internal/rate_limit.go`,
`warn`, with the offending key) and `commit rejected by entity policy`
(`internal/entity_policy.go`, `warn`, with `client_id` and the rejection reason
in `error`).
