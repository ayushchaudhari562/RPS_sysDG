package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/rueidis"
)

func getEnv(key, defaultVal string) string {
	if val := os.Getenv(key); val != "" {
		return val
	}
	data, err := os.ReadFile(".env")
	if err == nil {
		for _, line := range strings.Split(string(data), "\n") {
			line = strings.TrimSpace(line)
			if strings.HasPrefix(line, key+"=") {
				parts := strings.SplitN(line, "=", 2)
				if len(parts) == 2 {
					val := strings.TrimSpace(parts[1])
					return strings.Trim(val, `"'`)
				}
			}
		}
	}
	return defaultVal
}

func seedPostgres(pgURL string, totalRows int) error {
	log.Printf("Connecting to PostgreSQL at %s...", pgURL)
	config, err := pgxpool.ParseConfig(pgURL)
	if err != nil {
		return fmt.Errorf("parse config: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		return fmt.Errorf("connect pool: %w", err)
	}
	defer pool.Close()

	if err := pool.Ping(ctx); err != nil {
		return fmt.Errorf("ping db: %w", err)
	}

	log.Println("Ensuring schema: codes (id BIGINT PRIMARY KEY, code TEXT NOT NULL)...")
	_, err = pool.Exec(ctx, `
		CREATE TABLE IF NOT EXISTS codes (
			id BIGINT PRIMARY KEY,
			code TEXT NOT NULL
		);
	`)
	if err != nil {
		return fmt.Errorf("create table: %w", err)
	}

	log.Printf("Seeding %d rows (~500 bytes each) via INSERT..SELECT generate_series...", totalRows)
	start := time.Now()

	query := fmt.Sprintf(`
		INSERT INTO codes (id, code)
		SELECT i, repeat(md5(i::text), 15)
		FROM generate_series(1, %d) AS s(i)
		ON CONFLICT (id) DO UPDATE SET code = EXCLUDED.code;
	`, totalRows)

	opCtx, opCancel := context.WithTimeout(context.Background(), 300*time.Second)
	defer opCancel()

	tag, err := pool.Exec(opCtx, query)
	if err != nil {
		return fmt.Errorf("exec insert: %w", err)
	}

	elapsed := time.Since(start)
	rate := float64(totalRows) / elapsed.Seconds()
	log.Printf("Successfully seeded %d rows in %v (%.0f rows/s, tag: %s)", totalRows, elapsed, rate, tag.String())
	return nil
}

func seedRedis(redisURL string, totalKeys int) error {
	log.Printf("Connecting to Redis at %s...", redisURL)
	client, err := rueidis.NewClient(rueidis.ClientOption{
		InitAddress: []string{redisURL},
	})
	if err != nil {
		return fmt.Errorf("redis client: %w", err)
	}
	defer client.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := client.Do(ctx, client.B().Ping().Build()).Error(); err != nil {
		return fmt.Errorf("redis ping: %w", err)
	}

	log.Printf("Seeding %d Redis keys (code:1..%d) pipelined...", totalKeys, totalKeys)
	start := time.Now()

	payload500 := strings.Repeat("0123456789abcdef", 31)
	batchSize := 2000
	cmds := make(rueidis.Commands, 0, batchSize)

	for i := 1; i <= totalKeys; i++ {
		key := "code:" + strconv.Itoa(i)
		cmd := client.B().Set().Key(key).Value(payload500).Ex(24 * time.Hour).Build()
		cmds = append(cmds, cmd)

		if len(cmds) >= batchSize || i == totalKeys {
			bCtx, bCancel := context.WithTimeout(context.Background(), 5*time.Second)
			resps := client.DoMulti(bCtx, cmds...)
			bCancel()
			for _, r := range resps {
				if r.Error() != nil {
					return fmt.Errorf("redis set error: %w", r.Error())
				}
			}
			cmds = cmds[:0]
		}
	}

	elapsed := time.Since(start)
	rate := float64(totalKeys) / elapsed.Seconds()
	log.Printf("Successfully seeded %d keys into Redis in %v (%.0f keys/s)", totalKeys, elapsed, rate)
	return nil
}

func main() {
	doPG := flag.Bool("pg", false, "Seed PostgreSQL")
	doRedis := flag.Bool("redis", false, "Seed Redis")
	pgRows := flag.Int("pg-rows", 1000000, "Number of rows for PostgreSQL")
	redisKeys := flag.Int("redis-keys", 100000, "Number of keys for Redis (code:1..100000)")
	flag.Parse()

	if !*doPG && !*doRedis {
		*doPG = true
		*doRedis = true
	}

	pgURL := getEnv("DATABASE_URL", "postgres://postgres:postgrespassword@127.0.0.1:5435/1Million?sslmode=disable")
	redisURL := getEnv("REDIS_URL", "127.0.0.1:6379")

	if *doPG {
		if err := seedPostgres(pgURL, *pgRows); err != nil {
			log.Fatalf("PostgreSQL seed failed: %v", err)
		}
	}

	if *doRedis {
		if err := seedRedis(redisURL, *redisKeys); err != nil {
			log.Fatalf("Redis seed failed: %v", err)
		}
	}
}
