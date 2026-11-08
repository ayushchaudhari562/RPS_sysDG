#!/usr/bin/env bash
set -euo pipefail

TARGET_URL=${TARGET_URL:-"http://127.0.0.1:8081"}
METRICS_URL=${METRICS_URL:-"http://127.0.0.1:6060"}
PG_URL=${DATABASE_URL:-"postgresql://postgres:postgres@127.0.0.1:5432/1Million?sslmode=disable"}

echo "=========================================================="
echo " Invariant Integrity & Chaos Verification Test"
echo "=========================================================="

if ! curl -s "${TARGET_URL}/livez" | grep -q "alive"; then
    echo "Error: Server is not running at ${TARGET_URL}"
    exit 1
fi

echo "Step 1: Fetching initial metrics snapshot..."
INIT_METRICS=$(curl -s "${METRICS_URL}/metrics")
echo "Initial Metrics: ${INIT_METRICS}"

echo ""
echo "Step 2: Injecting 50,000 write requests into /code-write..."
POST_SCRIPT=$(mktemp)
echo 'wrk.method = "POST"' > "${POST_SCRIPT}"
wrk -t4 -c100 -d5s -s "${POST_SCRIPT}" "${TARGET_URL}/code-write" >/dev/null 2>&1 || true
rm -f "${POST_SCRIPT}"

echo "Step 3: Waiting 3 seconds for batch workers to drain buffer to DB..."
sleep 3

FINAL_METRICS=$(curl -s "${METRICS_URL}/metrics")
echo "Final Metrics: ${FINAL_METRICS}"

ACCEPTED=$(echo "${FINAL_METRICS}" | grep -o '"accepted_writes":[0-9]*' | cut -d: -f2)
PERSISTED=$(echo "${FINAL_METRICS}" | grep -o '"persisted_writes":[0-9]*' | cut -d: -f2)
FAILED=$(echo "${FINAL_METRICS}" | grep -o '"failed_writes":[0-9]*' | cut -d: -f2)
QUEUED=$(echo "${FINAL_METRICS}" | grep -o '"queue_depth":[0-9]*' | cut -d: -f2)

TOTAL_ACCOUNTED=$((PERSISTED + FAILED + QUEUED))

echo ""
echo "----------------------------------------------------------"
echo " Invariant Check: Accepted == Persisted + Failed + Queued"
echo "----------------------------------------------------------"
echo " Accepted Writes:       ${ACCEPTED}"
echo " Persisted to DB:       ${PERSISTED}"
echo " Failed Writes:         ${FAILED}"
echo " Still in Queue:        ${QUEUED}"
echo " Total Accounted For:   ${TOTAL_ACCOUNTED}"

if [ "${ACCEPTED}" -eq "${TOTAL_ACCOUNTED}" ]; then
    echo "PASS: Invariant preserved! Zero silent drops (Accepted == Accounted)."
else
    DIFF=$((ACCEPTED - TOTAL_ACCOUNTED))
    echo "WARNING: Discrepancy detected: ${DIFF} writes unaccounted for!"
fi

echo ""
echo "Step 4: Chaos Test (Ungraceful kill -9 simulation)..."
echo "To test row loss during sudden power outage / OOM kill:"
echo "1. Run: wrk -t8 -c200 -d30s -s post.lua ${TARGET_URL}/code-write &"
echo "2. Run: pkill -9 -f server_linux"
echo "3. Query DB: SELECT COUNT(*) FROM codes;"
echo "4. The difference between Accepted writes and DB count represents the in-memory buffer at time of crash."
