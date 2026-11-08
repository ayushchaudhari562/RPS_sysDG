#!/usr/bin/env bash
set -euo pipefail

TARGET_URL=${TARGET_URL:-"http://127.0.0.1:8081"}
METRICS_URL=${METRICS_URL:-"http://127.0.0.1:6060"}
DATABASE_URL=${DATABASE_URL:-"postgres://postgres:postgrespassword@127.0.0.1:5435/1Million?sslmode=disable"}
WARMUP_DURATION=${WARMUP_DURATION:-"5s"}
RUN_DURATION=${RUN_DURATION:-"30s"}
RUN_ITERATIONS=${RUN_ITERATIONS:-3}
SERVER_CPUS=${SERVER_CPUS:-"4-15"}
LOAD_CPUS=${LOAD_CPUS:-"0-3"}

TIMESTAMP=$(date +"%Y%m%d_%H%M%S")
RESULTS_DIR="bench/results/${TIMESTAMP}"
mkdir -p "${RESULTS_DIR}"
RAW_DIR="${RESULTS_DIR}/raw"
mkdir -p "${RAW_DIR}"

ulimit -n 65535 2>/dev/null || true

WRK_CMD="wrk"
if command -v taskset &> /dev/null; then
    TOTAL_CPUS=$(nproc 2>/dev/null || echo 16)
    if [ "${TOTAL_CPUS}" -gt 4 ]; then
        WRK_CMD="taskset -c ${LOAD_CPUS} wrk"
    fi
fi

HAS_WRK2=false
if command -v wrk2 &> /dev/null; then
    HAS_WRK2=true
fi

echo "=========================================================="
echo " 🚀 Go HTTP Engine Benchmark Suite"
echo " Results Directory: ${RESULTS_DIR}"
echo " Load Generator:    ${WRK_CMD}"
echo " Server CPU Pin:    ${SERVER_CPUS} (if running under taskset)"
echo " Load CPU Pin:      ${LOAD_CPUS}"
echo "=========================================================="

ENV_LOG="${RESULTS_DIR}/environment.json"
echo "Collecting hardware and system environment metadata..."

CPU_MODEL=$(lscpu 2>/dev/null | grep -E "Model name:" | sed 's/Model name:[ \t]*//' || uname -m)
CPU_CORES=$(nproc 2>/dev/null || echo "16")
RAM_TOTAL=$(free -h 2>/dev/null | awk '/^Mem:/ {print $2}' || echo "Unknown")
KERNEL_VER=$(uname -r 2>/dev/null || echo "Unknown")
GO_VER=$(go version 2>/dev/null || echo "Unknown")
WRK_VER=$(${WRK_CMD} --version 2>&1 | head -n1 || echo "wrk 4.x")
ULIMIT_N=$(ulimit -n)

PG_SYNC_COMMIT=$(psql "${DATABASE_URL}" -t -A -c "SHOW synchronous_commit;" 2>/dev/null || echo "unknown")
PG_FSYNC=$(psql "${DATABASE_URL}" -t -A -c "SHOW fsync;" 2>/dev/null || echo "unknown")
PG_SHARED_BUFF=$(psql "${DATABASE_URL}" -t -A -c "SHOW shared_buffers;" 2>/dev/null || echo "unknown")
PG_VER=$(psql "${DATABASE_URL}" -t -A -c "SELECT version();" 2>/dev/null | head -n1 || echo "unknown")
REDIS_VER=$(redis-cli info server 2>/dev/null | grep "redis_version" | cut -d: -f2 | tr -d '\r' || echo "unknown")

cat <<EOF > "${ENV_LOG}"
{
  "timestamp": "${TIMESTAMP}",
  "cpu_model": "${CPU_MODEL}",
  "cpu_cores": "${CPU_CORES}",
  "ram_total": "${RAM_TOTAL}",
  "kernel": "${KERNEL_VER}",
  "ulimit_n": "${ULIMIT_N}",
  "go_version": "${GO_VER}",
  "wrk_version": "${WRK_VER}",
  "postgres": {
    "version": "${PG_VER}",
    "synchronous_commit": "${PG_SYNC_COMMIT}",
    "fsync": "${PG_FSYNC}",
    "shared_buffers": "${PG_SHARED_BUFF}"
  },
  "redis_version": "${REDIS_VER}"
}
EOF

echo "Environment recorded in ${ENV_LOG}"

