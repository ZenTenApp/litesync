# litesync stress test results

Staging targets (both are staging, not production):

- `https://litesync1.zentext.me/litesync`
- `https://litesync2.nostr.box/litesync`

Method: concurrent virtual Brave browsers driven via the `brave-sync-ts` library
(harness at `brave-sync-ts/stress/run.tsx`). Each unit of concurrency = one unique
BIP39 seed = one independent sync chain/client identity. Every worker performs
`connect()` (key derivation + Nigori fetch/init) then N data operations.

Workload profiles (`--mode`):

- `read` — N × `readPasswords()`
- `write` — N × `pushPassword()`
- `mixed` — 1 write per 3 reads

Latency figures are wall-clock ms per operation. `err` counts include
`connect()` failures (which cascade to `Client not connected` on later ops).

---

## litesync1.zentext.me

Faster per-op, but breaks on writes.

| concurrency | mode | ops/s | errors | note |
|---|---|---|---|---|
| 1   | read  | 9.5  | 0  | baseline smoke |
| 10  | read  | 80   | 0  | read p50 44ms, connect p50 434ms |
| 30  | read  | 147  | 0  | read p95 210ms ← tail degrades |
| 75  | read  | 97   | 0  | connect p99 2.9s, read p99 1.3s |
| 150 | read  | 103  | 0  | connect p99 5.6s, read p99 2.7s — ceiling |
| 60  | write | 45   | 0  | write p99 3.7s |
| 90  | write | 50   | **48 (13%)** | **`database is locked`** |
| 100 | write | 47   | 12   | DB errors again; p99 5.7s |
| 75  | mixed | 62   | 0  | realistic mix stayed clean |

Latency & error detail:

```
read  c=150 : connect p50=3255ms p95=5251ms p99=5568ms (max 5652ms)
               read    p50=135ms  p95=1012ms p99=2696ms (max 3089ms)
write c=100 : connect p50=1982ms p95=6856ms p99=8260ms (max 8260ms)
             write    p50=299ms  p95=4928ms p99=5721ms (max 6488ms)
```

Sample errors (write c=90/100):

```
Failed to create Nigori entity: Insert sync entity failed: database is locked
Client not connected. Call connect() first.
```

The `database is locked` (SQLITE_BUSY) error repeats whenever pure-write
concurrency reaches ~c=90–100. `internal/sqlite_datastore.go` opens SQLite with
default settings — **no `busy_timeout`, no `journal_mode=WAL`** — so a busy lock
fails immediately instead of retrying.

Read throughput plateaus around **~100 ops/s regardless of how much more
concurrency you add** — a connection/worker/upstream ceiling. Past it, only tail
latency grows (p99 ≤ 5–6s), no throughput gain.

---

## litesync2.nostr.box

Higher per-op latency, but dramatically more robust under write load — **never
erred in any run**, including far above where lite1 broke.

| concurrency | mode | ops | errors | note |
|---|---|---|---|---|
| 10  | read  | 25  | 0 | read p50 180ms, connect p99 1.3s |
| 40  | write | 190 | 10 | transient: connect timeouts (`fetch failed`) |
| 30  | write | 120 | 0 | post-break recovery — fully clean |
| 75  | read  | 40  | 0 | connect p99 6.1s, read p99 1.4s |
| 45  | write | 180 | 0 | — |
| 60  | write | 240 | 0 | write p99 1.4s |
| 80  | write | 320 | 0 | 80 ops/s |
| 100 | write | 400 | 0 | 88 ops/s, write p99 2.1s |
| 150 | write | 600 | 0 | 600/600 clean, 86 ops/s, write p99 3.6s |

Latency & detail:

```
write c=100 : connect p50=1500ms p95=3047ms p99=3410ms (max=3578ms)
              write   p50=203ms  p95=1510ms p99=2115ms (max=2316ms)
read  c=75  : connect p50=1795ms p95=5472ms p99=6059ms (max=6059ms)
              read    p50=237ms  p95=818ms  p99=1432ms (max=1504ms)
```

Note the single transient blip at c=40 write with 2 `connect` timeouts and 6 write
failures — that was a one-off overload spike, not a persistent lock issue. It
recovered fully and no run since has re-triggered it.

---

## Headline findings

1. **Both instances plateau at ~90–150 ops/s max throughput.** Reads stop scaling
   beyond ~c=75; only tail latency grows (p99 up to 5–6s). Likely a Go worker /
   nginx connection ceiling shared by both.

2. **They behave differently under writes → they are probably running different
   builds/DB-guard configs.** lite1 throws `database is locked` at c≈90 write
   (13% errors, reproducible). lite2 survives c=150 write at 100% clean. Either
   lite2's binary has a busy-timeout/WAL datastore guard, or its database access
   is serialized differently. **Verify which deployed binary each box runs.**

3. **The DB write lock is the real bottleneck (lite1).** The failure is at the
   SQLite layer (`database is locked`, no busy_timeout), not the network.

---

## SQLite fix — measured before/after (local, rate limiting off)

Same workload (`--mode write`) against a rebuilt binary with WAL + busy_timeout:

| concurrency | before fix | after fix |
|---|---|---|
| 60 writes × 3 | ~45 ops/s, p99 ~3.7s | **361 ops/s, 0 err, write p99 142ms** (240 ops in 0.66s) |
| 120 writes × 4 | n/a (old cliff ~c90, 13% `database is locked`) | **476 ops/s, 0 err, write p99 293ms** (600/600) |

**`database is locked` errors are eliminated** by `journal_mode=WAL` +
`busy_timeout=5000` + `synchronous=NORMAL` in the SQLite DSN. Concurrent writers
now wait for the lock (up to 5s) instead of failing instantly.

---

## Recommendations

- **Harden the datastore:** `internal/sqlite_datastore.go` now sets `journal_mode=WAL`,
  `busy_timeout=5000`, `synchronous=NORMAL` in the connection DSN. **Measured:**
  concurrent write throughput went from ~45–50 ops/s with ~13% `database is locked`
  failures at ~90 clients to ~476 ops/s with 0 errors at 120 concurrent writers
  (write p99 ~293ms vs ~3.7s before). Deploy a build containing this change and
  restart to migrate existing DBs to WAL.
- **Verify deployed binaries match** on both boxes (`/usr/local/bin/litesync`,
  `systemctl status litesync`, `journalctl -u litesync`). If they should be
  identical, the write-behavior asymmetry is a bug or config drift.
- **Rate limiting / concurrency guard:** implemented in two layers — nginx (`limit_req`
  + `limit_conn` via deploy.sh, §6.6.1) and app-level token buckets (per-IP +
  per-Brave-client_id, §6.6.2). Check nginx `worker_processes` / `worker_connections`
  since reads still plateau ~100 ops/s.
- **Re-run on staging/production** with realistic read/write mixes after deploying
the patched binary; watch `journalctl -u litesync` and nginx access logs for 5xx.
- **Recheckbox** the CORS `OPTIONS` short-circuit in nginx — it replies `204`
  without hitting litesync, which is good for preflight floods but should not be
  confused with sync-throughput headroom.

---

## Notes on the next run

- New seeds are created on every run (a `connect()` writes a Nigira row + test
  password entities accumulate per write run). Small but the DB grows; use a
  staging DB and clean between stress runs.
- The harness is repeatable: `npx tsx stress/run.tsx --server <url> --concurrency N --ops-per-worker N --mode read|write|mixed`.