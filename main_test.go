package main

import (
	"encoding/json"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/valyala/fasthttp"
)

// call runs the handler directly, without a network socket.
func call(method, uri string, body []byte) *fasthttp.RequestCtx {
	var ctx fasthttp.RequestCtx
	ctx.Request.Header.SetMethod(method)
	ctx.Request.SetRequestURI(uri)
	ctx.Request.SetBody(body)
	handler(&ctx)
	return &ctx
}

func mustJSON(t *testing.T, b []byte, v any) {
	t.Helper()
	if v == nil {
		v = new(any)
	}
	if err := json.Unmarshal(b, v); err != nil {
		t.Fatalf("invalid JSON %q: %v", b, err)
	}
}

func drainQueue() {
	for {
		select {
		case <-writeQueue:
		default:
			return
		}
	}
}

func resetStats() {
	drainQueue()
	for _, c := range []interface{ Store(int64) }{
		&stats.accepted, &stats.rejected, &stats.persisted, &stats.failed, &stats.pending, &stats.redisErrors,
	} {
		c.Store(0)
	}
}

func TestRoutes(t *testing.T) {
	t.Setenv("ALLOW_NO_DB", "1")
	v := "dummy"
	memoryStore[42].Store(&v)

	tests := []struct {
		method, uri string
		body        string
		want        int
	}{
		{"GET", "/simple", "", 200},
		{"GET", "/livez", "", 200},
		{"GET", "/readyz", "", 503}, // no DB/Redis in unit tests
		{"GET", "/code-fast?id=42", "", 200},
		{"GET", "/code-fast?id=999999", "", 404},
		{"GET", "/code-fast?id=10000000", "", 404},
		{"GET", "/code-redis?id=42", "", 503},
		{"GET", "/code-db?id=42", "", 503},
		{"POST", "/code-write", "hello", 202},
		{"POST", "/simple", "", 404},
		{"GET", "/nope", "", 404},
	}
	for _, tc := range tests {
		ctx := call(tc.method, tc.uri, []byte(tc.body))
		if got := ctx.Response.StatusCode(); got != tc.want {
			t.Errorf("%s %s = %d, want %d (%s)", tc.method, tc.uri, got, tc.want, ctx.Response.Body())
		}
		if tc.uri != "/simple" {
			mustJSON(t, ctx.Response.Body(), nil)
		}
	}
}

// Writes with tricky characters must come back byte-for-byte from /code-fast.
func TestWriteThenRead(t *testing.T) {
	t.Setenv("ALLOW_NO_DB", "1")
	payloads := []string{
		`plain`,
		`He said "hi"`,
		`C:\path\with\backslashes`,
		"lines\nand\r\ntabs\t",
		`Unicode: 🚀 100€ 日本語`,
		`{"nested":"json"}`,
	}
	for _, p := range payloads {
		w := call("POST", "/code-write", []byte(p))
		var wr struct{ ID int64 }
		mustJSON(t, w.Response.Body(), &wr)

		r := call("GET", "/code-fast?id="+strconv.FormatInt(wr.ID, 10), nil)
		var rr struct {
			ID   int64
			Code string
		}
		mustJSON(t, r.Response.Body(), &rr)
		if rr.ID != wr.ID || rr.Code != p {
			t.Errorf("round trip: got (%d,%q), want (%d,%q)", rr.ID, rr.Code, wr.ID, p)
		}
	}
}

func TestBadInputs(t *testing.T) {
	t.Setenv("ALLOW_NO_DB", "1")
	bodies := map[string]int{
		"hello\x00world":          400, // NUL byte
		"\xff\xfe":                400, // invalid UTF-8
		strings.Repeat("a", 4097): 413, // too large
	}
	for body, want := range bodies {
		if got := call("POST", "/code-write", []byte(body)).Response.StatusCode(); got != want {
			t.Errorf("POST %q... = %d, want %d", body[:4], got, want)
		}
	}
	for _, id := range []string{"0", "-1", "abc", "", "1.5", "20000000000000000000"} {
		for _, route := range []string{"/code-fast", "/code-redis", "/code-db"} {
			ctx := call("GET", route+"?id="+id, nil)
			if ctx.Response.StatusCode() != 400 {
				t.Errorf("%s?id=%s = %d, want 400", route, id, ctx.Response.StatusCode())
			}
			mustJSON(t, ctx.Response.Body(), nil)
		}
	}
}

func TestWritesNeedDB(t *testing.T) {
	os.Unsetenv("ALLOW_NO_DB")
	if got := call("POST", "/code-write", []byte("x")).Response.StatusCode(); got != 503 {
		t.Errorf("write without DB = %d, want 503", got)
	}
}

