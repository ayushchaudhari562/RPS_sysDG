//go:build ignore

package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type seedSource struct {
	total int    
	idx   int    
	row   [2]any 
	hex   string 
}

func (s *seedSource) Next() bool {
	s.idx++
	return s.idx <= s.total
}

func (s *seedSource) Values() ([]any, error) {
	s.row[0] = int64(s.idx)
	s.row[1] = s.hex
	return s.row[:], nil
}

func (s *seedSource) Err() error {
	return nil
}

func loadEnvURL() string {
	if u := os.Getenv("DATABASE_URL"); u != "" {
		return u
	}
	data, err := os.ReadFile(".env")
	if err == nil {
		lines := strings.Split(string(data), "\n")
		for _, line := range lines {
			line = strings.TrimSpace(line)
			if strings.HasPrefix(line, "DATABASE_URL") {
				parts := strings.SplitN(line, "=", 2)
				if len(parts) == 2 {
					val := strings.TrimSpace(parts[1])
					val = strings.Trim(val, `"'`)
					return val
				}
			}
		}
	}
	return "postgresql://postgres:postgres@127.0.0.1:5432/1Million?sslmode=disable"
}

func main() {
	dbURL := loadEnvURL()

	targetRows := 1000000
	batchSize := 50000

	ctx := context.Background()
	config, err := pgxpool.ParseConfig(dbURL)
	if err != nil {
		log.Fatalf("Config error: %v", err)
	}
	config.MaxConns = 10

	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		log.Fatalf("Failed to connect: %v", err)
	}
	defer pool.Close()

	if err := pool.Ping(ctx); err != nil {
		log.Fatalf("DB ping failed: %v", err)
	}

	log.Printf("Connected to PostgreSQL. Creating schema...")

	_, err = pool.Exec(ctx, `
		CREATE TABLE IF NOT EXISTS codes (
			id BIGINT PRIMARY KEY,
			code TEXT NOT NULL
		);
		TRUNCATE TABLE codes;
	`)
	if err != nil {
		log.Fatalf("Failed to create table: %v", err)
	}

	dummyBytes := make([]byte, 250)
	_, _ = rand.Read(dummyBytes)
	dummyHex := hex.EncodeToString(dummyBytes)

	log.Printf("Seeding %d records using binary COPY in %d batches...", targetRows, targetRows/batchSize)
	start := time.Now()

	totalInserted := int64(0)
	for batchStart := 1; batchStart <= targetRows; batchStart += batchSize {
		count := batchSize
		if batchStart+count-1 > targetRows {
			count = targetRows - batchStart + 1
		}

		src := &seedSource{
			total: count,
			idx:   0,
			hex:   dummyHex,
		}

		srcValues := func(base int64) *offsetSource {
			return &offsetSource{src: src, base: base}
		}

		n, err := pool.CopyFrom(
			ctx,
			pgx.Identifier{"codes"},
			[]string{"id", "code"},
			srcValues(int64(batchStart-1)),
		)
		if err != nil {
			log.Fatalf("COPY failed at batch %d: %v", batchStart, err)
		}
		totalInserted += n
		fmt.Printf("\rInserted: %d / %d records...", totalInserted, targetRows)
	}

	elapsed := time.Since(start)
	fmt.Printf("\nSUCCESS: Seeded %d records in %v (%.0f rows/sec)\n",
		totalInserted, elapsed, float64(totalInserted)/elapsed.Seconds())
}

type offsetSource struct {
	src  *seedSource
	base int64
}

func (o *offsetSource) Next() bool {
	return o.src.Next()
}

func (o *offsetSource) Values() ([]any, error) {
	v, err := o.src.Values()
	if err != nil {
		return nil, err
	}
	v[0] = o.base + int64(o.src.idx)
	return v, nil
}

func (o *offsetSource) Err() error {
	return o.src.Err()
}
