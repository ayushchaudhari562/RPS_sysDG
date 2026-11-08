# Go HTTP Engine Benchmark Suite

A high-throughput, low-latency HTTP ingestion and tiered-read engine engineered in Go. The system demonstrates production-grade systems reliability patterns: Linux kernel socket load balancing (`SO_REUSEPORT`), zero-allocation HTTP request parsing (`fasthttp`), lock-free atomic in-memory caching (L1), RESP3 client-side Redis caching (L2), PostgreSQL connection pooling (L3), and asynchronous write-behind batch streaming with backpressure and recursive error bisection.

---

## 1. System Architecture

```mermaid
flowchart TD
    Client["Load Generator (wrk)"] -->|HTTP/1.1 Keep-Alive| Sockets

    subgraph Kernel["Linux Kernel Network Stack"]
        Sockets["16 SO_REUSEPORT Sockets<br/>(4-Tuple Hash Flow Balancing)"]
    end

    Sockets -->|Accept Dispatch| Listeners

    subgraph Server["Go Engine (16 Core Workers)"]
        Listeners["16 fasthttp Listeners"]
        
        Listeners -->|GET /simple| Baseline["2-Byte Ping<br/>(0 Allocations)"]
        Listeners -->|GET /code-fast| L1["L1: Atomic Memory Table<br/>(1M Records, 0 Allocs)"]
        Listeners -->|GET /code-redis| L2["L2: rueidis Client Cache<br/>(RESP3 Tracking, In-Memory)"]
        Listeners -->|GET /code-db| L3["L3: pgxpool Query<br/>(30 Connections)"]
        
        Listeners -->|POST /code-write| Queue["Write-Behind Channel<br/>(200k Buffer, 503 Shedding)"]
    end

    L2 -.->|Cache Miss / Invalidation| Redis[("Redis 8.10.2 (Docker)")]
    
    subgraph Workers["Background Batch Pipeline"]
        Queue -->|Non-Blocking Dequeue| BatchWorkers["4 Batch Workers<br/>(5,000 items / 100ms)"]
        BatchWorkers -->|Zero-Alloc Binary COPY| PG[("PostgreSQL 16 (Local Storage)")]
        BatchWorkers -.->|Async / Sync Write-Through| Redis
        BatchWorkers -->|Permanent Error Bisection| DeadLetter[("dead_letter.jsonl")]
    end
```

<p align="center">
  <img src="img/architecture.jpg" alt="System Architecture Schematic" width="100%" />
</p>

---

## 2. API Specification

| Method | Path | Status | Response Shape | Source Field | Description |
|:---|:---|:---:|:---|:---|:---|
| `GET` | `/simple` | `200 OK` | `OK` (`text/plain`) | N/A | Baseline 2-byte kernel network stack ping. |
| `GET` | `/livez` | `200 OK` | `{"status":"alive"}` | N/A | Kubernetes liveness probe. |
| `GET` | `/readyz` | `200` / `503` | `{"status":"ready","database":"...","redis":"..."}` | N/A | Dependency readiness probe. |
| `GET` | `/code-fast?id=:id` | `200` / `404` | `{"id":42,"code":"..."}` | N/A | L1 Lock-free in-memory table lookup. |
| `GET` | `/code-redis?id=:id`| `200` / `404` | `{"id":42,"code":"...","source":"redis"}` | `"redis"`, `"memory"`, `"postgresql"` | L2 Client-side cache with fallback tiers. |
| `GET` | `/code-db?id=:id` | `200` / `404` | `{"id":42,"code":"...","source":"postgresql"}`| `"postgresql"` | L3 B-Tree indexed read via PostgreSQL pool. |
| `POST`| `/code-write` | `202` / `503` | `{"status":"queued","id":1001}` | N/A | Write-behind queue. Rejects with 503 on saturation. |
| `GET` | `:6060/metrics` | `200 OK` | `{"accepted":...,"persisted":...,"invariant_holds":true}` | N/A | Live accounting metrics and invariant status. |
| `GET` | `:6060/debug/pprof/` | `200 OK` | HTML index | N/A | Standard Go pprof profiling endpoint. |

---

## 3. Measured Performance & Verification Data

All reported figures are taken directly from verifiable benchmark runs documented in [`results.md`](results.md).

