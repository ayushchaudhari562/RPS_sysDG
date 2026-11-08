package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"sync/atomic"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/valyala/fasthttp"
)

func invokeFastHandler(method, uri string, body []byte) *fasthttp.RequestCtx {
	var ctx fasthttp.RequestCtx
	ctx.Request.Header.SetMethod(method)
	ctx.Request.SetRequestURI(uri)
	if len(body) > 0 {
		ctx.Request.SetBody(body)
	}
	fastHandler(&ctx)
	return &ctx
}

func assertValidJSON(t *testing.T, data []byte) {
	t.Helper()
	var js any
	if err := json.Unmarshal(data, &js); err != nil {
		t.Fatalf("Expected valid JSON, got unmarshal error: %v. Raw body: %s", err, string(data))
	}
}

func TestEndpoints_ValidJSON(t *testing.T) {
	os.Setenv("ALLOW_NO_DB", "1")
	defer os.Unsetenv("ALLOW_NO_DB")

	dummyVal := `dummy_code_value`
	memoryStore[42].Store(&dummyVal)

	tests := []struct {
		name       string
		method     string
		uri        string
		body       []byte
		wantStatus int
	}{
		{
			name:       "/livez returns 200 and valid JSON",
			method:     "GET",
			uri:        "/livez",
			wantStatus: 200,
		},
		{
			name:       "/readyz returns valid JSON",
			method:     "GET",
			uri:        "/readyz",
			wantStatus: 503,
		},
		{
			name:       "/code-fast with existing ID returns 200 and valid JSON",
			method:     "GET",
			uri:        "/code-fast?id=42",
			wantStatus: 200,
		},
		{
			name:       "/code-fast with out-of-range ID returns 404 and valid JSON",
			method:     "GET",
			uri:        "/code-fast?id=10000000",
			wantStatus: 404,
		},
		{
			name:       "/code-fast with not found ID returns 404 and valid JSON",
			method:     "GET",
			uri:        "/code-fast?id=999999",
			wantStatus: 404,
		},
		{
			name:       "/code-write returns 202 and valid JSON",
			method:     "POST",
			uri:        "/code-write",
			body:       []byte("test code content"),
			wantStatus: 202,
		},
		{
			name:       "Unknown path returns 404 and valid JSON",
			method:     "GET",
			uri:        "/non-existent-path",
			wantStatus: 404,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ctx := invokeFastHandler(tc.method, tc.uri, tc.body)
			if ctx.Response.StatusCode() != tc.wantStatus {
				t.Errorf("got status %d, want %d (body: %s)", ctx.Response.StatusCode(), tc.wantStatus, string(ctx.Response.Body()))
			}
			assertValidJSON(t, ctx.Response.Body())
		})
	}
}

func TestPayloadVariations_RoundTrip(t *testing.T) {
	os.Setenv("ALLOW_NO_DB", "1")
	defer os.Unsetenv("ALLOW_NO_DB")

	payloads := []string{
		`Simple ASCII payload`,
		`He said, "Hello, World!" with quotes`,
		`C:\Users\Username\Documents\Path\With\Backslashes`,
		"Multi-line string\nSecond line\r\nThird line\tend",
		`Unicode:  Rocket, 100€ Euro, 日本語 Japanese, Español, Über, 1/2 ½`,
		`{"nested":"json","escaped":"\"quotes\"","flag":true}`,
	}

	for i, payload := range payloads {
		t.Run("Payload_"+strconv.Itoa(i), func(t *testing.T) {

			ctxWrite := invokeFastHandler("POST", "/code-write", []byte(payload))
			if ctxWrite.Response.StatusCode() != 202 {
				t.Fatalf("POST /code-write failed with status %d: %s", ctxWrite.Response.StatusCode(), string(ctxWrite.Response.Body()))
			}

			assertValidJSON(t, ctxWrite.Response.Body())

			var writeResp struct {
				Status string `json:"status"`
				ID     int64  `json:"id"`
			}
			if err := json.Unmarshal(ctxWrite.Response.Body(), &writeResp); err != nil {
				t.Fatalf("failed to unmarshal write response: %v", err)
			}
			if writeResp.ID <= 0 {
				t.Fatalf("expected positive ID, got %d", writeResp.ID)
			}

			ctxRead := invokeFastHandler("GET", "/code-fast?id="+strconv.FormatInt(writeResp.ID, 10), nil)
			if ctxRead.Response.StatusCode() != 200 {
				t.Fatalf("GET /code-fast failed with status %d: %s", ctxRead.Response.StatusCode(), string(ctxRead.Response.Body()))
			}

			assertValidJSON(t, ctxRead.Response.Body())

			var readResp struct {
				ID   int64  `json:"id"`
				Code string `json:"code"`
			}
			if err := json.Unmarshal(ctxRead.Response.Body(), &readResp); err != nil {
				t.Fatalf("failed to unmarshal read response: %v", err)
			}

			if readResp.ID != writeResp.ID {
				t.Errorf("ID mismatch: got %d, want %d", readResp.ID, writeResp.ID)
			}
			if readResp.Code != payload {
				t.Errorf("Payload mismatch!\nGot:  %q\nWant: %q", readResp.Code, payload)
			}
		})
	}
}

