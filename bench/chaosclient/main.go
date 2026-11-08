package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

type WriteResponse struct {
	Status string `json:"status"`
	ID     int64  `json:"id"`
}

func main() {
	targetURL := flag.String("url", "http://127.0.0.1:8081/code-write", "Target POST write URL")
	concurrency := flag.Int("concurrency", 50, "Number of concurrent POST workers")
	duration := flag.Duration("duration", 30*time.Second, "Test duration (or until SIGINT/SIGTERM)")
	logPath := flag.String("log", "bench/chaosclient/acknowledged_ids.txt", "File path to record acknowledged IDs")
	flag.Parse()

	log.Printf("Starting Chaos Client: %d workers -> %s (duration %v)", *concurrency, *targetURL, *duration)

	logFile, err := os.Create(*logPath)
	if err != nil {
		log.Fatalf("Failed to create acknowledged IDs file: %v", err)
	}
	defer logFile.Close()

	writer := bufio.NewWriterSize(logFile, 64*1024)
	defer writer.Flush()

	idChan := make(chan int64, 50000)
	var writerWG sync.WaitGroup
	writerWG.Add(1)

	var totalAcknowledged int64

	go func() {
		defer writerWG.Done()
		for id := range idChan {
			_, _ = fmt.Fprintf(writer, "%d\n", id)
			atomic.AddInt64(&totalAcknowledged, 1)
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), *duration)
	defer cancel()

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)
	go func() {
		select {
		case <-sigChan:
			log.Println("Interrupt received, stopping client load...")
			cancel()
		case <-ctx.Done():
		}
	}()

	client := &http.Client{
		Transport: &http.Transport{
			MaxIdleConns:        *concurrency * 2,
			MaxIdleConnsPerHost: *concurrency * 2,
			IdleConnTimeout:     30 * time.Second,
			DisableCompression:  true,
		},
		Timeout: 5 * time.Second,
	}

	var totalRequests int64
	var totalBackpressure int64
	var totalErrors int64

	var workerWG sync.WaitGroup
	payload := []byte(`{"event":"chaos_test_payload","metric":42}`)

	start := time.Now()

	for i := 0; i < *concurrency; i++ {
		workerWG.Add(1)
		go func(workerID int) {
			defer workerWG.Done()

			for {
				select {
				case <-ctx.Done():
					return
				default:
				}

				req, reqErr := http.NewRequestWithContext(ctx, "POST", *targetURL, bytes.NewReader(payload))
				if reqErr != nil {
					return
				}
				req.Header.Set("Content-Type", "application/json")

				resp, doErr := client.Do(req)
				if doErr != nil {
					atomic.AddInt64(&totalErrors, 1)
					continue
				}

				atomic.AddInt64(&totalRequests, 1)

				if resp.StatusCode == http.StatusAccepted {
					body, readErr := io.ReadAll(resp.Body)
					_ = resp.Body.Close()
					if readErr == nil {
						var wr WriteResponse
						if jErr := json.Unmarshal(body, &wr); jErr == nil && wr.ID > 0 {
							idChan <- wr.ID
						}
					}
				} else if resp.StatusCode == http.StatusServiceUnavailable {
					_, _ = io.Copy(io.Discard, resp.Body)
					_ = resp.Body.Close()
					atomic.AddInt64(&totalBackpressure, 1)
				} else {
					_, _ = io.Copy(io.Discard, resp.Body)
					_ = resp.Body.Close()
					atomic.AddInt64(&totalErrors, 1)
				}
			}
		}(i)
	}

	workerWG.Wait()
	close(idChan)
	writerWG.Wait()
	_ = writer.Flush()

	elapsed := time.Since(start)
	ack := atomic.LoadInt64(&totalAcknowledged)
	bp := atomic.LoadInt64(&totalBackpressure)
	reqs := atomic.LoadInt64(&totalRequests)
	errs := atomic.LoadInt64(&totalErrors)

	log.Println("==========================================================")
	log.Printf("Chaos Client Run Completed in %v", elapsed)
	log.Printf("Total Requests:       %d", reqs)
	log.Printf("202 Acknowledged:     %d (Saved to %s)", ack, *logPath)
	log.Printf("503 Backpressure:     %d", bp)
	log.Printf("Network/Other Errors: %d", errs)
	log.Printf("Avg Acknowledged Rate: %.0f req/s", float64(ack)/elapsed.Seconds())
	log.Println("==========================================================")
}
