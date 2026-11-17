package main

// Write-behind pipeline:
//
//	POST /code-write -> writeQueue -> 4 batch workers -> PostgreSQL COPY
//	                                                  -> redisQueue -> Redis SET
//
// The client gets 202 as soon as the item is queued. A batch is flushed when
// it reaches batchSize items or every flushEvery, whichever comes first.

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/redis/rueidis"
)

const (
	queueSize  = 200_000 // ~1s of DB drain rate; beyond this we answer 503
	numWorkers = 4       // parallel COPY streams into PostgreSQL
	batchSize  = 5_000
	flushEvery = 100 * time.Millisecond
	maxRetries = 3
)

// Item is one accepted write.
type Item struct {
	ID      int64
	Raw     string  // original text, stored in PostgreSQL
	Escaped string  // JSON-escaped text, stored in L1 and Redis
	memPtr  *string // the pointer put in L1, so a failed write can be evicted
}

var (
	writeQueue  = make(chan Item, queueSize)
	stopWorkers = make(chan struct{})

	redisQueue = make(chan []Item, 100)
	stopRedis  = make(chan struct{})
	redisDone  = make(chan struct{})

	deadLetterMu   sync.Mutex
	deadLetterFile *os.File
)

func startWorkers(wg *sync.WaitGroup) {
	for range numWorkers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			runWorker()
		}()
	}
}

// runWorker collects items into a batch and flushes it on size or time.
// On stopWorkers it drains whatever is left in the queue, flushes, and exits.
func runWorker() {
	batch := make([]Item, 0, batchSize)
	ticker := time.NewTicker(flushEvery)
	defer ticker.Stop()

	flushNow := func() {
		if len(batch) > 0 {
			flush(batch)
			batch = batch[:0] // reuse the array
		}
	}
	add := func(it Item) {
		stats.pending.Add(1)
		batch = append(batch, it)
		if len(batch) == batchSize {
			flushNow()
		}
	}

	for {
		select {
		case it := <-writeQueue:
			add(it)
		case <-ticker.C:
			flushNow()
		case <-stopWorkers:
			for {
				select {
				case it := <-writeQueue:
					add(it)
				default: // queue empty
					flushNow()
					return
				}
			}
		}
	}
}

// flush writes a batch to PostgreSQL.
//   - transient error (network, timeout): retry with backoff 50/100/200ms
//   - permanent error (bad row): split the batch in half and retry each half,
//     until the single bad row is isolated and sent to dead_letter.jsonl
func flush(batch []Item) {
	if dbPool == nil {
		markFailed(batch)
		return
	}
	var err error
	for attempt := range maxRetries {
		if err = copyToDB(batch); err == nil {
			n := int64(len(batch))
			stats.persisted.Add(n)
			stats.pending.Add(-n)
			sendToRedis(batch)
			return
		}
		if isPermanent(err) {
			break
		}
		time.Sleep((50 * time.Millisecond) << attempt)
	}

	switch {
	case !isPermanent(err):
		log.Printf("ERROR: %d writes dropped after %d retries: %v", len(batch), maxRetries, err)
		markFailed(batch)
	case len(batch) == 1:
		writeDeadLetter(batch[0], err)
		markFailed(batch)
	default:
		mid := len(batch) / 2
		flush(batch[:mid])
		flush(batch[mid:])
	}
}

// copyToDB streams the batch with PostgreSQL's binary COPY protocol, which is
// much faster than INSERTs. The row slice is reused: pgx encodes each row
// before asking for the next one.
func copyToDB(batch []Item) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	row := make([]any, 2)
	_, err := dbPool.CopyFrom(ctx, pgx.Identifier{"codes"}, []string{"id", "code"},
		pgx.CopyFromSlice(len(batch), func(i int) ([]any, error) {
			row[0], row[1] = batch[i].ID, batch[i].Raw
			return row, nil
		}))
	return err
}

// isPermanent reports errors that retrying cannot fix:
// SQLSTATE class 22 (bad data, e.g. NUL byte) and 23 (constraint, e.g. duplicate key).
func isPermanent(err error) bool {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && len(pgErr.Code) >= 2 {
		class := pgErr.Code[:2]
		return class == "22" || class == "23"
	}
	return false
}

// markFailed counts items as failed and removes them from L1, so readers
// never see data that was not saved. CompareAndSwap only clears the slot if
// it still holds this write's value.
func markFailed(items []Item) {
	for _, it := range items {
		if it.memPtr != nil {
			memoryStore[it.ID].CompareAndSwap(it.memPtr, nil)
		}
	}
	n := int64(len(items))
	stats.failed.Add(n)
	stats.pending.Add(-n)
}

// ---- Redis write-through ----

// sendToRedis queues a persisted batch for Redis without blocking the worker.
// The worker reuses its batch array, so we send a copy.
func sendToRedis(batch []Item) {
	if redisClient == nil {
		return
	}
	select {
	case redisQueue <- append([]Item(nil), batch...):
	default: // Redis is behind; it is only a cache, so drop instead of waiting
		stats.redisErrors.Add(int64(len(batch)))
	}
}

// runRedisWriter is the single goroutine that pipelines batches into Redis.
// On stopRedis it drains what is already queued, then closes redisDone.
func runRedisWriter() {
	defer close(redisDone)
	for {
		select {
		case batch := <-redisQueue:
			writeToRedis(batch)
		case <-stopRedis:
			for {
				select {
				case batch := <-redisQueue:
					writeToRedis(batch)
				default:
					return
				}
			}
		}
	}
}

func writeToRedis(items []Item) {
	if redisClient == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	cmds := make(rueidis.Commands, 0, len(items))
	for _, it := range items {
		key := "code:" + strconv.FormatInt(it.ID, 10)
		cmds = append(cmds, redisClient.B().Set().Key(key).Value(it.Escaped).Ex(10*time.Minute).Build())
	}
	for _, resp := range redisClient.DoMulti(ctx, cmds...) { // one round trip for the whole batch
		if resp.Error() != nil {
			stats.redisErrors.Add(1)
		}
	}
}

// ---- dead letter file (one JSON line per row PostgreSQL rejected) ----

func openDeadLetter() {
	f, err := os.OpenFile("dead_letter.jsonl", os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		log.Printf("WARN: cannot open dead_letter.jsonl: %v", err)
		return
	}
	deadLetterFile = f
}

func closeDeadLetter() {
	deadLetterMu.Lock()
	defer deadLetterMu.Unlock()
	if deadLetterFile != nil {
		_ = deadLetterFile.Close()
		deadLetterFile = nil
	}
}

func writeDeadLetter(it Item, err error) {
	line, _ := json.Marshal(map[string]any{
		"id": it.ID, "code": it.Raw, "error": err.Error(), "timestamp": time.Now().UTC(),
	})
	deadLetterMu.Lock()
	defer deadLetterMu.Unlock()
	if deadLetterFile != nil {
		_, _ = deadLetterFile.Write(append(line, '\n'))
	}
}
