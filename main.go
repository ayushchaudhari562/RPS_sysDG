// Command go-1m-rps is a high-throughput HTTP server with a 3-tier read path
// (RAM -> Redis -> PostgreSQL) and a write-behind queue for writes.
//
// File map:
//
//	main.go     startup and graceful shutdown
//	store.go    config, L1 memory table, PostgreSQL and Redis setup
//	handlers.go HTTP routes
//	writer.go   write-behind pipeline (queue -> batch -> COPY -> Redis)
//	metrics.go  counters, /metrics and pprof
package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"runtime"
	"sync"
	"syscall"
	"time"
	"github.com/valyala/fasthttp"
	"github.com/valyala/fasthttp/reuseport"
)

func main() {
	port := getEnv("PORT", "8081")
	metricsAddr := getEnv("METRICS_ADDR", "127.0.0.1:6060")

	// 1. Connect storage and warm the L1 table.
	initDB()
	loadMemory()
	initRedis()
	openDeadLetter()

	// 2. Start background writers.
	go runRedisWriter()
	var workers sync.WaitGroup
	startWorkers(&workers)

	// 3. Start the internal metrics/pprof server and the public HTTP servers.
	debug := &http.Server{Addr: metricsAddr, Handler: debugMux()}
	go func() {
		if err := debug.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Printf("debug server: %v", err)
		}
	}()
	servers := startServers(port)
	log.Printf("listening on :%s with %d SO_REUSEPORT sockets, metrics on %s", port, len(servers), metricsAddr)

	// 4. Block until Ctrl+C or SIGTERM.
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	<-sig

	shutdown(servers, debug, &workers)
}

// startServers opens one listener per CPU core on the same port. With
// SO_REUSEPORT the kernel spreads incoming connections across them, so no
// single accept queue becomes a bottleneck.
func startServers(port string) []*fasthttp.Server {
	servers := make([]*fasthttp.Server, runtime.NumCPU())
	for i := range servers {
		ln, err := reuseport.Listen("tcp4", ":"+port)
		if err != nil {
			log.Fatalf("listen: %v", err)
		}
		s := &fasthttp.Server{
			Handler:               handler,
			NoDefaultServerHeader: true,
			Concurrency:           256 * 1024,
			ReadTimeout:           5 * time.Second, // slowloris protection
			WriteTimeout:          5 * time.Second,
			IdleTimeout:           15 * time.Second,
			MaxRequestBodySize:    8 * 1024,
			TCPKeepalive:          true,
		}
		servers[i] = s
		go func() {
			if err := s.Serve(ln); err != nil {
				log.Printf("server stopped: %v", err)
			}
		}()
	}
	return servers
}

// shutdown stops things in dependency order: producers first (HTTP), then
// consumers (batch workers, Redis writer), then the connections they use.
func shutdown(servers []*fasthttp.Server, debug *http.Server, workers *sync.WaitGroup) {
	log.Println("shutting down: stopping HTTP servers")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	var wg sync.WaitGroup
	for _, s := range servers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := s.ShutdownWithContext(ctx); err != nil {
				log.Printf("server shutdown: %v", err)
			}
		}()
	}
	wg.Wait()
	_ = debug.Shutdown(ctx)

	log.Println("draining write queue")
	close(stopWorkers)
	workers.Wait()

	// redisQueue is never closed: a handler that missed the shutdown deadline
	// could still send to it, and sending on a closed channel panics.
	close(stopRedis)
	<-redisDone

	closeDeadLetter()
	if dbPool != nil {
		dbPool.Close()
	}
	if redisClient != nil {
		redisClient.Close()
	}

	m := snapshot()
	log.Printf("shutdown complete: accepted=%d persisted=%d failed=%d rejected=%d queue=%d",
		m.Accepted, m.Persisted, m.Failed, m.Rejected, m.QueueDepth)
}
