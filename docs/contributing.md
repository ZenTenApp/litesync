# Contributing Guide

This document covers everything you need to get litesync building, tested, and
linted on your local machine.

---

## Table of Contents

- [Contributing Guide](#contributing-guide)
  - [Table of Contents](#table-of-contents)
  - [Prerequisites](#prerequisites)
    - [Install golangci-lint](#install-golangci-lint)
  - [Clone and Build](#clone-and-build)
  - [Running the Server Locally](#running-the-server-locally)
  - [Running Tests](#running-tests)
    - [Test packages](#test-packages)
    - [Test helpers (`internal/datastoretest`)](#test-helpers-internaldatastoretest)
  - [Linting](#linting)
  - [Pre-commit Check](#pre-commit-check)
  - [Project Layout](#project-layout)
  - [Making Changes](#making-changes)
    - [Changing the HTTP layer](#changing-the-http-layer)
    - [Changing the datastore](#changing-the-datastore)
    - [Changing the cache](#changing-the-cache)
  - [Adding a New Datastore Method](#adding-a-new-datastore-method)
  - [Dependency Management](#dependency-management)
  - [Release Process](#release-process)

---

## Prerequisites

| Tool            | Minimum version | Notes                                                     |
| --------------- | --------------- | --------------------------------------------------------- |
| Go              | 1.21            | `go.mod` declares `go 1.25.1`; any recent toolchain works |
| GCC / Clang     | any recent      | Required by `mattn/go-sqlite3` (CGo)                      |
| `golangci-lint` | v1.57+          | Only needed for linting; see install instructions below   |
| `make`          | any             | Convenience wrapper around `go` commands                  |

### Install golangci-lint

```bash
# macOS
brew install golangci-lint

# Linux (binary install — always check https://golangci-lint.run/usage/install/ for latest)
curl -sSfL https://raw.githubusercontent.com/golangci-lint/golangci-lint/master/install.sh \
  | sh -s -- -b $(go env GOPATH)/bin v1.57.2
```

---

## Clone and Build

```bash
git clone https://github.com/mikaelhg/litesync.git
cd litesync

# Download all Go module dependencies
go mod download

# Compile the binary to out/bin/litesync
make build
```

The resulting binary is statically linked except for the CGo SQLite3 driver.
On Linux you can produce a fully static binary with:

```bash
CGO_ENABLED=1 \
  CGO_LDFLAGS="-static" \
  go build -o out/bin/litesync ./cmd/litesync
```

---

## Running the Server Locally

```bash
# Default: binds :8295, database at ./litesync.sqlite
./out/bin/litesync

# Custom bind address and database path
./out/bin/litesync -bind 127.0.0.1:9000 -db /tmp/my-sync.sqlite

# Print all flags
./out/bin/litesync -help
```

Point Brave at the local server:

```bash
brave-browser --sync-url=http://localhost:8295/litesync
```

The health-check endpoint (used by the `Heartbeat` middleware) is always available
at the root path:

```bash
curl http://localhost:8295/
# OK
```

---

## Running Tests

```bash
# Run all tests with the race detector
make test

# Equivalent go command
go test -v -race ./...
```

Tests use an **in-memory SQLite database** (`:memory:`) so they are fast, isolated,
and leave no files on disk.

### Test packages

| Package                                 | What it tests                                                              |
| --------------------------------------- | -------------------------------------------------------------------------- |
| `internal` (`sqlite_datastore_test.go`) | Direct CRUD operations on `SqliteDatastore`                                |
| `internal` (`sync_entity_test.go`)      | Full entity lifecycle via the `SyncEntityTestSuite` (uses `testify/suite`) |
| `cmd/litesync` (`litesync_test.go`)     | Placeholder — integration test stub                                        |

### Test helpers (`internal/datastoretest`)

| Helper                 | Purpose                                                                       |
| ---------------------- | ----------------------------------------------------------------------------- |
| `ScanSyncEntities(db)` | Returns all regular sync rows from the DB (for assertions)                    |
| `ScanTagItems(db)`     | Returns all tag sentinel rows (version IS NULL)                               |
| `ResetTable(db)`       | No-op stub (was DynamoDB table teardown; SQLite tests use `:memory:` instead) |
| `MockDatastore`        | `testify/mock` implementation of the `go-sync` `Datastore` interface          |

---

## Linting

```bash
make lint
# equivalent: golangci-lint run
```

To see lint issues without failing the build (useful during development):

```bash
golangci-lint run -n
```

---

## Pre-commit Check

Run both tests and lint in one step before pushing:

```bash
make pre-commit
```

This runs `make test` followed by `golangci-lint run -n`.

---

## Project Layout

```
litesync/
├── cmd/
│   └── litesync/
│       ├── litesync.go        # main() — flag parsing, calls internal.StartServer
│       └── litesync_test.go   # integration test stub
├── internal/
│   ├── server.go              # HTTP server, router, middleware wiring
│   ├── sqlite_datastore.go    # Datastore interface → SQLite3
│   ├── fake_redis_client.go   # RedisClient interface → in-process LRU
│   ├── create_table.sql       # Reference DDL (informational; schema is in Go code)
│   ├── sqlite_datastore_test.go
│   ├── sync_entity_test.go
│   └── datastoretest/
│       ├── dynamo.go          # Scan helpers for tests
│       └── mock_datastore.go  # Mock Datastore for unit tests
├── docs/                      # ← you are here
│   ├── architecture.md
│   ├── api.md
│   ├── contributing.md
│   ├── data-model.md
│   └── adr/
│       └── 001-sqlite-over-dynamodb.md
├── .github/
│   └── workflows/
│       └── release.yml        # CI: build + publish release binaries
├── go.mod
├── go.sum
├── Makefile
├── README.md
└── DEPLOY.md
```

---

## Making Changes

### Changing the HTTP layer

All routing and middleware lives in [`internal/server.go`](../internal/server.go).
The upstream `go-sync` controllers and middleware are consumed as a library — do not
copy or modify them. If you need to change routing behaviour, wrap or replace
middleware in `setupRouter`.

### Changing the datastore

[`internal/sqlite_datastore.go`](../internal/sqlite_datastore.go) implements the
`github.com/brave/go-sync/datastore.Datastore` interface. Every method on that
interface must be present; the compiler will tell you if one is missing.

When adding a new SQL query:

1. Write the query as a `const` string near the function that uses it.
2. Wrap the execution in a transaction via `ExecInTransaction` (for writes) or use
   `d.Db.QueryContext` directly (for reads).
3. Add a test in `internal/sqlite_datastore_test.go` using an in-memory DB.

### Changing the cache

[`internal/fake_redis_client.go`](../internal/fake_redis_client.go) implements
`github.com/brave/go-sync/cache.RedisClient`. The LRU size is controlled by the
`cacheSize` constant (currently `1024`). TTL values passed to `Set` are silently
ignored — the cache evicts by LRU only.

---

## Adding a New Datastore Method

If the upstream `go-sync` library adds a new method to the `Datastore` interface:

1. Add the method signature to `SqliteDatastore` in
   [`internal/sqlite_datastore.go`](../internal/sqlite_datastore.go).
2. Add the same method to `MockDatastore` in
   [`internal/datastoretest/mock_datastore.go`](../internal/datastoretest/mock_datastore.go)
   following the existing `testify/mock` pattern.
3. Run `make test` to confirm everything compiles and passes.

---

## Dependency Management

```bash
# Add a new dependency
go get github.com/some/package@v1.2.3
go mod tidy

# Update all dependencies to their latest minor/patch versions
go get -u ./...
go mod tidy

# Verify the module graph is consistent
go mod verify
```

> **Note:** `mattn/go-sqlite3` requires CGo. If you see build errors about a missing
> C compiler, install `gcc` (Linux) or Xcode Command Line Tools (macOS).

---

## Release Process

Releases are fully automated via GitHub Actions. See the
[Publishing a Release](../README.md#publishing-a-release) section in the README for
the step-by-step tagging instructions.

The workflow (`.github/workflows/release.yml`) produces:

- `litesync-linux-amd64` — statically linked x86-64 binary
- `litesync-linux-arm64` — statically linked ARM64 binary
- `checksums.txt` — SHA-256 checksums for both binaries
