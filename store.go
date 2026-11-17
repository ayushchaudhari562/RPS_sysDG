package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"log"
	"os"
	"strings"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/rueidis"
)

const maxRecords = 1_000_000 // IDs 1..999,999 are cached in the L1 table

var (
	dbPool      *pgxpool.Pool  // nil when PostgreSQL is not configured/reachable
	redisClient rueidis.Client // nil when Redis is not reachable

	// L1 table: one atomic pointer per ID. Reads and writes are lock-free, and
	// a pointer is one machine word, so readers never see a half-written
	// string (a plain string is two words: pointer + length).
	memoryStore [maxRecords]atomic.Pointer[string]

	// Body used when POST /code-write has no body. Hex needs no JSON escaping.
	defaultPayload = randomHex(250)
)

func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// getEnv returns an environment variable, falling back to a KEY=value line
// in ./.env, then to def.
func getEnv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	data, err := os.ReadFile(".env")
	if err != nil {
		return def
	}
	for _, line := range strings.Split(string(data), "\n") {
		if v, ok := strings.CutPrefix(strings.TrimSpace(line), key+"="); ok {
			if v = strings.Trim(strings.TrimSpace(v), `"'`); v != "" {
				return v
			}
		}
	}
	return def
}

// initDB connects the PostgreSQL pool and creates the table. Without a
// database the server still runs; DB routes and writes return 503.
func initDB() {
	url := getEnv("DATABASE_URL", "")
	if url == "" {
		log.Println("WARN: DATABASE_URL not set, PostgreSQL disabled")
		return
	}
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		log.Fatalf("bad DATABASE_URL: %v", err)
	}
	cfg.MaxConns = 30 // Postgres max_connections is 100; leave room for other clients
	cfg.MinConns = 5
	cfg.MaxConnIdleTime = 5 * time.Minute

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err == nil {
		err = pool.Ping(ctx)
	}
	if err != nil {
		log.Printf("WARN: PostgreSQL unavailable, DB routes disabled: %v", err)
		return
	}

	if _, err := pool.Exec(ctx, `CREATE TABLE IF NOT EXISTS codes (id BIGINT PRIMARY KEY, code TEXT NOT NULL)`); err != nil {
		log.Fatalf("create table: %v", err)
	}
	// Continue IDs after the highest stored one so a restart never reuses an ID.
	var maxID int64
	if err := pool.QueryRow(ctx, `SELECT COALESCE(MAX(id), 0) FROM codes`).Scan(&maxID); err != nil {
		log.Fatalf("read MAX(id): %v", err)
	}
	lastID.Store(maxID)
	dbPool = pool
	log.Printf("PostgreSQL ready, next id %d", maxID+1)
}

// loadMemory warms the L1 table. BENCH_FIXTURE=1 fills every slot with the
// same dummy value; otherwise rows are loaded from PostgreSQL.
func loadMemory() {
	if getEnv("BENCH_FIXTURE", "0") == "1" {
		for i := range memoryStore {
			memoryStore[i].Store(&defaultPayload)
		}
		log.Printf("BENCH_FIXTURE: %d dummy records in memory", maxRecords)
		return
	}
	if dbPool == nil {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	rows, err := dbPool.Query(ctx, `SELECT id, code FROM codes WHERE id < $1`, maxRecords)
	if err != nil {
		log.Printf("WARN: preload failed: %v", err)
		return
	}
	defer rows.Close()

	loaded := 0
	for rows.Next() {
		var id int64
		var code string
		if err := rows.Scan(&id, &code); err != nil {
			log.Printf("WARN: preload scan: %v", err)
			return
		}
		escaped := string(appendJSONString(nil, code))
		memoryStore[id].Store(&escaped)
		loaded++
	}
	log.Printf("preloaded %d records from PostgreSQL", loaded)
}

// initRedis connects rueidis with client-side caching: values are cached in
// this process and Redis (RESP3 tracking) tells us when a key changes.
func initRedis() {
	addr := getEnv("REDIS_URL", "127.0.0.1:6379")
	client, err := rueidis.NewClient(rueidis.ClientOption{
		InitAddress:       []string{addr},
		CacheSizeEachConn: 128 << 20, // 128 MB local cache per connection
	})
	if err != nil {
		log.Printf("WARN: Redis unavailable: %v", err)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := client.Do(ctx, client.B().Ping().Build()).Error(); err != nil {
		log.Printf("WARN: Redis ping failed at %s: %v", addr, err)
		client.Close()
		return
	}
	redisClient = client
	log.Printf("Redis ready at %s (client-side cache on)", addr)
}