### Verified Test Rig & Specifications
- **CPU:** 13th Gen Intel Core i5-13450HX (10 Physical Cores [6P+4E] / 16 logical threads, up to 4.6 GHz)
- **RAM:** **24 GB DDR5** (Lenovo LOQ 15IRX9, 23.71 GB usable)
- **OS / Virtualization:** **WSL2 (Windows Subsystem for Linux 2)**, Kernel `Linux 6.6.87.2-microsoft-standard-WSL2`
- **Database:** PostgreSQL 16.15 (Alpine, Docker container mapped to port 5435, `synchronous_commit=off`, `fsync=on`, `shared_buffers=512MB`)
- **Cache:** Redis 8.10.2 (Docker container mapped to port 6379, RESP3 client-side caching active)
- **Networking:** Local loopback interface (`127.0.0.1`)

### Benchmark Results Table

| Benchmark | Concurrency | Real Measured RPS | Latency (p50) | Latency (p99) | Non-2xx Errors | Status |
|:---|:---:|:---:|:---:|:---:|:---:|:---:|
| **`/simple` (Peak)** | 200 conns / 10t | **874,053.29** | 144 µs | 5.15 ms | 0 | **VALID** |
| **`/simple` (Repeat)** | 200 conns / 10t | **856,167.84** | 147 µs | 4.95 ms | 0 | **VALID** |
| **`/code-redis` (L2 Hit)** | 200 conns / 10t | **828,614.92** | 158 µs | 5.19 ms | 0 | **VALID** |
| **`/code-db` (L3 Direct DB)** | 200 conns / 10t | **49,113.20** | 4.01 ms | 6.62 ms | 0 | **VALID** |
| **`/code-write` (Knee)** | 40 conns / 4t | **246,491.83** | 130 µs | 1.33 ms | 0 | **VALID** |
| **`/code-write` (Overload)**| 200 conns / 10t | **~235,120.00** (accepted) | 210 µs | 6.80 ms | ~395,330 (503s) | **SHED 503** |

### Empirical Terminal Verification Proof

<p align="center">
  <img src="img/linkedin_proof_1_benchmarks.png" alt="Multi-Tier Benchmarks Terminal Proof" width="100%" />
</p>

*Latency step-down curve across tiers (Memory L1 ➔ Redis L2 ➔ PostgreSQL L3):*

<p align="center">
  <img src="img/latency_hierarchy.jpg" alt="Latency Hierarchy Across Tiers" width="100%" />
</p>

### Reliability & Fault-Tolerance Verification
- **Graceful Shutdown (SIGTERM / `kill -15` under load):**  
  **96,968 acknowledged writes**; all **96,968 persisted** during graceful queue drain. **0 data loss (0.00% loss)**.
- **Hard Crash (`kill -9` under active load):**  
  **100,153 acknowledged writes**; **97,058 persisted**; **3,095 writes lost** in the in-flight volatile RAM buffer (**~3.09% loss window in this run**).
- **PostgreSQL Error Bisection Live Test:**  
  Injected corrupted rows (duplicate primary key SQLState 23505 and raw NUL byte SQLState 22021) into 5,000-row batches. In both cases, **exactly 4,999 rows committed to disk** and the **1 bad row was isolated to `dead_letter.jsonl`** across 25–27 binary COPY attempts.

---

## 4. Honest Systems Engineering Analysis

### 1. The Read Path Ceiling: Kernel Loopback, Not Go
Notice that `/simple` (a 2-byte ping doing no application work) ran at **874,053 RPS**, while `/code-redis` (generating a 500-byte JSON response with client-side cache lookups) sustained **828,614 RPS**—a difference of only **~5.2%**.
* In a 30-second `pprof` CPU profile, **52.83% of all Go process samples sat inside raw syscall transitions (`internal/runtime/syscall/linux.Syscall6` executing `epoll_wait`/`readv`/`writev`)** and **11.50% in `runtime.futex`**. Application handler code did not make the top 15 nodes.
* `mpstat` revealed that during loopback benchmarking, kernel networking (`%sys` and `%soft` IRQs) consumed the vast majority of CPU time.

<p align="center">
  <img src="img/linkedin_proof_3_profiling_allocations.png" alt="pprof CPU Profile & Zero-Allocation Benchmarks" width="100%" />
</p>

### 2. Little's Law on PostgreSQL Reads (`/code-db`)
`/code-db` sustained **49,113 RPS** at a p50 latency of **4.01 ms** with 200 concurrent connections.  
By Little's Law ($L = \lambda \times W$):
$$\lambda = \frac{L}{W} = \frac{200\text{ in-flight requests}}{0.00401\text{ seconds}} \approx \mathbf{49,875\text{ req/s}}$$
This aligns with measured throughput. The bottleneck is the connection pool concurrency and database indexed lookup latency, representing an ~17.8x drop from memory ping.

