# HTTP API Reference

litesync exposes the same HTTP API as the upstream
[Brave sync server](https://github.com/brave/go-sync). The Brave browser speaks
this API natively; you do not need to interact with it directly under normal
operation. This document describes the surface area for debugging, integration
testing, and understanding what the server does.

---

## Base URL

```
http://<host>:<port>/litesync
```

Default when running locally:

```
http://localhost:8295/litesync
```

---

## Endpoints

### `GET /`

**Health check.** Handled by the `chi/middleware.Heartbeat` middleware before any
application logic runs.

**Request**

```
GET / HTTP/1.1
Host: localhost:8295
```

**Response**

```
HTTP/1.1 200 OK
Content-Type: text/plain; charset=utf-8

OK
```

This endpoint is always available regardless of the state of the datastore or
cache. Use it to verify the process is alive.

---

### `POST /litesync/command/`

**Sync command.** The only application endpoint. Handles all Brave sync
operations (get updates, commit changes, clear data, etc.) encoded as a
protobuf `ClientToServerMessage`.

**Authentication**

The request must carry a valid Brave sync `Authorization: Bearer <token>` header.
The token is validated by the upstream `go-sync` `Auth` middleware. Requests
without a valid token receive `401 Unauthorized`.

**Request**

```
POST /litesync/command/ HTTP/1.1
Host: localhost:8295
Authorization: Bearer <sync-token>
Content-Type: application/octet-stream

<protobuf-encoded ClientToServerMessage>
```

The body is a binary protobuf message defined in the upstream
[`sync_pb.ClientToServerMessage`](https://github.com/brave/go-sync/blob/main/schema/protobuf/sync_pb/sync.proto)
schema. The Brave browser constructs and parses these messages automatically.

**Response — success**

```
HTTP/1.1 200 OK
Content-Type: application/octet-stream

<protobuf-encoded ClientToServerResponse>
```

**Response — auth failure**

```
HTTP/1.1 401 Unauthorized
```

**Response — sync chain disabled**

```
HTTP/1.1 403 Forbidden
```

**Response — server error**

```
HTTP/1.1 500 Internal Server Error
```

---

## Middleware Stack

Every request to `/litesync/*` passes through the following middleware in order:

| Layer | Middleware                      | Effect                                                           |
| ----- | ------------------------------- | ---------------------------------------------------------------- |
| 1     | `chi/middleware.RealIP`         | Overwrites `r.RemoteAddr` with `X-Forwarded-For` / `X-Real-IP`   |
| 2     | `chi/middleware.Heartbeat("/")` | Short-circuits `GET /` → `200 OK`                                |
| 3     | `hlog.NewHandler`               | Attaches zerolog logger to request context                       |
| 4     | `hlog.UserAgentHandler`         | Logs `User-Agent` as `user_agent` field                          |
| 5     | `hlog.RequestIDHandler`         | Generates/propagates `req_id`; sets `Request-Id` response header |
| 6     | `chi/middleware.Timeout(60s)`   | Cancels request context after 60 seconds                         |
| 7     | `bearerToken` (litesync)        | Extracts `Authorization: Bearer <token>` into context            |
| 8     | `go-sync CommonResponseHeaders` | Sets standard Brave sync response headers                        |
| 9     | `go-sync Auth`                  | Validates the bearer token; returns `401` on failure             |
| 10    | `go-sync DisabledChain`         | Returns `403` if the sync chain has been disabled                |

---

## Response Headers

The `CommonResponseHeaders` middleware (from `go-sync`) adds the following headers
to every response under `/litesync`:

| Header                   | Value     |
| ------------------------ | --------- |
| `X-Content-Type-Options` | `nosniff` |
| `X-Frame-Options`        | `DENY`    |

The `RequestIDHandler` middleware adds:

| Header       | Value                                                     |
| ------------ | --------------------------------------------------------- |
| `Request-Id` | A unique ID for the request (useful for correlating logs) |

---

## Protobuf Schema

The request and response bodies are binary protobuf messages. The schema is
defined in the upstream `go-sync` repository:

- [`sync_pb/sync.proto`](https://github.com/brave/go-sync/blob/main/schema/protobuf/sync_pb/sync.proto) — `ClientToServerMessage`, `ClientToServerResponse`
- [`sync_pb/sync_enums.proto`](https://github.com/brave/go-sync/blob/main/schema/protobuf/sync_pb/sync_enums.proto) — data type IDs
- [`sync_pb/entity_specifics.proto`](https://github.com/brave/go-sync/blob/main/schema/protobuf/sync_pb/entity_specifics.proto) — per-type payload definitions

### Common data type IDs

| ID       | Type                     |
| -------- | ------------------------ |
| `47745`  | Nigori (encryption keys) |
| `37702`  | Bookmarks                |
| `47604`  | Preferences              |
| `41008`  | Passwords                |
| `963985` | History                  |
| `150`    | Autofill                 |
| `154`    | Autofill profile         |

---

## Logging

litesync uses structured JSON logging via [zerolog](https://github.com/rs/zerolog).
Each request produces a log line with at minimum:

```json
{
  "level": "info",
  "req_id": "c7f3a1b2",
  "user_agent": "Mozilla/5.0 ...",
  "time": "2024-01-15T10:30:00Z",
  "message": "..."
}
```

Set `ENV=production` to enable JSON output. Without it the logger may use a
human-readable console format.

To tail logs when running under systemd:

```bash
sudo journalctl -u litesync -f
```

---

## Example: Verifying the Server is Reachable

```bash
# Health check
curl -v http://localhost:8295/

# Confirm the sync endpoint exists (will return 401 without a valid token)
curl -v -X POST http://localhost:8295/litesync/command/ \
  -H "Content-Type: application/octet-stream" \
  --data-binary ""
```

Expected responses:

```
GET /          → 200 OK  (body: "OK")
POST /litesync/command/  → 401 Unauthorized  (no valid token)
```
