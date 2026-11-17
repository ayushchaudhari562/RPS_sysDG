package main

import (
	"bytes"
	"context"
	"errors"
	"math"
	"os"
	"strconv"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/redis/rueidis"
	"github.com/valyala/fasthttp"
)

const maxCodeLen = 4096 // max POST body in bytes

// handler routes every request. `switch string(b)` does not allocate: the Go
// compiler compares the bytes directly.
func handler(ctx *fasthttp.RequestCtx) {
	if ctx.IsPost() && string(ctx.Path()) == "/code-write" {
		handleWrite(ctx)
		return
	}
	if !ctx.IsGet() {
		writeJSON(ctx, fasthttp.StatusNotFound, `{"error":"not_found"}`)
		return
	}
	switch string(ctx.Path()) {
	case "/simple":
		ctx.SetContentType("text/plain; charset=utf-8")
		ctx.SetBodyString("OK")
	case "/livez":
		writeJSON(ctx, fasthttp.StatusOK, `{"status":"alive"}`)
	case "/readyz":
		handleReady(ctx)
	case "/code-fast":
		handleFast(ctx)
	case "/code-redis":
		handleRedis(ctx)
	case "/code-db":
		handleDB(ctx)
	default:
		writeJSON(ctx, fasthttp.StatusNotFound, `{"error":"not_found"}`)
	}
}

// handleReady pings both dependencies (Kubernetes readiness probe).
func handleReady(ctx *fasthttp.RequestCtx) {
	pingCtx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	dbOK := dbPool != nil && dbPool.Ping(pingCtx) == nil
	redisOK := redisClient != nil && redisClient.Do(pingCtx, redisClient.B().Ping().Build()).Error() == nil

	status, code := "ready", fasthttp.StatusOK
	if !dbOK || !redisOK {
		status, code = "degraded", fasthttp.StatusServiceUnavailable
	}
	writeJSON(ctx, code, `{"status":"`+status+`","database":"`+state(dbOK)+`","redis":"`+state(redisOK)+`"}`)
}

func state(ok bool) string {
	if ok {
		return "connected"
	}
	return "disconnected"
}

// handleFast: L1 only. Lock-free array lookup, zero allocations.
func handleFast(ctx *fasthttp.RequestCtx) {
	id, ok := readID(ctx)
	if !ok {
		return
	}
	if v := fromMemory(id); v != "" {
		writeRecord(ctx, id, v, "")
		return
	}
	writeJSON(ctx, fasthttp.StatusNotFound, `{"error":"record_not_found"}`)
}

// handleRedis: L2 (Redis client-side cache) -> L1 -> L3 (PostgreSQL).
// A value found below Redis is queued for Redis so the next read hits L2.
func handleRedis(ctx *fasthttp.RequestCtx) {
	id, ok := readID(ctx)
	if !ok {
		return
	}
	if redisClient == nil {
		writeJSON(ctx, fasthttp.StatusServiceUnavailable, `{"error":"redis_not_connected"}`)
		return
	}

	cmd := redisClient.B().Get().Key("code:" + strconv.FormatInt(id, 10)).Cache()
	val, err := redisClient.DoCache(ctx, cmd, 10*time.Minute).ToString()
	source := "redis"
	if err != nil {
		if !rueidis.IsRedisNil(err) {
			stats.redisErrors.Add(1)
			writeJSON(ctx, fasthttp.StatusBadGateway, `{"error":"redis_unavailable"}`)
			return
		}
		if val, source = fromMemory(id), "memory"; val == "" {
			val, source = fromDB(id), "postgresql"
		}
		if val == "" {
			writeJSON(ctx, fasthttp.StatusNotFound, `{"error":"record_not_found"}`)
			return
		}
		select { // non-blocking: never slow a read down for cache warming
		case redisQueue <- []Item{{ID: id, Escaped: val}}:
		default:
		}
	}
	writeRecord(ctx, id, val, source)
}

// handleDB: L3 only, a direct indexed lookup with a 200ms timeout.
func handleDB(ctx *fasthttp.RequestCtx) {
	id, ok := readID(ctx)
	if !ok {
		return
	}
	if dbPool == nil {
		writeJSON(ctx, fasthttp.StatusServiceUnavailable, `{"error":"database_not_connected"}`)
		return
	}
	dbCtx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	var code string
	err := dbPool.QueryRow(dbCtx, `SELECT code FROM codes WHERE id = $1`, id).Scan(&code)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		writeJSON(ctx, fasthttp.StatusNotFound, `{"error":"record_not_found"}`)
	case err != nil:
		writeJSON(ctx, fasthttp.StatusInternalServerError, `{"error":"database_query_failed"}`)
	default:
		writeRecord(ctx, id, string(appendJSONString(nil, code)), "postgresql")
	}
}

