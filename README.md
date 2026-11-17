# go-1m-rps

A Go HTTP server that serves **~874K requests/sec** on one laptop, with a
3-tier read path (RAM → Redis → PostgreSQL) and a write-behind queue that
batches writes into PostgreSQL.

| Route | Peak RPS | p50 |
|---|---:|---:|
| `GET /simple` (2-byte ping) | 874,053 | 144 µs |
| `GET /code-redis` (L2 hit) | 828,614 | 158 µs |
| `GET /code-db` (L3, direct) | 49,113* | 4.01 ms |
| `POST /code-write` (no 503s) | 246,491 | 130 µs |

Measured on an i5-13450HX (16 threads), WSL2, loopback, `wrk -t10 -c200`.
Full raw outputs and caveats: [results.md](results.md).
\*re-run pending, see the note in results.md.

---

## 1. Architecture

```
client ──HTTP──► Linux kernel: 16 SO_REUSEPORT sockets on :8081
                        │
                  16 × fasthttp server ── handler()
                        │
   ┌──────────────┬─────┴────────┬───────────────┐
GET /code-fast  GET /code-redis  GET /code-db    POST /code-write
   │              │              │               │
 L1 RAM        L2 Redis        L3 PostgreSQL   writeQueue (200k)──full──► 503
 atomic array  client cache    pool of 30        │
               miss → L1 → L3                  4 workers: 5000 items OR 100ms
                                                 │ binary COPY
                                               PostgreSQL ──bad row──► dead_letter.jsonl
                                                 │ ok
                                               redisQueue ──► Redis SET

internal :6060 → /metrics (counters) and /debug/pprof
```

<p align="center"><img src="img/hld_architecture.png" alt="HLD" width="90%"></p>

**Four design choices**

1. **Kernel load balancing (`SO_REUSEPORT`).** One listener per core. The kernel hashes each connection to a socket, so the cores never compete for one accept queue and no proxy sits in front.
2. **Three read tiers.** RAM takes nanoseconds, Redis microseconds, PostgreSQL milliseconds. Each route shows the cost of one tier.
3. **Write-behind.** Writes are acknowledged (`202`) once they are queued and saved in batches. You get fast writes; the cost is that a hard crash loses whatever is still queued.
4. **Bounded queue + `503`.** When writes arrive faster than PostgreSQL can absorb them, the server rejects the extra requests instead of running out of memory.

---

## 2. Code layout

| File | What it does |
|---|---|
| [main.go](main.go) | Startup order, one listener per core, graceful shutdown |
| [store.go](store.go) | `.env` config, L1 memory table, PostgreSQL pool, Redis client |
| [handlers.go](handlers.go) | All HTTP routes + zero-allocation helpers |
| [writer.go](writer.go) | Write-behind: queue → batch → COPY → retry/bisection → Redis |
| [metrics.go](metrics.go) | Counters, the accounting invariant, `/metrics`, pprof |
| [main_test.go](main_test.go) | Unit tests and allocation benchmarks |
| [bench/](bench/) | Load, soak and chaos tests, Lua scripts for `wrk`, DB/Redis seeder |

---

## 3. API

| Method | Path | Success | Notes |
|---|---|---|---|
| GET | `/simple` | `200 OK` | Baseline, no app work |
| GET | `/livez` | `200 {"status":"alive"}` | Liveness probe |
| GET | `/readyz` | `200` / `503` | Pings PostgreSQL and Redis |
| GET | `/code-fast?id=N` | `200 {"id","code"}` | L1 only |
| GET | `/code-redis?id=N` | `200 {"id","code","source"}` | `source` = `redis` / `memory` / `postgresql` |
| GET | `/code-db?id=N` | `200 {"id","code","source"}` | L3 only, 200 ms timeout |
| POST | `/code-write` | `202 {"status":"queued","id":N}` | `503` when queue full, `413` > 4 KB, `400` bad UTF-8 / NUL |
| GET | `:6060/metrics` | counters JSON | Internal port only |

---

## 4. How it works

### Read path
- **L1:** `[1_000_000]atomic.Pointer[string]`. Lock-free, and a pointer is one machine word, so a reader can never see half of a write. Values are stored already JSON-escaped, so a read only copies bytes.
- **L2:** `rueidis` with RESP3 client-side caching. Hot keys are served from this process's memory; Redis sends an invalidation message when a key changes. A miss falls back to L1, then PostgreSQL, and the value found is queued for Redis (cache warming).
- **Zero allocations:** routing uses `switch string(path)`, which the compiler turns into a byte comparison. Responses are appended into fasthttp's reused buffer, and IDs are parsed straight from bytes. `/simple` and `/code-fast` report **0 allocs/op**.

