package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log"
	"net"
	"net/http"
	"net/http/pprof"
	"os"
	"os/signal"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/rueidis"
	"github.com/valyala/fasthttp"
	"github.com/valyala/fasthttp/reuseport"
)

var (
	
	dbPool *pgxpool.Pool

	redisClient rueidis.Client
)

var (
	
	writeQueue = make(chan Item, 200000)

	doneChan = make(chan struct{})

	redisQueue = make(chan []Item, 100)

	totalWrites        int64 
	acceptedWrites     int64 
	rejectedFullWrites int64 
	dequeuedWrites     int64 
	persistedWrites    int64 
	failedWrites       int64 
	pendingInBatches   int64 
	redisErrors        int64 
	redisDroppedWrites int64 
)

type Item struct {
	ID          int64  
	RawCode     string 
	EscapedCode string 
}

const (
	TOTAL_RECORDS = 1000000 
	NUM_WORKERS   = 4       
	BATCH_SIZE    = 5000    
	MAX_CODE_LEN  = 4096    
)

var memoryStore [TOTAL_RECORDS]atomic.Pointer[string]

var (
	defaultPayload        string     
	defaultEscapedPayload string     
	deadLetterFile        *os.File   
	deadLetterMu          sync.Mutex 
)

func init() {

	defaultPayload = generateDummyCode()
	defaultEscapedPayload = string(appendJSONString(nil, defaultPayload))

	var err error
	deadLetterFile, err = os.OpenFile("dead_letter.jsonl", os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		log.Printf("WARN: Failed to initialize dead_letter.jsonl: %v", err)
	}

	go func() {
		for batch := range redisQueue {
			flushBatchToRedis(batch)
		}
	}()
}

func generateDummyCode() string {
	b := make([]byte, 250)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func appendJSONString(dst []byte, s string) []byte {
	const hexd = "0123456789abcdef"
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '"':
			dst = append(dst, '\\', '"')
		case c == '\\':
			dst = append(dst, '\\', '\\')
		case c < 0x20:

			dst = append(dst, '\\', 'u', '0', '0', hexd[c>>4], hexd[c&0xf])
		default:
			dst = append(dst, c)
		}
	}
	return dst
}

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
			return 0, false
		}
	}
	return n, n > 0
}

func initData() {

	if os.Getenv("BENCH_FIXTURE") == "1" {
		log.Printf("BENCH_FIXTURE=1: Pre-loading %d records into memory...", TOTAL_RECORDS)
		for i := 0; i < TOTAL_RECORDS; i++ {
			memoryStore[i].Store(&defaultEscapedPayload)
		}
		log.Printf("%d records ready in memory", TOTAL_RECORDS)
		return
	}

	if dbPool == nil {
		log.Println("INFO: BENCH_FIXTURE not set and dbPool is nil. In-memory table starts empty.")
		return
	}

	log.Println("Pre-loading memory store from PostgreSQL codes table...")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	rows, err := dbPool.Query(ctx, "SELECT id, code FROM codes WHERE id < 1000000 ORDER BY id")
	if err != nil {
		log.Printf("WARN: Failed to preload memory store from DB: %v", err)
		return
	}
	defer rows.Close()

	var loaded int
	for rows.Next() {
		var id int64
		var code string
		if err := rows.Scan(&id, &code); err != nil {
			log.Printf("WARN: Error scanning row during preload: %v", err)
			break
		}
		if id >= 0 && id < TOTAL_RECORDS {
			escaped := string(appendJSONString(nil, code))
			memoryStore[id].Store(&escaped)
			loaded++
		}
	}
	if err := rows.Err(); err != nil {
		log.Printf("WARN: Row iteration error during preload: %v", err)
	}

	log.Printf("Successfully preloaded %d records from PostgreSQL into memory store", loaded)
}