### 3. Write-Behind Ingestion Ceiling & Backpressure
PostgreSQL streaming binary COPY achieves approximately **240,000 to 246,000 rows/second** on local storage with `synchronous_commit=off` and `fsync=on`.
- Under 40 connections: Ingestion matches the database drain rate (~246k rows/s) with zero queue accumulation.
- Under 200 connections: The load generator attempts ~630,000 writes/sec. The 200,000-item channel absorbs burst load, after which non-blocking `select` triggers HTTP 503 load-shedding (~395k writes/s shed). The system protects itself from OOM while preserving data integrity:
  $$\text{accepted} == \text{persisted} + \text{failed} + \text{pending} + \text{queue\_depth}$$

<p align="center">
  <img src="img/write_behind.jpg" alt="Write-Behind Ingestion Pipeline" width="100%" />
</p>

---

## 5. Failure Semantics & Error Handling

1. **Database Offline (`dbPool == nil`):**
   - `POST /code-write` immediately returns `503 Service Unavailable` with `{"error":"database_not_connected"}`.
   - Batch workers never silently skip writes. If a flush fails due to database disconnection, items are marked as failed, decremented from pending, and **never written to Redis**.
2. **Redis Offline / Unavailable:**
   - On connection failure, `/code-redis` returns `502 Bad Gateway`.
   - On cache miss (`nil`), the route gracefully falls back to L1 memory table (`id < 1,000,000`), then PostgreSQL, accurately setting `"source"` in the JSON payload.
3. **Permanent Error Bisection (Poison Pill Rows):**
   - If PostgreSQL returns a permanent error (SQLState Class `22` Data Exception or Class `23` Integrity Violation), the worker bisects the batch recursively:
     - The valid sub-batches commit successfully and write through to Redis.
     - The single poison row is isolated, appended to `dead_letter.jsonl` with error details, and counted as failed without crashing the batch.
   - Transient errors retry 3 times with exponential backoff before marking the batch as failed.

<p align="center">
  <img src="img/linkedin_proof_2_reliability_chaos.png" alt="Reliability, Bisection & Chaos Terminal Proof" width="100%" />
</p>

---

## 6. Rate Limiting & Load Balancing Architecture

A critical architectural consideration at 800k+ RPS is how load balancing, rate limiting, and backpressure are enforced without introducing latency penalties or CPU lock contention:

### 1. Load Balancing: Kernel L4 (`SO_REUSEPORT`) vs. Reverse Proxies
* **Kernel-Level Distribution:**  
  The engine binds 16 independent listener sockets to port `:8081` using `SO_REUSEPORT`. The Linux kernel network stack performs lock-free 4-tuple flow hashing (`src_ip`, `src_port`, `dst_ip`, `dst_port`) directly in the `softirq` bottom half, distributing incoming connections evenly across all 16 CPU cores without user-space lock contention.
* **Why No External Reverse Proxy on Single Host?**  
  At 870,000+ RPS, an intervening user-space proxy on the same host introduces additional TCP socket hops, context switching, and packet serialization overhead that degrades raw throughput *(expected architectural impact; not directly benchmarked against an external proxy in this suite)*.

<p align="center">
  <img src="img/kernel_load_balancer.png" alt="Kernel SO_REUSEPORT Flow Hashing" width="100%" />
</p>

### 2. Rate Limiting: Backpressure Load Shedding vs. Token Bucket
* **Write Path (Active Concurrency & Capacity Limiter):**  
  `POST /code-write` enforces strict **backpressure rate limiting** via a bounded 200,000-slot buffer channel. When incoming write rates exceed the database ingestion drain rate (~246k rows/sec) and the queue fills, excess requests are immediately shed with **HTTP 503 Service Unavailable** (`{"error":"write_queue_full"}`).  
  *Evidence:* During overload testing at ~630,000 writes/sec, the engine safely accepted ~235k writes/sec while shedding ~395k excess writes/sec, preserving 100% data integrity and zero memory leaks.
* **Read Path (Why No In-Process Per-IP Token Bucket):**  
  Evaluating per-IP token bucket rate limits in-process at 870k+ RPS forces atomic lock synchronization or shared map lookups on every request, triggering severe CPU cache-line bouncing (MESI protocol). In production architectures, per-IP rate limiting and DDoS mitigation are strictly offloaded to the **Edge / WAF Layer (Cloudflare, AWS WAF, or Envoy API Gateways)** before traffic touches the cluster.
