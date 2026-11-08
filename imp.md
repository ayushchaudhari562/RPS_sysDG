# Master Engineering Audit: Bugs, Loopholes, & Architectural Solutions (`imp.md`)

**Repository:** `go-1m-rps`  
**Purpose:** Technical log of real concurrency pitfalls, architectural trade-offs, and critical bug fixes discovered while building the high-throughput Go HTTP engine.

---

## 1. Zero-Allocation & Hot-Path Optimizations

At sub-millisecond latencies, heap allocations trigger garbage collection (GC) cycles that degrade tail latencies. The following hot-path optimizations eliminated heap allocations on the read path:

### A. Direct Byte Path Matching vs String Casting
* **The Pitfall:**  
  Early prototypes routed incoming requests using string conversion:
  ```go
  // BAD: Allocates a new string on the heap for EVERY request
  switch string(ctx.Path()) {
  case "/simple":
      // ...
  }
  ```
  `ctx.Path()` returns a `[]byte`. Casting `string(ctx.Path())` copies the slice into a heap-allocated string, generating massive garbage churn under high concurrency.
* **The Solution:**  
  Defined static byte slice constants at package level and used `bytes.Equal`:
  ```go
  // ZERO-ALLOCATION: Direct byte comparison in assembly (runtime.memequal)
  var (
      pathSimple    = []byte("/simple")
      pathCodeFast  = []byte("/code-fast")
      pathCodeRedis = []byte("/code-redis")
      pathCodeWrite = []byte("/code-write")
  )

  path := ctx.Path()
  if bytes.Equal(path, pathSimple) {
      // zero allocations
  }
  ```

---

### B. In-Place ASCII Integer Parser
* **The Pitfall:**  
  Parsing query parameters originally used standard library helpers:
  ```go
  // BAD: Allocates string from byte slice and formats error strings on failure
  idBytes := ctx.QueryArgs().Peek("id")
  id, err := strconv.Atoi(string(idBytes))
  ```
* **The Solution:**  
  Implemented a zero-allocation integer parser operating directly on ASCII byte slices:
  ```go
  // ZERO-ALLOCATION: Direct byte iteration with overflow protection
  func parsePositiveInt64(b []byte) (int64, bool) {
      if len(b) == 0 {
          return 0, false
      }
      var n int64
      for _, c := range b {
          if c < '0' || c > '9' {
              return 0, false
          }
          n = n*10 + int64(c-'0')
          if n < 0 {
              return 0, false // Overflow protection
          }
      }
      return n, true
  }
  ```

---

### C. Response Buffer Slice Reuse vs `fmt.Fprintf`
* **The Pitfall:**  
  Early JSON generation used `fmt.Fprintf`:
  ```go
  // BAD: Reflection-based formatting and string allocations
  fmt.Fprintf(ctx, `{"id":%d,"code":"%s"}`, id, code)
  ```
