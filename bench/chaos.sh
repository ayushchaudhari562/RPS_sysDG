#!/usr/bin/env bash
set -euo pipefail

SERVER_BIN=${SERVER_BIN:-"./server_linux"}
SERVER_PORT=${SERVER_PORT:-8081}
METRICS_PORT=${METRICS_PORT:-6060}
CHAOS_CLIENT="go run bench/chaosclient/main.go"
ACK_LOG_SIGTERM="bench/results/chaos_ack_sigterm.txt"
ACK_LOG_KILL9="bench/results/chaos_ack_kill9.txt"

mkdir -p bench/results

DATABASE_URL=${DATABASE_URL:-"postgres://postgres:postgrespassword@127.0.0.1:5435/1Million?sslmode=disable"}

query_db_count() {
    psql "${DATABASE_URL}" -t -A -c "SELECT count(*) FROM codes;" 2>/dev/null || echo "0"
}

truncate_table() {
    psql "${DATABASE_URL}" -c "TRUNCATE TABLE codes;" >/dev/null 2>&1 || true
}

echo "=========================================================="
echo " Running Chaos Test Suite"
echo "=========================================================="

echo ""
echo ">>> SCENARIO A: Graceful Shutdown (SIGTERM)"
echo "Resetting database table..."
truncate_table

echo "Starting server..."
${SERVER_BIN} &
SERVER_PID=$!
sleep 2

echo "Launching chaos load generator against /code-write..."
${CHAOS_CLIENT} -url "http://127.0.0.1:${SERVER_PORT}/code-write" -duration 10s -concurrency 40 -log "${ACK_LOG_SIGTERM}" &
CLIENT_PID=$!

sleep 3
echo "Sending SIGTERM to server (PID: ${SERVER_PID}) during load..."
kill -15 "${SERVER_PID}" || true

echo "Waiting for server to complete graceful drain..."
wait "${SERVER_PID}" 2>/dev/null || true
wait "${CLIENT_PID}" 2>/dev/null || true

ACK_COUNT_SIGTERM=$(wc -l < "${ACK_LOG_SIGTERM}" | tr -d ' ')
DB_COUNT_SIGTERM=$(query_db_count | tr -d ' ')

echo "----------------------------------------------------------"
echo "Scenario A Verification:"
echo "202 Acknowledged IDs: ${ACK_COUNT_SIGTERM}"
echo "DB Persisted Rows:    ${DB_COUNT_SIGTERM}"

if [ "${ACK_COUNT_SIGTERM}" -eq "${DB_COUNT_SIGTERM}" ]; then
    echo "PASS: ZERO DATA LOSS! All ${ACK_COUNT_SIGTERM} acknowledged writes persisted safely."
else
    DIFF=$((ACK_COUNT_SIGTERM - DB_COUNT_SIGTERM))
    echo "FAIL: Discrepancy detected! Difference: ${DIFF} rows"
fi

echo ""
echo ">>> SCENARIO B: Hard Abrupt Crash (kill -9)"
echo "Resetting database table..."
truncate_table

echo "Starting server..."
${SERVER_BIN} &
SERVER_PID=$!
sleep 2

echo "Launching chaos load generator against /code-write..."
${CHAOS_CLIENT} -url "http://127.0.0.1:${SERVER_PORT}/code-write" -duration 10s -concurrency 40 -log "${ACK_LOG_KILL9}" &
CLIENT_PID=$!

sleep 3
echo "Sending kill -9 to server (PID: ${SERVER_PID}) during active queue ingestion..."
kill -9 "${SERVER_PID}" || true

wait "${CLIENT_PID}" 2>/dev/null || true

ACK_COUNT_KILL9=$(wc -l < "${ACK_LOG_KILL9}" | tr -d ' ')
DB_COUNT_KILL9=$(query_db_count | tr -d ' ')

LOST_ROWS=$((ACK_COUNT_KILL9 - DB_COUNT_KILL9))
if [ "${LOST_ROWS}" -lt 0 ]; then LOST_ROWS=0; fi

LOSS_PERCENT=0
if [ "${ACK_COUNT_KILL9}" -gt 0 ]; then
    LOSS_PERCENT=$(awk "BEGIN {printf \"%.2f\", (${LOST_ROWS} / ${ACK_COUNT_KILL9}) * 100}")
fi

echo "----------------------------------------------------------"
echo "Scenario B Crash Report (Volatile Buffer Exposure):"
echo "202 Acknowledged IDs: ${ACK_COUNT_KILL9}"
echo "DB Persisted Rows:    ${DB_COUNT_KILL9}"
echo "Lost In-Flight Rows:  ${LOST_ROWS} (${LOSS_PERCENT}% loss)"
echo "Analysis: When killed with SIGKILL (-9), in-memory queue buffer (up to 200k items) is lost as expected for asynchronous write-behind."
echo "=========================================================="