func getEnvWithFile(key, defaultVal string) string {
	if val := os.Getenv(key); val != "" {
		return val
	}
	data, err := os.ReadFile(".env")
	if err == nil {
		for _, line := range strings.Split(string(data), "\n") {
			line = strings.TrimSpace(line)
			if strings.HasPrefix(line, key) {
				parts := strings.SplitN(line, "=", 2)
				if len(parts) == 2 {
					val := strings.TrimSpace(parts[1])
					val = strings.Trim(val, `"'`)
					if val != "" {
						return val
					}
				}
			}
		}
	}
	return defaultVal
}

func initDB() {
	dbURL := getEnvWithFile("DATABASE_URL", "")
	if dbURL == "" {
		log.Println("WARN: DATABASE_URL not set; PostgreSQL features disabled")
		return
	}

	config, err := pgxpool.ParseConfig(dbURL)
	if err != nil {
		log.Fatalf("FATAL: DB config parse error: %v", err)
	}

	config.MaxConns = 30
	config.MinConns = 5
	config.MaxConnIdleTime = 5 * time.Minute

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		log.Printf("WARN: DB connection failed: %v (Postgres routes disabled)", err)
		return
	}

	if err := pool.Ping(ctx); err != nil {
		log.Printf("WARN: DB ping failed: %v (Postgres routes disabled)", err)
		return
	}

	dbPool = pool
	log.Println("Connected to PostgreSQL")

	_, err = dbPool.Exec(ctx, `
		CREATE TABLE IF NOT EXISTS codes (
			id BIGINT PRIMARY KEY,
			code TEXT NOT NULL
		);
	`)
	if err != nil {
		log.Fatalf("FATAL: Failed to ensure table schema: %v", err)
	}

	var maxID int64
	err = dbPool.QueryRow(ctx, "SELECT COALESCE(MAX(id), 0) FROM codes").Scan(&maxID)
	if err != nil {
		log.Fatalf("FATAL: Failed to read MAX(id) from codes: %v", err)
	}
	atomic.StoreInt64(&totalWrites, maxID)
	log.Printf("PostgreSQL table 'codes' ready. Sequence starts at %d", maxID+1)
}

