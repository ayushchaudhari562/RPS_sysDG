package main

import (
	"encoding/json"
	"net/http"
	"net/http/pprof"
	"sync/atomic"
)

// lastID is the last write ID handed out (continues from MAX(id) on startup).
var lastID atomic.Int64

// stats are the write-path counters. Every accepted write is always in
// exactly one place, which gives the invariant:
//
//	accepted == persisted + failed + pending + queue_depth
//
// queue_depth = still in writeQueue, pending = taken by a worker but not yet
// flushed. If the two sides differ after traffic stops, a write was lost.
var stats struct {
	accepted    atomic.Int64 // got 202
	rejected    atomic.Int64 // got 503, queue full
	persisted   atomic.Int64 // committed to PostgreSQL
	failed      atomic.Int64 // dropped after retries or dead-lettered
	pending     atomic.Int64 // inside a worker's batch
	redisErrors atomic.Int64 // failed or dropped Redis writes (cache only)
}

type Metrics struct {
	Accepted       int64 `json:"accepted"`
	Rejected       int64 `json:"rejected_full"`
	Persisted      int64 `json:"persisted"`
	Failed         int64 `json:"failed"`
	Pending        int64 `json:"pending_in_batches"`
	QueueDepth     int64 `json:"queue_depth"`
	RedisErrors    int64 `json:"redis_errors"`
	Unaccounted    int64 `json:"unaccounted"`
	InvariantHolds bool  `json:"invariant_holds"`
}

// snapshot reads the counters one by one (not atomically as a group), so under
// live load Unaccounted can be off by a few in-flight requests. Once traffic
// stops and the queue drains it must be exactly 0.
func snapshot() Metrics {
	m := Metrics{
		Accepted:    stats.accepted.Load(),
		Rejected:    stats.rejected.Load(),
		Persisted:   stats.persisted.Load(),
		Failed:      stats.failed.Load(),
		Pending:     stats.pending.Load(),
		QueueDepth:  int64(len(writeQueue)),
		RedisErrors: stats.redisErrors.Load(),
	}
	m.Unaccounted = m.Accepted - (m.Persisted + m.Failed + m.Pending + m.QueueDepth)
	m.InvariantHolds = m.Unaccounted == 0
	return m
}

// debugMux serves /metrics and pprof on the internal port only (127.0.0.1),
// never on the public one: pprof exposes memory and command-line details.
func debugMux() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("/debug/pprof/", pprof.Index)
	mux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
	mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
	mux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
	mux.HandleFunc("/debug/pprof/trace", pprof.Trace)
	mux.HandleFunc("/metrics", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(snapshot())
	})
	return mux
}