func TestParsePositiveInt64(t *testing.T) {
	tests := []struct {
		in   string
		want int64
		ok   bool
	}{
		{"42", 42, true},
		{"9223372036854775807", 9223372036854775807, true}, // MaxInt64
		{"9223372036854775808", 0, false},                  // MaxInt64 + 1
		{"20000000000000000000", 0, false},                 // would wrap to a positive number
		{"0", 0, false},
	}
	for _, tc := range tests {
		if got, ok := parsePositiveInt64([]byte(tc.in)); got != tc.want || ok != tc.ok {
			t.Errorf("parse(%q) = (%d,%v), want (%d,%v)", tc.in, got, ok, tc.want, tc.ok)
		}
	}
}

// Walks items through queue -> pending -> persisted/failed and checks the
// invariant at each step, then proves a lost item is detected.
func TestInvariant(t *testing.T) {
	resetStats()
	defer resetStats()

	for i := range 10 {
		writeQueue <- Item{ID: int64(i + 1)}
		stats.accepted.Add(1)
	}
	for range 6 { // a worker takes 6
		<-writeQueue
		stats.pending.Add(1)
	}
	stats.pending.Add(-6) // 4 saved, 2 failed
	stats.persisted.Add(4)
	stats.failed.Add(2)

	if m := snapshot(); !m.InvariantHolds || m.QueueDepth != 4 {
		t.Fatalf("invariant should hold: %+v", m)
	}

	<-writeQueue // an item disappears without being counted
	if m := snapshot(); m.InvariantHolds || m.Unaccounted != 1 {
		t.Fatalf("lost item not detected: %+v", m)
	}
}

func TestFailedWriteEvictedFromL1(t *testing.T) {
	resetStats()
	defer resetStats()
	const id = 777
	old := "old"
	memoryStore[id].Store(&old)
	stats.pending.Add(1)
	flush([]Item{{ID: id, memPtr: &old}}) // dbPool is nil -> fails
	if memoryStore[id].Load() != nil {
		t.Fatal("failed write still in L1")
	}

	// A newer value must survive an older write's failure.
	newer := "new"
	memoryStore[id].Store(&newer)
	stats.pending.Add(1)
	flush([]Item{{ID: id, memPtr: &old}})
	if p := memoryStore[id].Load(); p == nil || *p != "new" {
		t.Fatal("newer value wrongly evicted")
	}
	memoryStore[id].Store(nil)
}

func TestIsPermanent(t *testing.T) {
	for code, want := range map[string]bool{"22001": true, "23505": true, "40001": false, "08006": false} {
		if got := isPermanent(&pgconn.PgError{Code: code}); got != want {
			t.Errorf("isPermanent(%s) = %v, want %v", code, got, want)
		}
	}
}

func TestGetEnvExactKey(t *testing.T) {
	t.Chdir(t.TempDir())
	os.WriteFile(".env", []byte("PORTAL=9999\nPORT=8081\n"), 0644)
	t.Setenv("PORT", "")
	if got := getEnv("PORT", "x"); got != "8081" {
		t.Fatalf("getEnv(PORT) = %q, want 8081", got)
	}
}

func TestMetricsEndpoint(t *testing.T) {
	resetStats()
	for _, path := range []string{"/metrics", "/debug/pprof/"} {
		rec := httptest.NewRecorder()
		debugMux().ServeHTTP(rec, httptest.NewRequest("GET", path, nil))
		if rec.Code != 200 {
			t.Errorf("GET %s = %d", path, rec.Code)
		}
	}
}

// ---- benchmarks: the read paths must report 0 allocs/op ----

func bench(b *testing.B, method, uri string, body []byte) {
	var ctx fasthttp.RequestCtx
	ctx.Request.Header.SetMethod(method)
	ctx.Request.SetRequestURI(uri)
	ctx.Request.SetBody(body)
	b.ReportAllocs()
	for b.Loop() {
		handler(&ctx)
	}
}

func BenchmarkSimple(b *testing.B) { bench(b, "GET", "/simple", nil) }

func BenchmarkCodeFast(b *testing.B) {
	v := "bench_payload"
	memoryStore[42].Store(&v)
	bench(b, "GET", "/code-fast?id=42", nil)
}

func BenchmarkCodeWrite(b *testing.B) {
	b.Setenv("ALLOW_NO_DB", "1")
	stop := make(chan struct{})
	defer close(stop)
	go func() { // stand-in for the batch workers
		for {
			select {
			case <-writeQueue:
			case <-stop:
				return
			}
		}
	}()
	bench(b, "POST", "/code-write", []byte("bench write payload"))
}

// Guards the old /code-db bug where the closing quote after "code" was missing.
func TestWriteRecordJSON(t *testing.T) {
	var ctx fasthttp.RequestCtx
	writeRecord(&ctx, 7, `a\"b`, "postgresql")
	var r struct {
		ID           int64
		Code, Source string
	}
	mustJSON(t, ctx.Response.Body(), &r)
	if r.ID != 7 || r.Code != `a"b` || r.Source != "postgresql" {
		t.Fatalf("got %+v", r)
	}
}