func TestBadInputs(t *testing.T) {
	os.Setenv("ALLOW_NO_DB", "1")
	defer os.Unsetenv("ALLOW_NO_DB")

	t.Run("NUL byte in body -> 400", func(t *testing.T) {
		ctx := invokeFastHandler("POST", "/code-write", []byte("hello\x00world"))
		if ctx.Response.StatusCode() != 400 {
			t.Errorf("got status %d, want 400", ctx.Response.StatusCode())
		}
		assertValidJSON(t, ctx.Response.Body())
	})

	t.Run("Invalid UTF-8 in body -> 400", func(t *testing.T) {
		ctx := invokeFastHandler("POST", "/code-write", []byte{0xff, 0xfe, 0xfd})
		if ctx.Response.StatusCode() != 400 {
			t.Errorf("got status %d, want 400", ctx.Response.StatusCode())
		}
		assertValidJSON(t, ctx.Response.Body())
	})

	t.Run("Body > 4096 bytes -> 413", func(t *testing.T) {
		largeBody := make([]byte, 4097)
		for i := range largeBody {
			largeBody[i] = 'a'
		}
		ctx := invokeFastHandler("POST", "/code-write", largeBody)
		if ctx.Response.StatusCode() != 413 {
			t.Errorf("got status %d, want 413", ctx.Response.StatusCode())
		}
		assertValidJSON(t, ctx.Response.Body())
	})

	invalidIDs := []string{"0", "-1", "-999", "abc", "", "1.5"}
	for _, idStr := range invalidIDs {
		t.Run("Invalid ID "+idStr+" -> 400", func(t *testing.T) {
			ctxFast := invokeFastHandler("GET", "/code-fast?id="+idStr, nil)
			if ctxFast.Response.StatusCode() != 400 {
				t.Errorf("/code-fast with id=%q: got status %d, want 400", idStr, ctxFast.Response.StatusCode())
			}
			assertValidJSON(t, ctxFast.Response.Body())

			ctxRedis := invokeFastHandler("GET", "/code-redis?id="+idStr, nil)
			if ctxRedis.Response.StatusCode() != 400 {
				t.Errorf("/code-redis with id=%q: got status %d, want 400", idStr, ctxRedis.Response.StatusCode())
			}
			assertValidJSON(t, ctxRedis.Response.Body())

			ctxDB := invokeFastHandler("GET", "/code-db?id="+idStr, nil)
			if ctxDB.Response.StatusCode() != 400 {
				t.Errorf("/code-db with id=%q: got status %d, want 400", idStr, ctxDB.Response.StatusCode())
			}
			assertValidJSON(t, ctxDB.Response.Body())
		})
	}
}

func TestDBPoolNil_WritesDisabled(t *testing.T) {

	os.Unsetenv("ALLOW_NO_DB")
	originalPool := dbPool
	dbPool = nil
	defer func() { dbPool = originalPool }()

	ctx := invokeFastHandler("POST", "/code-write", []byte("sample"))
	if ctx.Response.StatusCode() != 503 {
		t.Errorf("Expected 503 when dbPool is nil and ALLOW_NO_DB unset, got %d", ctx.Response.StatusCode())
	}
	assertValidJSON(t, ctx.Response.Body())
}

func TestPprofAndMetrics(t *testing.T) {
	mux := setupDebugMux()

	reqPprof := httptest.NewRequest("GET", "/debug/pprof/", nil)
	recPprof := httptest.NewRecorder()
	mux.ServeHTTP(recPprof, reqPprof)
	if recPprof.Code != http.StatusOK {
		t.Errorf("GET /debug/pprof/ returned %d, want 200", recPprof.Code)
	}

	reqMetrics := httptest.NewRequest("GET", "/metrics", nil)
	recMetrics := httptest.NewRecorder()
	mux.ServeHTTP(recMetrics, reqMetrics)
	if recMetrics.Code != http.StatusOK {
		t.Errorf("GET /metrics returned %d, want 200", recMetrics.Code)
	}
	assertValidJSON(t, recMetrics.Body.Bytes())

	var m MetricsResponse
	if err := json.Unmarshal(recMetrics.Body.Bytes(), &m); err != nil {
		t.Fatalf("failed to unmarshal /metrics response: %v", err)
	}
	if !m.InvariantHolds {
		t.Errorf("Metrics invariant holds is false: %+v", m)
	}
}