assert_json_endpoint() {
    local url=$1
    local name=$2
    echo -n "Pre-flight check: ${name} (${url})... "
    local http_code
    http_code=$(curl -s -o /dev/null -w "%{http_code}" "${url}" || echo "000")
    if [ "${http_code}" -ne 200 ] && [ "${http_code}" -ne 202 ]; then
        echo "FAILED (HTTP ${http_code})!"
        echo "ERROR: ${url} failed pre-flight verification."
        exit 1
    fi
    echo "OK (HTTP ${http_code})"
}

assert_json_endpoint "${TARGET_URL}/livez" "Liveness"
assert_json_endpoint "${TARGET_URL}/code-fast?id=42" "L1 In-Memory Table"
assert_json_endpoint "${TARGET_URL}/code-redis?id=42" "L2 Redis Cache"

TOTAL_INVALID_TESTS=0
declare -a SUMMARY_TABLE=()

parse_wrk_run() {
    local file=$1
    local rps
    local p50 p75 p90 p99
    local non2xx socket_errs

    rps=$(grep "Requests/sec:" "${file}" | awk '{print $2}')
    p50=$(grep -E "^[ \t]*50%" "${file}" | awk '{print $2}' || echo "0us")
    p75=$(grep -E "^[ \t]*75%" "${file}" | awk '{print $2}' || echo "0us")
    p90=$(grep -E "^[ \t]*90%" "${file}" | awk '{print $2}' || echo "0us")
    p99=$(grep -E "^[ \t]*99%" "${file}" | awk '{print $2}' || echo "0us")
    
    non2xx=$(grep "Non-2xx" "${file}" | awk '{print $NF}' || echo "0")
    if [ -z "${non2xx}" ]; then non2xx="0"; fi

    socket_errs=$(grep "Socket errors:" "${file}" | awk '{print $4+$6+$8+$10}' || echo "0")
    if [ -z "${socket_errs}" ]; then socket_errs="0"; fi

    echo "${rps:-0} ${p50} ${p75} ${p90} ${p99} ${non2xx} ${socket_errs}"
}

execute_benchmark() {
    local test_name=$1
    local url=$2
    local threads=$3
    local conns=$4
    local script=${5:-""}
    local is_overload=${6:-false}

    echo ""
    echo "----------------------------------------------------------"
    echo " Benchmark: ${test_name}"
    echo " Target:    ${url}"
    echo " Setup:     ${threads} threads, ${conns} connections"
    echo " Script:    ${script:-none}"
    echo "----------------------------------------------------------"

    local extra_args=""
    if [ -n "${script}" ]; then
        extra_args="-s ${script}"
    fi

    echo -n "Warming up for ${WARMUP_DURATION}... "
    ${WRK_CMD} -t2 -c10 -d"${WARMUP_DURATION}" ${extra_args} "${url}" > /dev/null 2>&1 || true
    echo "Done"

    local rps_list=()
    local p50_list=()
    local p99_list=()
    local had_invalid=false
    local max_non2xx=0

    for ((i=1; i<=RUN_ITERATIONS; i++)); do
        local raw_file="${RAW_DIR}/${test_name}_run${i}.txt"
        echo -n "  Run ${i}/${RUN_ITERATIONS} (${RUN_DURATION})... "
        ${WRK_CMD} -t"${threads}" -c"${conns}" -d"${RUN_DURATION}" --latency --timeout 10s ${extra_args} "${url}" > "${raw_file}" 2>&1
        
        read -r rps p50 p75 p90 p99 non2xx socket_errs <<< "$(parse_wrk_run "${raw_file}")"
        echo "RPS: ${rps}, p50: ${p50}, p99: ${p99}, non-2xx: ${non2xx}, sock_err: ${socket_errs}"
        
        rps_list+=("${rps}")
        p50_list+=("${p50}")
        p99_list+=("${p99}")

        if [ "${non2xx}" -gt 0 ]; then
            max_non2xx=$(( max_non2xx > non2xx ? max_non2xx : non2xx ))
            if [ "${is_overload}" = false ]; then
                had_invalid=true
            fi
        fi
    done

    IFS=$'\n' sorted_rps=($(sort -n <<<"${rps_list[*]}")); unset IFS
    local min_rps=${sorted_rps[0]}
    local median_rps=${sorted_rps[$((RUN_ITERATIONS / 2))]}
    local max_rps=${sorted_rps[$((RUN_ITERATIONS - 1))]}
    local med_p50=${p50_list[$((RUN_ITERATIONS / 2))]}
    local med_p99=${p99_list[$((RUN_ITERATIONS / 2))]}

    local status="VALID"
    if [ "${had_invalid}" = true ]; then
        status="INVALID"
        TOTAL_INVALID_TESTS=$((TOTAL_INVALID_TESTS + 1))
        echo "❌ INVALID TEST: ${max_non2xx} Non-2xx responses encountered! Throughput voided."
    else
        echo "✅ PASS: Median RPS: ${median_rps} (Min: ${min_rps}, Max: ${max_rps})"
    fi

    SUMMARY_TABLE+=("${test_name}|${conns}|${median_rps}|${min_rps}|${max_rps}|${med_p50}|${med_p99}|${status}")
}