* **Connection & Payload Caps:**  
  - Server connection cap: `Concurrency: 256 * 1024` (max 262,144 concurrent connections).
  - Body cap: `MaxRequestBodySize: 8 * 1024` (8 KB hard limit in fasthttp, 4 KB in handler) to prevent memory exhaustion attacks.
  - Slowloris protection: Explicit `ReadTimeout: 5s`, `WriteTimeout: 5s`, and `IdleTimeout: 15s`.

---

## 7. Known Limitations & Disclosures

1. **Single-Host Loopback:** Benchmarking server and client on the same host introduces CPU context contention between `wrk` and Go worker threads. Real-world physical 10GbE/40GbE network interfaces introduce NIC interrupt coalescing and network wire latency.
2. **Closed-Loop Load Generator:** `wrk` is a closed-loop load generator that suffers from coordinated omission at high saturations. For open-loop, fixed-rate distribution measurements, use `wrk2`.
3. **Crash Recovery Window:** Because writes are acknowledged at the in-memory queue stage (HTTP 202), an abrupt power outage or `kill -9` will lose in-flight buffered rows (up to 200,000 items). For mission-critical zero-loss durability across power loss, use a durable WAL or Kafka/NATS write-ahead log.
4. **Retry Ambiguity on Network Interruptions:** If a binary `COPY` batch is successfully committed by PostgreSQL but the TCP ACK packet is lost due to a client-side connection timeout, the worker's retry will trigger a duplicate primary key violation (SQLSTATE 23505). In that specific edge case, bisection will route the rows to `dead_letter.jsonl` even though they reside in the database.

---

## 8. How to Run & Benchmark (Quick Guide)

### 1. Windows PowerShell
```powershell
# Compile Windows executable:
go build -o server.exe main.go

# Cross-compile for Linux (WSL2):
$env:GOOS="linux"; $env:GOARCH="amd64"; go build -o server_linux main.go; $env:GOOS="windows"

# Run server on Windows:
$env:BENCH_FIXTURE="1"; .\server.exe
```

---

### 2. Linux Shell (WSL2 / Ubuntu / Kali)
```bash
# 1. Raise open socket file limit:
ulimit -n 65535

# 2. Start server in background:
# (Note: GOGC=off can be used for read-only burst tests, but must never be used for write tests)
BENCH_FIXTURE=1 ./server_linux &

# 3. Check if server is running:
curl http://127.0.0.1:8081/livez

# 4. Stop server cleanly:
pkill -INT -f server_linux
```

---

### 3. Individual `wrk` Benchmark Commands

After starting the server, benchmark individual routes using `wrk`:

- **Baseline Network Ping (`/simple`):**
  ```bash
  wrk -t10 -c200 -d10s --latency http://127.0.0.1:8081/simple
  ```

- **L1 In-Memory RAM Table (`/code-fast`):**
  ```bash
  wrk -t10 -c200 -d10s --latency "http://127.0.0.1:8081/code-fast?id=42"
  ```

- **L2 Redis Cache Read (`/code-redis`):**
  ```bash
  wrk -t10 -c200 -d10s --latency "http://127.0.0.1:8081/code-redis?id=42"
  ```

- **L3 PostgreSQL NVMe Read (`/code-db`):**
  ```bash
  wrk -t10 -c200 -d10s --latency "http://127.0.0.1:8081/code-db?id=42"
  ```

- **L3 Write-Behind Queue Capacity Knee (`POST /code-write`):**
  ```bash
  wrk -t4 -c40 -d10s --latency -s bench/lua/body_variants.lua http://127.0.0.1:8081/code-write
  ```

- **L3 Overload Backpressure Test (`POST /code-write`):**
  ```bash
  wrk -t10 -c200 -d10s --latency -s bench/lua/body_variants.lua http://127.0.0.1:8081/code-write
  ```

---

### 4. Automated Benchmark & Chaos Bash Scripts

To execute automated multi-run suites:

- **Standard Benchmark Suite:**
  ```bash
  chmod +x scripts/bench.sh
  ./scripts/bench.sh
  ```

<p align="center">
  <img src="img/linkedin_proof_4_bench_script.png" alt="Automated Benchmark Suite Execution" width="100%" />
</p>

- **Strict Benchmark Suite (CPU Pinning + Logging):**
  ```bash
  chmod +x bench/run_bench.sh
  ./bench/run_bench.sh
  ```

- **Chaos Fault-Tolerance Test (SIGTERM vs kill -9 Recovery Test):**
  ```bash
  chmod +x bench/chaos.sh
  ./bench/chaos.sh
  ```

- **5-Minute Soak Test:**
  ```bash
  chmod +x bench/soak.sh
  ./bench/soak.sh
  ```

---

## 9. License

Distributed under the [MIT License](LICENSE).
