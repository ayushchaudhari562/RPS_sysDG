#!/usr/bin/env bash
set -euo pipefail

TARGET_URL=${TARGET_URL:-"http://127.0.0.1:8081"}
METRICS_URL=${METRICS_URL:-"http://127.0.0.1:6060"}
DATABASE_URL=${DATABASE_URL:-"postgres://postgres:postgrespassword@127.0.0.1:5435/1Million?sslmode=disable"}
SOAK_DURATION=${SOAK_DURATION:-"300s"}
SOAK_CONNS=${SOAK_CONNS:-32}
SOAK_THREADS=${SOAK_THREADS:-4}

TIMESTAMP=$(date +"%Y%m%d_%H%M%S")
RESULTS_DIR="bench/results/soak_${TIMESTAMP}"
mkdir -p "${RESULTS_DIR}"
CSV_FILE="${RESULTS_DIR}/soak_metrics.csv"

echo "=========================================================="
echo " ⏳ Starting Soak Test (5 Minutes at 80% Capacity Knee)"
echo " Target:        ${TARGET_URL}/code-write"
echo " Duration:      ${SOAK_DURATION}"
echo " Connections:   ${SOAK_CONNS} (${SOAK_THREADS} threads)"
echo " Output CSV:    ${CSV_FILE}"
echo "=========================================================="

psql "${DATABASE_URL}" -c "TRUNCATE TABLE codes;" >/dev/null 2>&1 || true

echo "timestamp,accepted,persisted,failed,rejected_full,pending_in_batches,queue_depth,rps_persisted" > "${CSV_FILE}"

SAMPLER_STOP=false
sample_metrics() {
    local prev_persisted=0
    while [ "${SAMPLER_STOP}" = false ]; do
        local m_json
        m_json=$(curl -s "${METRICS_URL}/metrics" || echo "{}")
        local ts=$(date +"%H:%M:%S")

        local acc=$(echo "${m_json}" | grep -o '"accepted":[0-9]*' | cut -d: -f2 || echo 0)
        local per=$(echo "${m_json}" | grep -o '"persisted":[0-9]*' | cut -d: -f2 || echo 0)
        local fail=$(echo "${m_json}" | grep -o '"failed":[0-9]*' | cut -d: -f2 || echo 0)
        local rej=$(echo "${m_json}" | grep -o '"rejected_full":[0-9]*' | cut -d: -f2 || echo 0)
        local pen=$(echo "${m_json}" | grep -o '"pending_in_batches":[0-9]*' | cut -d: -f2 || echo 0)
        local qd=$(echo "${m_json}" | grep -o '"queue_depth":[0-9]*' | cut -d: -f2 || echo 0)

        local delta_persisted=$(( per - prev_persisted ))
        prev_persisted=${per}

        echo "${ts},${acc},${per},${fail},${rej},${pen},${qd},${delta_persisted}" >> "${CSV_FILE}"
        sleep 1
    done
}

sample_metrics &
SAMPLER_PID=$!

echo "Launching 5-minute sustained write-behind load..."
wrk -t"${SOAK_THREADS}" -c"${SOAK_CONNS}" -d"${SOAK_DURATION}" --latency -s bench/lua/body_variants.lua "${TARGET_URL}/code-write" > "${RESULTS_DIR}/wrk_soak_output.txt" 2>&1

echo "Load generation complete. Waiting for write queue to drain to zero..."
for i in {1..30}; do
    m_json=$(curl -s "${METRICS_URL}/metrics" || echo "{}")
    pen=$(echo "${m_json}" | grep -o '"pending_in_batches":[0-9]*' | cut -d: -f2 || echo 0)
    qd=$(echo "${m_json}" | grep -o '"queue_depth":[0-9]*' | cut -d: -f2 || echo 0)
    if [ "${pen}" -eq 0 ] && [ "${qd}" -eq 0 ]; then
        echo "Queue fully drained (pending: 0, queue_depth: 0) after ${i}s."
        break
    fi
    sleep 1
done

SAMPLER_STOP=true
kill "${SAMPLER_PID}" 2>/dev/null || true

FINAL_METRICS=$(curl -s "${METRICS_URL}/metrics" || echo "{}")
FINAL_PERSISTED=$(echo "${FINAL_METRICS}" | grep -o '"persisted":[0-9]*' | cut -d: -f2 || echo 0)
DB_ROW_COUNT=$(psql "${DATABASE_URL}" -t -A -c "SELECT count(*) FROM codes;" 2>/dev/null || echo 0)

echo "=========================================================="
echo " Soak Test Audit Summary"
echo "=========================================================="
echo "Metrics Persisted: ${FINAL_PERSISTED}"
echo "DB Count Delta:    ${DB_ROW_COUNT}"

if [ "${FINAL_PERSISTED}" -eq "${DB_ROW_COUNT}" ]; then
    echo "✅ PASS: Perfect match! All ${FINAL_PERSISTED} writes verified in PostgreSQL."
else
    echo "❌ FAIL: Mismatch between metrics persisted and DB count!"
fi

echo "Results saved to: ${RESULTS_DIR}"