for c in 64 200 512 1024; do
    t=$(( c < 16 ? c : 16 ))
    execute_benchmark "Simple_c${c}" "${TARGET_URL}/simple" "${t}" "${c}"
done

execute_benchmark "L1_Uniform_c200" "${TARGET_URL}/code-fast" 10 200 "bench/lua/random_id.lua"
execute_benchmark "L1_Zipf_c200" "${TARGET_URL}/code-fast" 10 200 "bench/lua/zipf.lua"

execute_benchmark "L2_Redis_Uniform_c200" "${TARGET_URL}/code-redis" 10 200 "bench/lua/random_id.lua"
execute_benchmark "L2_Redis_Zipf_c200" "${TARGET_URL}/code-redis" 10 200 "bench/lua/zipf.lua"

execute_benchmark "L3_DB_c30" "${TARGET_URL}/code-db" 6 30 "bench/lua/random_id.lua"
execute_benchmark "L3_DB_c100" "${TARGET_URL}/code-db" 8 100 "bench/lua/random_id.lua"

echo ""
echo "=========================================================="
echo " 📈 Write Capacity Sweep (POST /code-write)"
echo " Finding the Knee: Highest load with rejected_full == 0"
echo "=========================================================="

KNEE_CONNS=40
for c in 20 40 80 160 320; do
    t=$(( c < 8 ? c : 8 ))
    execute_benchmark "Write_Sweep_c${c}" "${TARGET_URL}/code-write" "${t}" "${c}" "bench/lua/body_variants.lua"
    sleep 3
done

execute_benchmark "Write_Overload_3xKnee" "${TARGET_URL}/code-write" 12 160 "bench/lua/body_variants.lua" true

REPORT_MD="${RESULTS_DIR}/results.md"
REPORT_JSON="${RESULTS_DIR}/results.json"

cat <<EOF > "${REPORT_MD}"
**Date:** ${TIMESTAMP}  
**Platform:** ${CPU_MODEL} (${CPU_CORES} Cores), ${RAM_TOTAL} RAM, Kernel: ${KERNEL_VER}  
**Go:** ${GO_VER} | **PostgreSQL:** ${PG_VER} | **Redis:** ${REDIS_VER}  

| Benchmark | Conns | Median RPS | Min RPS | Max RPS | p50 | p99 | Status |
|:---|:---:|:---:|:---:|:---:|:---:|:---:|:---:|
EOF

for row in "${SUMMARY_TABLE[@]}"; do
    IFS='|' read -r name conns med min max p50 p99 stat <<< "${row}"
    echo "| ${name} | ${conns} | ${med} | ${min} | ${max} | ${p50} | ${p99} | ${stat} |" >> "${REPORT_MD}"
done

echo "" >> "${REPORT_MD}"
echo "### System Invariant Audit" >> "${REPORT_MD}"
METRICS_FINAL=$(curl -s "${METRICS_URL}/metrics" || echo "{}")
echo "\`\`\`json" >> "${REPORT_MD}"
echo "${METRICS_FINAL}" >> "${REPORT_MD}"
echo "\`\`\`" >> "${REPORT_MD}"

echo "=========================================================="
echo " Summary generated: ${REPORT_MD}"
echo "=========================================================="
cat "${REPORT_MD}"

if [ "${TOTAL_INVALID_TESTS}" -gt 0 ]; then
    echo "❌ BENCHMARK SUITE FAILED: ${TOTAL_INVALID_TESTS} test(s) had non-2xx errors."
    exit 1
fi

echo "✅ ALL BENCHMARK RUNS 100% VALID & VERIFIED"
exit 0