### Write path
1. The handler validates the body, takes `id = lastID.Add(1)`, stores the value in L1 (read-your-writes), then does a **non-blocking** send to `writeQueue`: if the send succeeds the client gets `202`, if the queue is full it gets `503`.
2. 4 workers collect batches and flush at **5000 items or every 100 ms**, whichever comes first, using PostgreSQL **binary COPY**.
3. **Transient error** → retry 3× (50/100/200 ms). **Permanent error** (SQLSTATE 22/23, e.g. duplicate key or NUL byte) → split the batch in half recursively until the one bad row is found and written to `dead_letter.jsonl`. The other 4,999 rows are saved, in about 26 COPY calls.
4. A failed write is removed from L1 (`CompareAndSwap`, so a newer value in the same slot stays).
5. Saved batches go to Redis via one background goroutine that pipelines them (`DoMulti`).

### Accounting invariant
```
accepted == persisted + failed + pending + queue_depth
```
Every `202` is always in exactly one of those four places. `/metrics` reports `unaccounted`. Under live load it can be off by a few in-flight requests, because the counters are read one at a time; after traffic stops it must be 0.

### Graceful shutdown (order matters)
Stop the HTTP servers (no new writes) → workers drain the queue → Redis writer drains → close the file, the DB pool and Redis. `redisQueue` is never closed, because a late handler sending on a closed channel would panic.

---

## 5. Reliability results

| Test | Result |
|---|---|
| `kill -15` under load | 96,968 acked, 96,968 saved, **0 lost** |
| `kill -9` under load | 100,153 acked, 97,058 saved, **3,095 lost (~3%)**, the queued items in RAM |
| 1 duplicate key in a 5,000-row batch | 4,999 saved, 1 dead-lettered, 25 COPY calls |
| 1 NUL byte in a 5,000-row batch | 4,999 saved, 1 dead-lettered, 27 COPY calls |
| Overload (~630K writes/s offered) | ~235K/s accepted, ~395K/s shed with `503`, no crash |

---

## 6. Limits (said plainly)

- **Not 1M RPS.** The peak was 874K. pprof shows 52.8% of CPU time in syscalls (`epoll`/`read`/`write`), so loopback networking is the bottleneck, not the Go code.
- **WSL2 + loopback.** `wrk` and the server share one CPU. Real NICs behave differently.
- **`synchronous_commit=off`.** This is how the 246K rows/s write rate is reached. A PostgreSQL crash can lose the last few hundred ms of commits (`fsync` is on, so no corruption).
- **`202` before durability.** A `kill -9` loses whatever is queued. For zero loss, put a durable log (Kafka/NATS/WAL) in front.
- **Retry ambiguity.** If a COPY commits but the reply is lost, the retry hits a duplicate key and those rows are dead-lettered even though they are saved. Fix: idempotency keys, or staging table + `ON CONFLICT DO NOTHING`.
- **Single instance.** IDs come from an in-process counter. Running several instances needs a DB sequence or Snowflake IDs, and each instance would have its own L1.
- **`wrk` is closed-loop** (coordinated omission). Use `wrk2` for fixed-rate latency.

---

## 7. Bugs found while building it

| Bug | Fix |
|---|---|
| `string(ctx.Path())` in an `if`, `strconv.Atoi(string(b))`, `fmt.Fprintf` allocated on every request | `switch`, byte parser, append into the reused buffer |
| One goroutine per write for Redis → 200K goroutines/s | One Redis writer fed by a bounded channel |
| Full queue silently dropped writes but returned `201` | Non-blocking `select` → `202` or `503` |
| `close(writeQueue)` on shutdown → `send on closed channel` panic | Stop producers first, signal consumers with a separate channel |
| One bad row aborted the whole 5,000-row COPY | Recursive bisection + dead letter file |
| `invariant_holds` was always true (pending was derived from the other counters) | Independent `pending` counter + a test that loses an item |
| `/code-db` JSON missing a `"` | Shared `writeRecord` helper + test |
| `n*10` overflow could wrap to a *positive* ID | Check before multiplying |
| Docker Postgres on 5432 clashed with a Windows Postgres service | Mapped to 5435 |

---

## 8. Run it

```bash
cp .env.example .env
make up          # PostgreSQL :5435 + Redis :6379 (Docker)
make seed        # 1M rows + 100K Redis keys
make run         # or: make build && ./server_linux   (Linux/WSL2)

curl localhost:8081/livez
curl "localhost:8081/code-fast?id=42"
curl -X POST -d 'hello' localhost:8081/code-write
curl localhost:6060/metrics
```

Benchmarks (Linux/WSL2, needs `wrk`):

```bash
ulimit -n 65535
wrk -t10 -c200 -d10s --latency http://127.0.0.1:8081/simple
wrk -t4  -c40  -d10s --latency -s bench/lua/body_variants.lua http://127.0.0.1:8081/code-write
make bench   # full suite      make chaos   # SIGTERM vs kill -9      make soak   # 5 min
make test    # vet + unit tests + allocation benchmarks
```

License: [MIT](LICENSE)
