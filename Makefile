.PHONY: build run test up down seed bench soak chaos profile clean

build:            ## Linux binary for WSL2
	GOOS=linux GOARCH=amd64 go build -o server_linux .

run:
	BENCH_FIXTURE=1 go run .

test:
	go vet ./...
	go test ./...
	go test -run=^$$ -bench . -benchmem

up:               ## start PostgreSQL + Redis
	docker compose up -d

down:
	docker compose down

seed:             ## 1M rows in PostgreSQL, 100k keys in Redis
	go run ./bench/seed -pg -redis -pg-rows 1000000 -redis-keys 100000

bench:            ## full read + write benchmark suite
	./bench/run_bench.sh

soak:             ## 5 minutes of steady writes, then DB count check
	./bench/soak.sh

chaos:            ## SIGTERM vs kill -9 data-loss test
	./bench/chaos.sh

profile:          ## 30s CPU profile from a running server
	mkdir -p bench/profiles
	curl -s "http://127.0.0.1:6060/debug/pprof/profile?seconds=30" > bench/profiles/cpu.pb.gz
	go tool pprof -top bench/profiles/cpu.pb.gz | head -25

clean:
	rm -f server_linux server.exe