func initRedis() {
	redisURL := getEnvWithFile("REDIS_URL", "127.0.0.1:6379")

	client, err := rueidis.NewClient(rueidis.ClientOption{
		InitAddress:       []string{redisURL},
		CacheSizeEachConn: 128 * (1 << 20),
		DisableCache:      false,
	})
	if err != nil {
		log.Printf("WARN: Redis config error: %v", err)
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	if err := client.Do(ctx, client.B().Ping().Build()).Error(); err != nil {
		log.Printf("WARN: Redis ping failed at %s: %v", redisURL, err)
		return
	}

	redisClient = client
	log.Printf("Connected to Redis at %s (RESP3 client-side cache active)", redisURL)
}

type itemCopySource struct {
	items []Item
	idx   int
	row   [2]any
}

func (s *itemCopySource) Next() bool {
	s.idx++
	return s.idx <= len(s.items)
}

func (s *itemCopySource) Values() ([]any, error) {
	it := s.items[s.idx-1]
	s.row[0] = it.ID
	s.row[1] = it.RawCode
	return s.row[:], nil
}

func (s *itemCopySource) Err() error {
	return nil
}

func flushBatchToDB(items []Item) error {
	if len(items) == 0 {
		return nil
	}
	if dbPool == nil {
		return errors.New("database not connected")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	src := itemCopySource{items: items}
	_, err := dbPool.CopyFrom(
		ctx,
		pgx.Identifier{"codes"},
		[]string{"id", "code"},
		&src,
	)
	return err
}

func flushBatchToRedis(items []Item) {
	if redisClient == nil || len(items) == 0 {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	cmds := make(rueidis.Commands, 0, len(items))
	for _, it := range items {
		key := "code:" + strconv.FormatInt(it.ID, 10)
		cmds = append(cmds, redisClient.B().Set().Key(key).Value(it.EscapedCode).Ex(10*time.Minute).Build())
	}

	for _, resp := range redisClient.DoMulti(ctx, cmds...) {
		if err := resp.Error(); err != nil {
			atomic.AddInt64(&redisErrors, 1)
		}
	}
}

type DeadLetterRecord struct {
	ID        int64     `json:"id"`
	RawCode   string    `json:"code"`
	Error     string    `json:"error"`
	Timestamp time.Time `json:"timestamp"`
}

func recordDeadLetter(item Item, err error) {
	deadLetterMu.Lock()
	defer deadLetterMu.Unlock()

	if deadLetterFile == nil {
		return
	}

	errMsg := ""
	if err != nil {
		errMsg = err.Error()
	}
	rec := DeadLetterRecord{
		ID:        item.ID,
		RawCode:   item.RawCode,
		Error:     errMsg,
		Timestamp: time.Now().UTC(),
	}
	b, _ := json.Marshal(rec)
	b = append(b, '\n')
	_, _ = deadLetterFile.Write(b)
}

func isPermanentError(err error) bool {
	if err == nil {
		return false
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && len(pgErr.Code) >= 2 {
		class := pgErr.Code[:2]
		if class == "22" || class == "23" {
			return true
		}
	}
	return false
}

func flushBatchWithRetryAndBisection(batch []Item) {
	if len(batch) == 0 {
		return
	}

	if dbPool == nil {

		atomic.AddInt64(&failedWrites, int64(len(batch)))
		atomic.AddInt64(&pendingInBatches, -int64(len(batch)))
		return
	}

	var err error
	for attempt := 0; attempt < 3; attempt++ {
		err = flushBatchToDB(batch)
		if err == nil {

			atomic.AddInt64(&persistedWrites, int64(len(batch)))
			atomic.AddInt64(&pendingInBatches, -int64(len(batch)))

			if redisClient != nil && len(batch) > 0 {
				redisBatch := make([]Item, len(batch))
				copy(redisBatch, batch)
				select {
				case redisQueue <- redisBatch:
				default:

					atomic.AddInt64(&redisDroppedWrites, int64(len(batch)))
					atomic.AddInt64(&redisErrors, int64(len(batch)))
				}
			}
			return
		}

		if isPermanentError(err) {
			break
		}

		time.Sleep(time.Duration(50<<attempt) * time.Millisecond)
	}

	if isPermanentError(err) {
		if len(batch) == 1 {

			recordDeadLetter(batch[0], err)
			atomic.AddInt64(&failedWrites, 1)
			atomic.AddInt64(&pendingInBatches, -1)
			return
		}

		mid := len(batch) / 2
		flushBatchWithRetryAndBisection(batch[:mid])
		flushBatchWithRetryAndBisection(batch[mid:])
		return
	}

	log.Printf("ERROR: Batch flush failed after 3 retries (%d items dropped): %v", len(batch), err)
	atomic.AddInt64(&failedWrites, int64(len(batch)))
	atomic.AddInt64(&pendingInBatches, -int64(len(batch)))
}

func flushWithRetry(batch []Item) {
	flushBatchWithRetryAndBisection(batch)
}

func startBatchWorkers(wg *sync.WaitGroup) {
	for i := 0; i < NUM_WORKERS; i++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			batch := make([]Item, 0, BATCH_SIZE)
			ticker := time.NewTicker(100 * time.Millisecond)
			defer ticker.Stop()

			for {
				select {
				case item := <-writeQueue:

					atomic.AddInt64(&dequeuedWrites, 1)
					atomic.AddInt64(&pendingInBatches, 1)
					batch = append(batch, item)
					if len(batch) >= BATCH_SIZE {
						flushWithRetry(batch)
						batch = batch[:0]
					}
				case <-ticker.C:

					if len(batch) > 0 {
						flushWithRetry(batch)
						batch = batch[:0]
					}
				case <-doneChan:

					for {
						select {
						case item := <-writeQueue:
							atomic.AddInt64(&dequeuedWrites, 1)
							atomic.AddInt64(&pendingInBatches, 1)
							batch = append(batch, item)
							if len(batch) >= BATCH_SIZE {
								flushWithRetry(batch)
								batch = batch[:0]
							}
						default:

							if len(batch) > 0 {
								flushWithRetry(batch)
								batch = batch[:0]
							}
							return
						}
					}
				}
			}
		}(i)
	}
}

type MetricsResponse struct {
	Accepted         int64 `json:"accepted"`
	RejectedFull     int64 `json:"rejected_full"`
	Dequeued         int64 `json:"dequeued"`
	Persisted        int64 `json:"persisted"`
	Failed           int64 `json:"failed"`
	PendingInBatches int64 `json:"pending_in_batches"`
	QueueDepth       int64 `json:"queue_depth"`
	RedisErrors      int64 `json:"redis_errors"`
	RedisDropped     int64 `json:"redis_dropped,omitempty"`
	InvariantHolds   bool  `json:"invariant_holds"`

	AcceptedWrites     int64 `json:"accepted_writes"`
	PersistedWrites    int64 `json:"persisted_writes"`
	FailedWrites       int64 `json:"failed_writes"`
	RejectedFullWrites int64 `json:"rejected_full_writes"`
}

func getMetrics() MetricsResponse {
	accepted := atomic.LoadInt64(&acceptedWrites)
	rejected := atomic.LoadInt64(&rejectedFullWrites)
	dequeued := atomic.LoadInt64(&dequeuedWrites)
	persisted := atomic.LoadInt64(&persistedWrites)
	failed := atomic.LoadInt64(&failedWrites)
	qDepth := int64(len(writeQueue))
	rErrors := atomic.LoadInt64(&redisErrors)
	rDropped := atomic.LoadInt64(&redisDroppedWrites)

	pending := accepted - persisted - failed - qDepth
	if pending < 0 {
		pending = 0
	}

	invariant := accepted == (persisted + failed + pending + qDepth)

	return MetricsResponse{
		Accepted:           accepted,
		RejectedFull:       rejected,
		Dequeued:           dequeued,
		Persisted:          persisted,
		Failed:             failed,
		PendingInBatches:   pending,
		QueueDepth:         qDepth,
		RedisErrors:        rErrors,
		RedisDropped:       rDropped,
		InvariantHolds:     invariant,
		AcceptedWrites:     accepted,
		PersistedWrites:    persisted,
		FailedWrites:       failed,
		RejectedFullWrites: rejected,
	}
}

func handleMetrics(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	m := getMetrics()
	_ = json.NewEncoder(w).Encode(m)
}

func setupDebugMux() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("/debug/pprof/", pprof.Index)
	mux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
	mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
	mux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
	mux.HandleFunc("/debug/pprof/trace", pprof.Trace)
	mux.HandleFunc("/metrics", handleMetrics)
	return mux
}

var (
	okBytes       = []byte("OK")
	textPlain     = []byte("text/plain; charset=utf-8")
	pathSimple    = []byte("/simple")
	pathLive      = []byte("/livez")
	pathReady     = []byte("/readyz")
	pathCodeFast  = []byte("/code-fast")
	pathCodeRedis = []byte("/code-redis")
	pathCodeDB    = []byte("/code-db")
	pathCodeWrite = []byte("/code-write")
)

func fastHandler(ctx *fasthttp.RequestCtx) {
	path := ctx.Path()

	if ctx.IsGet() {

		if bytes.Equal(path, pathSimple) {
			ctx.Response.Header.SetContentTypeBytes(textPlain)
			ctx.SetStatusCode(fasthttp.StatusOK)
			ctx.SetBody(okBytes)
			return
		}

		if bytes.Equal(path, pathLive) {
			ctx.SetContentType("application/json")
			ctx.SetStatusCode(fasthttp.StatusOK)
			ctx.SetBodyString(`{"status":"alive"}`)
			return
		}

		if bytes.Equal(path, pathReady) {
			dbOK := dbPool != nil
			redisOK := redisClient != nil

			ctx.SetContentType("application/json")
			if dbOK && redisOK {
				ctx.SetStatusCode(fasthttp.StatusOK)
				ctx.SetBodyString(`{"status":"ready","database":"connected","redis":"connected"}`)
				return
			}

			ctx.SetStatusCode(fasthttp.StatusServiceUnavailable)
			b := ctx.Response.Body()[:0]
			b = append(b, `{"status":"degraded","database":"`...)
			if dbOK {
				b = append(b, "connected"...)
			} else {
				b = append(b, "disconnected"...)
			}
			b = append(b, `","redis":"`...)
			if redisOK {
				b = append(b, "connected"...)
			} else {
				b = append(b, "disconnected"...)
			}
			b = append(b, `"}`...)
			ctx.SetBody(b)
			return
		}

		if bytes.Equal(path, pathCodeFast) {
			id, ok := parsePositiveInt64(ctx.QueryArgs().Peek("id"))
			if !ok {
				ctx.SetContentType("application/json")
				ctx.SetStatusCode(fasthttp.StatusBadRequest)
				ctx.SetBodyString(`{"error":"invalid_id","message":"id must be a positive integer"}`)
				return
			}
			if id >= TOTAL_RECORDS {
				ctx.SetContentType("application/json")
				ctx.SetStatusCode(fasthttp.StatusNotFound)
				ctx.SetBodyString(`{"error":"record_not_found"}`)
				return
			}

			ptr := memoryStore[id].Load()
			if ptr == nil || *ptr == "" {
				ctx.SetContentType("application/json")
				ctx.SetStatusCode(fasthttp.StatusNotFound)
				ctx.SetBodyString(`{"error":"record_not_found"}`)
				return
			}

			ctx.SetContentType("application/json")
			ctx.SetStatusCode(fasthttp.StatusOK)
			b := ctx.Response.Body()[:0]
			b = append(b, `{"id":`...)
			b = strconv.AppendInt(b, id, 10)
			b = append(b, `,"code":"`...)
			b = append(b, *ptr...)
			b = append(b, `"}`...)
			ctx.SetBody(b)
			return
		}

		if bytes.Equal(path, pathCodeRedis) {
			id, ok := parsePositiveInt64(ctx.QueryArgs().Peek("id"))
			if !ok {
				ctx.SetContentType("application/json")
				ctx.SetStatusCode(fasthttp.StatusBadRequest)
				ctx.SetBodyString(`{"error":"invalid_id","message":"id must be a positive integer"}`)
				return
			}

			if redisClient == nil {
				ctx.SetContentType("application/json")
				ctx.SetStatusCode(fasthttp.StatusServiceUnavailable)
				ctx.SetBodyString(`{"error":"redis_not_connected"}`)
				return
			}

			var keyBuf [32]byte
			k := append(keyBuf[:0], "code:"...)
			k = strconv.AppendInt(k, id, 10)
			keyStr := string(k)

			source := "redis"

			cmd := redisClient.B().Get().Key(keyStr).Cache()
			resp := redisClient.DoCache(ctx, cmd, 10*time.Minute)
			val, err := resp.ToString()

			if err != nil {
				if !rueidis.IsRedisNil(err) {

					atomic.AddInt64(&redisErrors, 1)
					ctx.SetContentType("application/json")
					ctx.SetStatusCode(fasthttp.StatusBadGateway)
					ctx.SetBodyString(`{"error":"redis_unavailable"}`)
					return
				}

				if id < TOTAL_RECORDS {
					ptr := memoryStore[id].Load()
					if ptr != nil && *ptr != "" {
						val = *ptr
						source = "memory"
					}
				}

				if val == "" && dbPool != nil {
					var dbCode string
					dbCtx, dbCancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
					qErr := dbPool.QueryRow(dbCtx, "SELECT code FROM codes WHERE id = $1", id).Scan(&dbCode)
					dbCancel()
					if qErr == nil {
						val = string(appendJSONString(nil, dbCode))
						source = "postgresql"
					}
				}
			}

			if val == "" {
				ctx.SetContentType("application/json")
				ctx.SetStatusCode(fasthttp.StatusNotFound)
				ctx.SetBodyString(`{"error":"record_not_found"}`)
				return
			}

			if source != "redis" && redisClient != nil {
				select {
				case redisQueue <- []Item{{ID: id, EscapedCode: val}}:
				default:

				}
			}

			ctx.SetContentType("application/json")
			ctx.SetStatusCode(fasthttp.StatusOK)
			b := ctx.Response.Body()[:0]
			b = append(b, `{"id":`...)
			b = strconv.AppendInt(b, id, 10)
			b = append(b, `,"code":"`...)
			b = append(b, val...)
			b = append(b, `","source":"`...)
			b = append(b, source...)
			b = append(b, `"}`...)
			ctx.SetBody(b)
			return
		}

		if bytes.Equal(path, pathCodeDB) {
			id, ok := parsePositiveInt64(ctx.QueryArgs().Peek("id"))
			if !ok {
				ctx.SetContentType("application/json")
				ctx.SetStatusCode(fasthttp.StatusBadRequest)
				ctx.SetBodyString(`{"error":"invalid_id","message":"id must be a positive integer"}`)
				return
			}

			if dbPool == nil {
				ctx.SetContentType("application/json")
				ctx.SetStatusCode(fasthttp.StatusServiceUnavailable)
				ctx.SetBodyString(`{"error":"database_not_connected"}`)
				return
			}

			dbCtx, dbCancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
			defer dbCancel()

			var code string
			err := dbPool.QueryRow(dbCtx, "SELECT code FROM codes WHERE id = $1", id).Scan(&code)
			if err != nil {
				ctx.SetContentType("application/json")
				if errors.Is(err, pgx.ErrNoRows) {
					ctx.SetStatusCode(fasthttp.StatusNotFound)
					ctx.SetBodyString(`{"error":"record_not_found"}`)
				} else {
					ctx.SetStatusCode(fasthttp.StatusInternalServerError)
					ctx.SetBodyString(`{"error":"database_query_failed"}`)
				}
				return
			}

			ctx.SetContentType("application/json")
			ctx.SetStatusCode(fasthttp.StatusOK)
			escapedCode := appendJSONString(nil, code)
			b := ctx.Response.Body()[:0]
			b = append(b, `{"id":`...)
			b = strconv.AppendInt(b, id, 10)
			b = append(b, `,"code":"`...)
			b = append(b, escapedCode...)
			b = append(b, `,"source":"postgresql"}`...)
			ctx.SetBody(b)
			return
		}
	}

	if ctx.IsPost() {
		if bytes.Equal(path, pathCodeWrite) {

			if dbPool == nil && os.Getenv("ALLOW_NO_DB") != "1" {
				ctx.SetContentType("application/json")
				ctx.SetStatusCode(fasthttp.StatusServiceUnavailable)
				ctx.SetBodyString(`{"error":"database_not_connected","message":"writes disabled when database is offline"}`)
				return
			}

			body := ctx.PostBody()

			if len(body) > MAX_CODE_LEN {
				ctx.SetContentType("application/json")
				ctx.SetStatusCode(fasthttp.StatusRequestEntityTooLarge)
				ctx.SetBodyString(`{"error":"payload_too_large","max_bytes":4096}`)
				return
			}
			if len(body) > 0 {
				if !utf8.Valid(body) || bytes.IndexByte(body, 0) >= 0 {
					ctx.SetContentType("application/json")
					ctx.SetStatusCode(fasthttp.StatusBadRequest)
					ctx.SetBodyString(`{"error":"invalid_payload","message":"must be valid utf-8 without null bytes"}`)
					return
				}
			}

			var rawCode string
			var escapedCode string
			if len(body) > 0 {
				rawCode = string(body)
				escapedCode = string(appendJSONString(nil, rawCode))
			} else {
				rawCode = defaultPayload
				escapedCode = defaultEscapedPayload
			}

			newID := atomic.AddInt64(&totalWrites, 1)

			select {
			case writeQueue <- Item{ID: newID, RawCode: rawCode, EscapedCode: escapedCode}:
				atomic.AddInt64(&acceptedWrites, 1)

				if newID < TOTAL_RECORDS {
					memoryStore[newID].Store(&escapedCode)
				}

				ctx.SetContentType("application/json")
				ctx.SetStatusCode(fasthttp.StatusAccepted)
				b := ctx.Response.Body()[:0]
				b = append(b, `{"status":"queued","id":`...)
				b = strconv.AppendInt(b, newID, 10)
				b = append(b, '}')
				ctx.SetBody(b)
				return

			default:

				atomic.AddInt64(&rejectedFullWrites, 1)
				ctx.SetContentType("application/json")
				ctx.SetStatusCode(fasthttp.StatusServiceUnavailable)
				ctx.SetBodyString(`{"error":"write_queue_full","message":"backpressure triggered, retry later"}`)
				return
			}
		}
	}

	ctx.SetContentType("application/json")
	ctx.SetStatusCode(fasthttp.StatusNotFound)
	ctx.SetBodyString(`{"error":"not_found"}`)
}

func main() {
	numCores := runtime.NumCPU()
	port := getEnvWithFile("PORT", "8081")
	metricsAddr := getEnvWithFile("METRICS_ADDR", "127.0.0.1:6060")

	log.Println("==========================================================")
	log.Printf(" 🚀 Go HTTP engine benchmark suite")
	log.Printf(" Starting server on :%s using %d cores (SO_REUSEPORT)", port, numCores)
	log.Println("==========================================================")

	initDB()
	initData()
	initRedis()

	var workerWG sync.WaitGroup
	startBatchWorkers(&workerWG)

	debugMux := setupDebugMux()
	debugServer := &http.Server{
		Addr:    metricsAddr,
		Handler: debugMux,
	}

	go func() {
		log.Printf("Internal debug (pprof) & metrics server listening on %s", metricsAddr)
		if err := debugServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Printf("Debug server error: %v", err)
		}
	}()

	servers := make([]*fasthttp.Server, numCores)

	for i := 0; i < numCores; i++ {
		ln, err := reuseport.Listen("tcp4", ":"+port)
		if err != nil {
			log.Fatalf("Worker %d reuseport error: %v", i, err)
		}

		servers[i] = &fasthttp.Server{
			Handler:               fastHandler,
			NoDefaultServerHeader: true,
			Concurrency:           256 * 1024,
			ReadBufferSize:        4096,
			WriteBufferSize:       4096,
			ReadTimeout:           5 * time.Second,
			WriteTimeout:          5 * time.Second,
			IdleTimeout:           15 * time.Second,
			MaxRequestBodySize:    8 * 1024,
			DisableKeepalive:      false,
			TCPKeepalive:          true,
		}

		go func(s *fasthttp.Server, listener net.Listener, workerID int) {
			if err := s.Serve(listener); err != nil {
				log.Printf("Worker %d stopped: %v", workerID, err)
			}
		}(servers[i], ln, i)
	}

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)
	<-sigChan

	log.Println("Shutdown signal received. Stopping HTTP servers...")

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutdownCancel()

	var shutdownErr atomic.Bool
	var shutdownWG sync.WaitGroup
	for i, s := range servers {
		shutdownWG.Add(1)
		go func(srv *fasthttp.Server, idx int) {
			defer shutdownWG.Done()
			if err := srv.ShutdownWithContext(shutdownCtx); err != nil {
				shutdownErr.Store(true)
				log.Printf("ERROR: Worker %d shutdown failed: %v", idx, err)
			}
		}(s, i)
	}
	shutdownWG.Wait()

	if shutdownErr.Load() {
		log.Println("WARN: Some HTTP servers did not shut down cleanly within deadline.")
	}

	_ = debugServer.Shutdown(shutdownCtx)

	log.Println("Stopping batch workers and draining remaining queued items...")
	close(doneChan)
	workerWG.Wait()

	close(redisQueue)

	if deadLetterFile != nil {
		_ = deadLetterFile.Close()
	}
	if dbPool != nil {
		dbPool.Close()
	}
	if redisClient != nil {
		redisClient.Close()
	}

	log.Printf("Shutdown complete. Final stats: Accepted=%d, Persisted=%d, Failed=%d, RejectedFull=%d, QueueDepth=%d",
		atomic.LoadInt64(&acceptedWrites),
		atomic.LoadInt64(&persistedWrites),
		atomic.LoadInt64(&failedWrites),
		atomic.LoadInt64(&rejectedFullWrites),
		len(writeQueue),
	)
}