func TestAccountingInvariant(t *testing.T) {

drainLoop:
	for {
		select {
		case <-writeQueue:
		default:
			break drainLoop
		}
	}

	atomic.StoreInt64(&acceptedWrites, 0)
	atomic.StoreInt64(&rejectedFullWrites, 0)
	atomic.StoreInt64(&dequeuedWrites, 0)
	atomic.StoreInt64(&persistedWrites, 0)
	atomic.StoreInt64(&failedWrites, 0)
	atomic.StoreInt64(&pendingInBatches, 0)

	m0 := getMetrics()
	if !m0.InvariantHolds {
		t.Fatalf("Baseline invariant failed: %+v", m0)
	}

	for i := int64(1); i <= 10; i++ {
		writeQueue <- Item{ID: i, RawCode: "code", EscapedCode: "code"}
		atomic.AddInt64(&acceptedWrites, 1)
	}
	m1 := getMetrics()
	if !m1.InvariantHolds || m1.Accepted != 10 || m1.QueueDepth != 10 {
		t.Errorf("Step 1 invariant failed: %+v", m1)
	}

	for i := 0; i < 6; i++ {
		<-writeQueue
		atomic.AddInt64(&dequeuedWrites, 1)
		atomic.AddInt64(&pendingInBatches, 1)
	}
	m2 := getMetrics()
	if !m2.InvariantHolds || m2.PendingInBatches != 6 || m2.QueueDepth != 4 {
		t.Errorf("Step 2 invariant failed: %+v", m2)
	}

	atomic.AddInt64(&persistedWrites, 4)
	atomic.AddInt64(&pendingInBatches, -4)

	atomic.AddInt64(&failedWrites, 2)
	atomic.AddInt64(&pendingInBatches, -2)

	m3 := getMetrics()
	if !m3.InvariantHolds || m3.Persisted != 4 || m3.Failed != 2 || m3.PendingInBatches != 0 || m3.QueueDepth != 4 {
		t.Errorf("Step 3 invariant failed: %+v", m3)
	}

	for i := 0; i < 4; i++ {
		<-writeQueue
		atomic.AddInt64(&dequeuedWrites, 1)
		atomic.AddInt64(&pendingInBatches, 1)
	}
	atomic.AddInt64(&persistedWrites, 4)
	atomic.AddInt64(&pendingInBatches, -4)

	m4 := getMetrics()
	if !m4.InvariantHolds || m4.Persisted != 8 || m4.Failed != 2 || m4.QueueDepth != 0 || m4.PendingInBatches != 0 {
		t.Errorf("Step 4 invariant failed: %+v", m4)
	}
}

func TestErrorClassification_PermanentVsTransient(t *testing.T) {

	pgErr22 := &pgconn.PgError{Code: "22001"}
	if !isPermanentError(pgErr22) {
		t.Errorf("expected pgErr 22001 to be classified as permanent")
	}

	pgErr23 := &pgconn.PgError{Code: "23505"}
	if !isPermanentError(pgErr23) {
		t.Errorf("expected pgErr 23505 to be classified as permanent")
	}

	pgErr40 := &pgconn.PgError{Code: "40001"}
	if isPermanentError(pgErr40) {
		t.Errorf("expected pgErr 40001 to be classified as transient")
	}

	pgErr08 := &pgconn.PgError{Code: "08006"}
	if isPermanentError(pgErr08) {
		t.Errorf("expected pgErr 08006 to be classified as transient")
	}
}

func BenchmarkHandler_Simple(b *testing.B) {
	var ctx fasthttp.RequestCtx
	ctx.Request.Header.SetMethod("GET")
	ctx.Request.SetRequestURI("/simple")

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		fastHandler(&ctx)
	}
}

func BenchmarkHandler_CodeFast(b *testing.B) {
	val := `{"id":42,"code":"bench_test_payload"}`
	memoryStore[42].Store(&val)

	var ctx fasthttp.RequestCtx
	ctx.Request.Header.SetMethod("GET")
	ctx.Request.SetRequestURI("/code-fast?id=42")

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		fastHandler(&ctx)
	}
}

func BenchmarkHandler_CodeRedis_Hit(b *testing.B) {

	val := "mock_redis_cached_code"
	memoryStore[42].Store(&val)

	var ctx fasthttp.RequestCtx
	ctx.Request.Header.SetMethod("GET")
	ctx.Request.SetRequestURI("/code-redis?id=42")

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		fastHandler(&ctx)
	}
}

func BenchmarkHandler_CodeWrite_CustomBody(b *testing.B) {
	os.Setenv("ALLOW_NO_DB", "1")
	defer os.Unsetenv("ALLOW_NO_DB")

	stopDrain := make(chan struct{})
	go func() {
		for {
			select {
			case <-writeQueue:
			case <-stopDrain:
				return
			}
		}
	}()
	defer close(stopDrain)

	var ctx fasthttp.RequestCtx
	ctx.Request.Header.SetMethod("POST")
	ctx.Request.SetRequestURI("/code-write")
	body := []byte("bench write payload")
	ctx.Request.SetBody(body)

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		fastHandler(&ctx)
	}
}

func BenchmarkHandler_CodeWrite_EmptyBody(b *testing.B) {
	os.Setenv("ALLOW_NO_DB", "1")
	defer os.Unsetenv("ALLOW_NO_DB")

	stopDrain := make(chan struct{})
	go func() {
		for {
			select {
			case <-writeQueue:
			case <-stopDrain:
				return
			}
		}
	}()
	defer close(stopDrain)

	var ctx fasthttp.RequestCtx
	ctx.Request.Header.SetMethod("POST")
	ctx.Request.SetRequestURI("/code-write")

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		fastHandler(&ctx)
	}
}
