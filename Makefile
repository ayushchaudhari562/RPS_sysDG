.PHONY: all build up down seed seed-db seed-redis bench sweep soak overload chaos profile node clean test

all: build

build:
	GOOS=linux GOARCH=amd64 go build -o server_linux main.go

test:
	go vet ./...
	go test -v ./...
	go test -run=^$$ -bench . -benchmem

up:
	docker compose up -d

down:
	docker compose down

seed:
	go run bench/seed/seed.go -pg -redis -pg-rows 1000000 -redis-keys 100000

seed-db:
	go run bench/seed/seed.go -pg -pg-rows 1000000

seed-redis:
	go run bench/seed/seed.go -redis -redis-keys 100000

bench:
	chmod +x bench/run_bench.sh
	./bench/run_bench.sh

sweep:
	chmod +x bench/run_bench.sh
	RUN_ITERATIONS=1 RUN_DURATION=15s ./bench/run_bench.sh

soak:
	chmod +x bench/soak.sh
	./bench/soak.sh

overload:
	wrk -t12 -c160 -d30s --latency -s bench/lua/body_variants.lua http://127.0.0.1:8081/code-write

chaos:
	chmod +x bench/chaos.sh
	./bench/chaos.sh

profile:
	mkdir -p bench/profiles
	curl -s "http://127.0.0.1:6060/debug/pprof/profile?seconds=30" > bench/profiles/cpu.pb.gz
	curl -s "http://127.0.0.1:6060/debug/pprof/heap" > bench/profiles/heap.pb.gz
	go tool pprof -svg bench/profiles/cpu.pb.gz > bench/profiles/cpu.svg 2>/dev/null || true
	go tool pprof -svg bench/profiles/heap.pb.gz > bench/profiles/heap.svg 2>/dev/null || true

node:
	cd bench/node && node server.js

clean:
	rm -f server_linux server.exe bench/chaosclient/chaosclient.exe
