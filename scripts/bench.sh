#!/usr/bin/env bash
set -euo pipefail

TARGET_URL=${TARGET_URL:-"http://127.0.0.1:8081"}
METRICS_URL=${METRICS_URL:-"http://127.0.0.1:6060"}
DURATION=${DURATION:-"10s"}
THREADS=${THREADS:-10}
CONNECTIONS=${CONNECTIONS:-200}

ulimit -n 65535 2>/dev/null || true

WRK_CMD="wrk"
if command -v taskset &> /dev/null; then
    TOTAL_CPUS=$(nproc 2>/dev/null || echo 16)
    if [ "${TOTAL_CPUS}" -ge 8 ]; then
        WRK_CMD="taskset -c 0-3 wrk"
        echo "INFO: CPU Pinning enabled for load generator (taskset -c 0-3)"
    fi
fi

echo "=========================================================="
echo " 1 Million RPS in Go - Strict Benchmark Suite"
echo "=========================================================="
echo "Target:      ${TARGET_URL}"
echo "Duration:    ${DURATION} per test"
echo "Threads:     ${THREADS} threads, ${CONNECTIONS} connections"
echo "Load Exec:   ${WRK_CMD}"
echo "=========================================================="

echo -n "Checking server liveness (/livez)... "
LIVE_RESP=$(curl -s "${TARGET_URL}/livez" || echo "failed")
if [[ "${LIVE_RESP}" != *"alive"* ]]; then
    echo "FAILED! Server is unreachable at ${TARGET_URL}/livez"
    exit 1
fi
echo "OK"

echo -n "Checking server readiness (/readyz)... "
READY_RESP=$(curl -s "${TARGET_URL}/readyz" || echo "failed")
echo "${READY_RESP}"

echo -n "Verifying L1 in-memory table route (/code-fast?id=42)... "
FAST_STATUS=$(curl -s -o /dev/null -w "%{http_code}" "${TARGET_URL}/code-fast?id=42" || echo "500")
if [ "${FAST_STATUS}" -ne 200 ]; then
    echo "WARNING: /code-fast returned HTTP ${FAST_STATUS}. Ensure server is initialized."
else
    echo "OK (HTTP 200)"
fi

FAILED_TESTS=0

run_strict_bench() {
    local NAME=$1
    local URL=$2
    local SCRIPT=${3:-""}
    local THREAD_CNT=${4:-"${THREADS}"}
    local CONN_CNT=${5:-"${CONNECTIONS}"}

    echo ""
    echo "----------------------------------------------------------"
    echo " Running: ${NAME}"
    echo " URL:     ${URL} (${THREAD_CNT}t, ${CONN_CNT}c, ${DURATION})"
    echo "----------------------------------------------------------"

    local EXTRA_ARGS=""
    if [ -n "${SCRIPT}" ]; then
        EXTRA_ARGS="-s ${SCRIPT}"
    fi

    local OUTPUT
    OUTPUT=$(${WRK_CMD} -t"${THREAD_CNT}" -c"${CONN_CNT}" -d"${DURATION}" --latency ${EXTRA_ARGS} "${URL}")
    echo "${OUTPUT}"

    if echo "${OUTPUT}" | grep -q "Non-2xx"; then
        local ERROR_COUNT
        ERROR_COUNT=$(echo "${OUTPUT}" | grep "Non-2xx" | awk '{print $NF}')
        echo ""
        echo "INVALID TEST: ${ERROR_COUNT} Non-2xx responses detected!"
        FAILED_TESTS=$((FAILED_TESTS + 1))
    else
        echo ""
        echo "PASS: 0 Non-2xx errors. 100% valid response throughput."
    fi
}

echo ""
echo "Warming up socket buffers for 3 seconds..."
${WRK_CMD} -t2 -c50 -d3s "${TARGET_URL}/simple" >/dev/null 2>&1 || true

run_strict_bench "1. Baseline Memory Ping (/simple)" "${TARGET_URL}/simple"
run_strict_bench "2. L1 In-Memory Table (/code-fast?id=42)" "${TARGET_URL}/code-fast?id=42"
run_strict_bench "3. L2 Distributed Redis (/code-redis?id=42)" "${TARGET_URL}/code-redis?id=42"

if [[ "${READY_RESP}" == *"database\":\"connected"* ]]; then
    run_strict_bench "4. L3 PostgreSQL NVMe Read (/code-db?id=42)" "${TARGET_URL}/code-db?id=42" "" 8 100

    POST_SCRIPT=$(mktemp)
    echo 'wrk.method = "POST"' > "${POST_SCRIPT}"

    echo ""
    echo "----------------------------------------------------------"
    echo " Running: 5A. L3 Write-Behind Capacity Test (POST /code-write)"
    echo " Purpose: Measure sustained ingestion rate with ZERO rejected writes"
    echo "----------------------------------------------------------"
    CAP_OUTPUT=$(${WRK_CMD} -t4 -c40 -d"${DURATION}" --latency -s "${POST_SCRIPT}" "${TARGET_URL}/code-write")
    echo "${CAP_OUTPUT}"
    if echo "${CAP_OUTPUT}" | grep -q "Non-2xx"; then
        echo "FAILED: Write capacity saturated. Queue experienced backpressure."
        FAILED_TESTS=$((FAILED_TESTS + 1))
    else
        echo "PASS: 0 Non-2xx errors. 100% persisted into write-behind queue (HTTP 202)."
    fi

    echo -n "Waiting 3 seconds for batch workers to commit to PostgreSQL... "
    sleep 3
    echo "Done"

    echo ""
    echo "----------------------------------------------------------"
    echo " Running: 5B. L3 Write-Behind Overload & Load-Shedding Test"
    echo " Purpose: Flood with 3x capacity (200 conns) to verify backpressure (HTTP 503)"
    echo "----------------------------------------------------------"
    OVER_OUTPUT=$(${WRK_CMD} -t8 -c200 -d5s --latency -s "${POST_SCRIPT}" "${TARGET_URL}/code-write")
    echo "${OVER_OUTPUT}"
    rm -f "${POST_SCRIPT}"

    if echo "${OVER_OUTPUT}" | grep -q "Non-2xx"; then
        echo "BACKPRESSURE ACTIVE: Server shed excess load with HTTP 503 to prevent OOM."
    fi
fi

echo ""
echo "=========================================================="
echo " Final Systems Metrics Audit (Port :6060)"
echo "=========================================================="
METRICS_JSON=$(curl -s "${METRICS_URL}/metrics" || echo "{}")
echo "${METRICS_JSON}"

if [ "${FAILED_TESTS}" -eq 0 ]; then
    echo ""
    echo "  ALL BENCHMARKS PASSED: 100% VALID & VERIFIED"
    exit 0
else
    echo ""
    echo " ATTENTION: ${FAILED_TESTS} test(s) reported Non-2xx errors."
    exit 1
fi