// handleWrite validates the body, puts it in L1 and the write queue, and
// answers 202 at once. If the queue is full it answers 503 (backpressure).
func handleWrite(ctx *fasthttp.RequestCtx) {
	if dbPool == nil && os.Getenv("ALLOW_NO_DB") != "1" {
		writeJSON(ctx, fasthttp.StatusServiceUnavailable, `{"error":"database_not_connected","message":"writes disabled when database is offline"}`)
		return
	}
	body := ctx.PostBody()
	if len(body) > maxCodeLen {
		writeJSON(ctx, fasthttp.StatusRequestEntityTooLarge, `{"error":"payload_too_large","max_bytes":4096}`)
		return
	}
	// PostgreSQL TEXT rejects NUL bytes and invalid UTF-8, so reject them here.
	if !utf8.Valid(body) || bytes.IndexByte(body, 0) >= 0 {
		writeJSON(ctx, fasthttp.StatusBadRequest, `{"error":"invalid_payload","message":"must be valid utf-8 without null bytes"}`)
		return
	}

	raw, escaped := defaultPayload, defaultPayload
	if len(body) > 0 {
		raw = string(body)
		escaped = string(appendJSONString(nil, raw)) // escape once on write, never on read
	}
	id := lastID.Add(1) // unique, lock-free; a 503 below leaves a gap (fine, like a SEQUENCE)

	// Read-your-writes: store in L1 BEFORE enqueueing. Once queued, a worker
	// may fail the item and evict it; storing afterwards would bring it back.
	var memPtr *string
	if id < maxRecords {
		memPtr = &escaped
		memoryStore[id].Store(memPtr)
	}

	select {
	case writeQueue <- Item{ID: id, Raw: raw, Escaped: escaped, memPtr: memPtr}:
		stats.accepted.Add(1)
		ctx.SetContentType("application/json")
		ctx.SetStatusCode(fasthttp.StatusAccepted)
		b := append(ctx.Response.Body()[:0], `{"status":"queued","id":`...)
		b = strconv.AppendInt(b, id, 10)
		ctx.SetBody(append(b, '}'))
	default:
		if memPtr != nil {
			memoryStore[id].CompareAndSwap(memPtr, nil)
		}
		stats.rejected.Add(1)
		writeJSON(ctx, fasthttp.StatusServiceUnavailable, `{"error":"write_queue_full","message":"backpressure triggered, retry later"}`)
	}
}

// ---- helpers ----

func fromMemory(id int64) string {
	if id >= maxRecords {
		return ""
	}
	if p := memoryStore[id].Load(); p != nil {
		return *p
	}
	return ""
}

// fromDB returns the JSON-escaped code for id, or "" if missing or on error.
func fromDB(id int64) string {
	if dbPool == nil {
		return ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	var code string
	if dbPool.QueryRow(ctx, `SELECT code FROM codes WHERE id = $1`, id).Scan(&code) != nil {
		return ""
	}
	return string(appendJSONString(nil, code))
}

// readID parses ?id= and answers 400 itself when it is not a positive integer.
func readID(ctx *fasthttp.RequestCtx) (int64, bool) {
	id, ok := parsePositiveInt64(ctx.QueryArgs().Peek("id"))
	if !ok {
		writeJSON(ctx, fasthttp.StatusBadRequest, `{"error":"invalid_id","message":"id must be a positive integer"}`)
	}
	return id, ok
}

func writeJSON(ctx *fasthttp.RequestCtx, status int, body string) {
	ctx.SetContentType("application/json")
	ctx.SetStatusCode(status)
	ctx.SetBodyString(body)
}

// writeRecord writes {"id":N,"code":"...","source":"..."} into fasthttp's
// reused body buffer (no allocation). code must already be JSON-escaped.
func writeRecord(ctx *fasthttp.RequestCtx, id int64, code, source string) {
	ctx.SetContentType("application/json")
	b := append(ctx.Response.Body()[:0], `{"id":`...)
	b = strconv.AppendInt(b, id, 10)
	b = append(b, `,"code":"`...)
	b = append(b, code...)
	b = append(b, '"')
	if source != "" {
		b = append(b, `,"source":"`...)
		b = append(b, source...)
		b = append(b, '"')
	}
	ctx.SetBody(append(b, '}'))
}

// parsePositiveInt64 parses ASCII digits without allocating (strconv.Atoi
// would need a string). Rejects 0, signs, and overflow.
func parsePositiveInt64(b []byte) (int64, bool) {
	if len(b) == 0 {
		return 0, false
	}
	var n int64
	for _, c := range b {
		if c < '0' || c > '9' {
			return 0, false
		}
		d := int64(c - '0')
		if n > (math.MaxInt64-d)/10 { // check BEFORE multiplying: n*10 can wrap to a positive number
			return 0, false
		}
		n = n*10 + d
	}
	return n, n > 0
}

// appendJSONString appends s with JSON string escaping (no surrounding quotes).
func appendJSONString(dst []byte, s string) []byte {
	const hexDigits = "0123456789abcdef"
	for i := 0; i < len(s); i++ {
		switch c := s[i]; {
		case c == '"' || c == '\\':
			dst = append(dst, '\\', c)
		case c < 0x20:
			dst = append(dst, '\\', 'u', '0', '0', hexDigits[c>>4], hexDigits[c&0xf])
		default:
			dst = append(dst, c)
		}
	}
	return dst
}