* **The Solution:**  
  Reused the pre-allocated fasthttp response buffer using `strconv.AppendInt`:
  ```go
  // ZERO-ALLOCATION: Append directly to recycled response buffer
  buf := ctx.Response.Body()[:0]
  buf = append(buf, `{"id":`...)
  buf = strconv.AppendInt(buf, id, 10)
  buf = append(buf, `,"code":"`...)
  buf = append(buf, code...)
  buf = append(buf, `"}`...)
  ctx.Response.SetBody(buf)
  ```

---

## 2. Concurrency & Queue Engineering

### A. The Unbounded Goroutine Storm
* **The Bug (Commit `ef965ae`):**  
  In early versions of the write route, asynchronous Redis updates were spawned directly inside the HTTP handler:
  ```go
  // CATASTROPHIC: Spawns an unconstrained goroutine per request
  if redisClient != nil {
      go func(id int, code string) {
          key := "code:" + strconv.Itoa(id)
          setCmd := redisClient.B().Set().Key(key).Value(code).Ex(10 * time.Minute).Build()
          _ = redisClient.Do(context.Background(), setCmd)
      }(newID, newCode)
  }
  ```
  Under heavy write load (>200k writes/s), this spawned over 200,000 goroutines per second, causing massive Go runtime scheduler contention and overwhelming Redis with concurrent TCP connections.
* **The Solution (Commit `2c6cb6a`):**  
  Replaced per-request goroutines with a **bounded worker pool**:
  1. Requests enqueue items into a bounded channel buffer (`redisQueue = make(chan []Item, 1000)`).
  2. Fixed background workers dequeue batches and pipeline them using `DoMulti`, bounding concurrency and preventing goroutine leaks.

---

### B. Silent Data Loss on Saturated Queue (The 201 Bug)
* **The Bug (Commits `f842be0` & `ef965ae`):**  
  When incoming write rates exceeded database flush capacity, the non-blocking channel send silently dropped unbuffered writes while still returning `201 Created`:
  ```go
  // CATASTROPHIC BUG: Drops write silently but tells client it succeeded!
  select {
  case writeQueue <- Item{ID: newID, Code: newCode}:
  default:
  }

  ctx.SetStatusCode(fasthttp.StatusCreated)
  fmt.Fprintf(ctx, `{"status":"queued","id":%d}`, newID)
  ```
* **The Solution (Commit `2c6cb6a`):**  
  Implemented **explicit backpressure load shedding**:
  1. Return `HTTP 202 Accepted` only when the item is successfully enqueued into `writeQueue`.
  2. If the channel is full, return **`HTTP 503 Service Unavailable`** (`{"error":"write_queue_full"}`) so upstream clients and load balancers know the write was shed and must be retried.

---

### C. Data Race & Torn Reads: `atomic.Pointer`
* **The Pitfall:**  
  In Go, a `string` header is two machine words (data pointer and length). When multiple worker threads read and write to an in-memory cache slice without synchronization, concurrent writes can trigger a **torn read**, where a reader sees the pointer from one write and the length from another, resulting in segmentation faults or garbage bytes.
* **The Solution:**  
  Used an array of atomic pointers:
  ```go
  var memoryStore [TOTAL_RECORDS]atomic.Pointer[string]

  // Atomic pointer load is lock-free and race-free
  ptr := memoryStore[id].Load()
  ```

---

## 3. Storage Layer & Error Handling

### A. PostgreSQL 5,000-Row Poison Pill Batch Crash
* **The Problem:**  
  Streaming rows to PostgreSQL using binary `CopyFrom` executes within a single database transaction. If **one single row** in a 5,000-item batch violates a constraint (e.g. duplicate primary key `SQLSTATE 23505` or invalid UTF-8 NUL byte `SQLSTATE 22021`), PostgreSQL aborts the **entire transaction**, discarding all 5,000 rows.
* **The Solution: Recursive Binary Search Bisection:**  
  When `flushBatchToDB` encounters a permanent error:
  1. If `len(batch) == 1`: The isolated poisonous row is written to `dead_letter.jsonl` with error metadata, counted as `failedWrites`, and skipped.
  2. If `len(batch) > 1`: The batch is split into two halves and recursively processed:
     ```go
     mid := len(batch) / 2
     flushBatchWithRetryAndBisection(batch[:mid])
     flushBatchWithRetryAndBisection(batch[mid:])
     ```
  3. Valid sub-batches commit successfully. A single bad row is isolated in $\approx 25-27$ `CopyFrom` calls, committing the remaining 4,999 valid rows to disk.

---

### B. Transient Error Retries with Exponential Backoff
* **Implementation:**  
  Database connection drops, network timeouts, or lock contention are classified as transient errors. The flusher retries up to 3 times with exponential backoff (`time.Duration(50<<attempt) * time.Millisecond`). If all 3 retries are exhausted, the batch is recorded as failed, decremented from pending, and logged.

---

### C. Known Limitation: Retry Ambiguity on Network Drops
* **The Edge Case:**  
  If PostgreSQL successfully commits a binary `CopyFrom` batch, but the connection's TCP ACK is lost due to a sudden network drop, the client flusher interprets this as a network failure and retries.
* **The Consequence:**  
  Upon retry, PostgreSQL rejects the batch with duplicate key violation (`SQLSTATE 23505`). Because 23505 is classified as permanent, bisection will route these rows to `dead_letter.jsonl` even though they already exist in the database.
* **Mitigation:** In production architectures, database tables should leverage idempotency keys, unique request deduplication, or handle conflict resolution (`ON CONFLICT DO NOTHING`).

---

## 4. Operational & Infrastructure Bugs

### A. PostgreSQL Port 5432 Collision (Windows Service vs Docker)
* **What Happened:** Connecting to Docker PostgreSQL failed with `SQLSTATE 28P01 (password authentication failed)`.
* **Root Cause:** A host Windows background service (`postgresql-x64-18`) was already bound to port `5432`. Docker was unable to route traffic properly or connected to the wrong instance.
* **Solution:** Re-mapped Docker PostgreSQL container to port `5435:5432` (`127.0.0.1:5435`) and loaded connection strings dynamically via `.env`.

---

### B. Graceful Shutdown Panic (`close(writeQueue)`)
* **What Happened:** On receiving `SIGTERM`, calling `close(writeQueue)` immediately caused active HTTP worker goroutines to panic with `panic: send on closed channel`.
* **Solution: 3-Phase Shutdown Sequence:**
  1. Stop accepting new traffic: Call `server.ShutdownWithContext()` on all HTTP listener sockets.
  2. Drain remaining items: Allow background workers to flush all buffered items from `writeQueue` to PostgreSQL.
  3. Close pools: Close PostgreSQL connection pool and Redis clients cleanly.
