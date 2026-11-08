//go:build ignore

package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log"
	"os"
	"strconv"
	"time"

	"github.com/redis/rueidis"
)

func main() {
	redisURL := os.Getenv("REDIS_URL")
	if redisURL == "" {
		redisURL = "127.0.0.1:6379"
	}

	targetKeys := 1000000
	pipelineBatch := 5000

	client, err := rueidis.NewClient(rueidis.ClientOption{
		InitAddress: []string{redisURL},
	})
	if err != nil {
		log.Fatalf("Redis connection error: %v", err)
	}
	defer client.Close()

	ctx := context.Background()
	if err := client.Do(ctx, client.B().Ping().Build()).Error(); err != nil {
		log.Fatalf("Redis ping failed at %s: %v", redisURL, err)
	}

	dummyBytes := make([]byte, 250)
	_, _ = rand.Read(dummyBytes)
	dummyVal := hex.EncodeToString(dummyBytes)

	log.Printf("Connected to Redis at %s. Seeding %d keys in batches of %d...", redisURL, targetKeys, pipelineBatch)
	start := time.Now()

	for startKey := 1; startKey <= targetKeys; startKey += pipelineBatch {
		cmds := make(rueidis.Commands, 0, pipelineBatch)
		endKey := startKey + pipelineBatch - 1
		if endKey > targetKeys {
			endKey = targetKeys
		}

		for k := startKey; k <= endKey; k++ {
			key := "code:" + strconv.Itoa(k)
			cmds = append(cmds, client.B().Set().Key(key).Value(dummyVal).Ex(1*time.Hour).Build())
		}

		resps := client.DoMulti(ctx, cmds...)
		for _, resp := range resps {
			if err := resp.Error(); err != nil {
				log.Fatalf("Pipeline error: %v", err)
			}
		}

		fmt.Printf("\rSeeded: %d / %d keys in Redis...", endKey, targetKeys)
	}
	elapsed := time.Since(start)
	fmt.Printf("\nSUCCESS: Seeded %d keys in %v (%.0f keys/sec)\n",
		targetKeys, elapsed, float64(targetKeys)/elapsed.Seconds())
}
