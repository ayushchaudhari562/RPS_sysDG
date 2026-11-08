#!/usr/bin/env bash

PORT=${PORT:-6379}
SERVER_URL=${SERVER_URL:-"http://127.0.0.1:8081"}

DOCKER_BIN="docker"
if ! docker ps &>/dev/null && command -v docker.exe &>/dev/null; then
    DOCKER_BIN="docker.exe"
fi

COMMAND=${1:-"bench"}

case "$COMMAND" in
  start)
    echo "Starting Docker Redis container..."
    $DOCKER_BIN start redis-server 2>/dev/null || $DOCKER_BIN run -d --name redis-server -p ${PORT}:6379 redis:latest
    ;;

  stop)
    echo "Stopping Docker Redis container..."
    $DOCKER_BIN stop redis-server
    ;;

  status)
    echo "Checking Redis connectivity on port ${PORT}..."
    if timeout 1 bash -c "</dev/tcp/127.0.0.1/${PORT}" 2>/dev/null; then
        echo " Redis is UP and listening on 127.0.0.1:${PORT}"
    else
        echo " Redis is NOT reachable on 127.0.0.1:${PORT}"
    fi
    $DOCKER_BIN ps --filter "name=redis-server"
    ;;

  cli)
    echo "Opening redis-cli in container..."
    $DOCKER_BIN exec -it redis-server redis-cli
    ;;

  seed)
    echo "Seeding 10,000 keys directly into Redis via redis-cli pipeline..."
    awk 'BEGIN {
        for (i=1; i<=10000; i++) {
            print "SET code:" i " 0be1d3500de85ee851089e3f6d8b856cce0c800c6d53b85773aeacc94922ef45f180489c61945ba798f7d24158472e52be420be5f68b95f77ea71b306282fb7c1106a538cbc9d32570e03c41f1500c860f558dee9956ec059d6859ae70536bdb724804f179a809fd02d2fd3e16ba3490224bde55d23efdf19fea5e4eaf6610d4027e8b1cd96cf8b0ed3aa275a56f3b41949e053e1f4232fe80c8dc7fcfa7650604cba5366e15937bdcb67d5f2d8b222d9a44ccfe30ff5315cf60b875441baba0e4c13c7099fffb04e7793b3dacbcfee1c109d99575735935cc41e0767123d051dc8b71c112e07fd88145feb8a4c0f1dc56f2965cf11d971b2820 EX 3600"
        }
    }' | $DOCKER_BIN exec -i redis-server redis-cli >/dev/null
    echo "Done! Seeded 10,000 keys into Redis directly."
    ;;

  bench)
    echo "Running wrk benchmark on ${SERVER_URL}/code-redis?id=42..."
    ulimit -n 65535 2>/dev/null || true
    wrk -t9 -c250 -d10s --latency "${SERVER_URL}/code-redis?id=42"
    ;;

  help|--help|-h)
    echo "Usage: ./redis.sh [command]"
    echo ""
    echo "Commands:"
    echo "  bench   (default) Run wrk 10s benchmark on /code-redis"
    echo "  seed    Pre-seed 10,000 records into Redis"
    echo "  start   Start Docker Redis container"
    echo "  stop    Stop Docker Redis container"
    echo "  status  Check Redis port and container status"
    echo "  cli     Open interactive redis-cli"
    ;;

  *)
    echo "Unknown command: $1"
    echo "Run './redis.sh help' for available commands."
    exit 1
    ;;
esac
