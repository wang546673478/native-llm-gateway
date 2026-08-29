#!/usr/bin/env bash
# Gateway Health Check Script
# 检查 Native LLM Gateway 运行状况并输出诊断报告

set -euo pipefail

# 配置
GATEWAY_URL="${GATEWAY_URL:-http://localhost:8080}"
CONFIG_FILE="${CONFIG_FILE:-/home/hhhh/llm-gateway/config.yaml}"
SHOW_METRICS="${SHOW_METRICS:-false}"

# 颜色输出
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
NC='\033[0m' # No Color

# 辅助函数
print_section() {
    echo ""
    echo "========================================="
    echo "$1"
    echo "========================================="
}

print_status() {
    local status=$1
    local message=$2
    if [[ "$status" == "OK" ]]; then
        echo -e "${GREEN}✓${NC} $message"
    elif [[ "$status" == "WARN" ]]; then
        echo -e "${YELLOW}⚠${NC} $message"
    else
        echo -e "${RED}✗${NC} $message"
    fi
}

# 检查系统服务状态
check_systemd_service() {
    print_section "1. Systemd Service Status"
    if systemctl is-active --quiet llm-gateway; then
        print_status "OK" "Service is running"
        systemctl status llm-gateway --no-pager | grep -E "(Active|Main PID|Memory|CPU)" || true
    else
        print_status "FAIL" "Service is NOT running"
        return 1
    fi
}

# 检查进程状态
check_process() {
    print_section "2. Process Status"
    if pgrep -f "/home/hhhh/llm-gateway/bin/gateway" > /dev/null; then
        print_status "OK" "Gateway process found"
        ps aux | grep "[/]home/hhhh/llm-gateway/bin/gateway" | awk '{printf "  PID: %s, Memory: %s, CPU: %s%%\n", $2, $6, $3}'
    else
        print_status "FAIL" "Gateway process NOT found"
        return 1
    fi
}

# 检查健康端点
check_health_endpoints() {
    print_section "3. Health Endpoints"

    # /healthz
    if response=$(curl -s -w "\n%{http_code}" "${GATEWAY_URL}/healthz" 2>/dev/null); then
        http_code=$(echo "$response" | tail -1)
        body=$(echo "$response" | sed '$d')
        if [[ "$http_code" == "200" ]]; then
            print_status "OK" "/healthz returned 200"
            echo "  $body"
        else
            print_status "FAIL" "/healthz returned $http_code"
            return 1
        fi
    else
        print_status "FAIL" "/healthz unreachable"
        return 1
    fi

    # /readyz
    if response=$(curl -s -w "\n%{http_code}" "${GATEWAY_URL}/readyz" 2>/dev/null); then
        http_code=$(echo "$response" | tail -1)
        body=$(echo "$response" | sed '$d')
        if [[ "$http_code" == "200" ]]; then
            print_status "OK" "/readyz returned 200"
            echo "  $body"
        else
            print_status "WARN" "/readyz returned $http_code"
        fi
    else
        print_status "WARN" "/readyz unreachable"
    fi
}

# 检查数据库连接
check_database() {
    print_section "4. Database Connection"

    # 从 config.yaml 提取 DSN
    if [[ ! -f "$CONFIG_FILE" ]]; then
        print_status "WARN" "Config file not found: $CONFIG_FILE"
        return 0
    fi

    local driver=$(grep -A 5 "^database:" "$CONFIG_FILE" | grep "driver:" | awk '{print $2}' | tr -d '"')
    local dsn=$(grep -A 5 "^database:" "$CONFIG_FILE" | grep "dsn:" | awk '{print $2}' | tr -d '"')

    if [[ "$driver" == "postgres" ]]; then
        # 解析 PostgreSQL DSN: postgres://user:pass@host:port/dbname
        if [[ $dsn =~ postgres://([^:]+):([^@]+)@([^:]+):([^/]+)/(.+) ]]; then
            local pg_user="${BASH_REMATCH[1]}"
            local pg_pass="${BASH_REMATCH[2]}"
            local pg_host="${BASH_REMATCH[3]}"
            local pg_port="${BASH_REMATCH[4]}"
            local pg_db="${BASH_REMATCH[5]}"

            # 测试连接
            if PGPASSWORD="$pg_pass" psql -h "$pg_host" -p "$pg_port" -U "$pg_user" -d "$pg_db" -c "SELECT 1;" &>/dev/null; then
                print_status "OK" "PostgreSQL connection successful"

                # 查询统计
                local usage_count=$(PGPASSWORD="$pg_pass" psql -h "$pg_host" -p "$pg_port" -U "$pg_user" -d "$pg_db" -t -c "SELECT COUNT(*) FROM usage_records;" 2>/dev/null | xargs)
                local gateway_keys=$(PGPASSWORD="$pg_pass" psql -h "$pg_host" -p "$pg_port" -U "$pg_user" -d "$pg_db" -t -c "SELECT COUNT(*) FROM gateway_keys;" 2>/dev/null | xargs)
                local provider_keys=$(PGPASSWORD="$pg_pass" psql -h "$pg_host" -p "$pg_port" -U "$pg_user" -d "$pg_db" -t -c "SELECT COUNT(*) FROM provider_api_keys;" 2>/dev/null | xargs)

                echo "  Usage records: $usage_count"
                echo "  Gateway keys: $gateway_keys"
                echo "  Provider keys: $provider_keys"
            else
                print_status "FAIL" "PostgreSQL connection failed"
                return 1
            fi
        else
            print_status "WARN" "Could not parse PostgreSQL DSN"
        fi
    elif [[ "$driver" == "sqlite" ]]; then
        print_status "OK" "Using SQLite (driver: $driver)"
    else
        print_status "WARN" "Unknown database driver: $driver"
    fi
}

# 检查 Prometheus 指标
check_metrics() {
    print_section "5. Metrics Summary"

    if ! response=$(curl -s "${GATEWAY_URL}/metrics" 2>/dev/null); then
        print_status "WARN" "/metrics endpoint unreachable"
        return 0
    fi

    # 提取关键指标
    local total_requests=$(echo "$response" | grep 'gateway_requests_total' | grep -v '^#' | awk '{sum += $2} END {print sum}')
    local quota_pending=$(echo "$response" | grep 'gateway_quota_pending_probes' | grep -v '^#' | awk '{print $2}')

    print_status "OK" "Metrics endpoint accessible"
    echo "  Total requests processed: ${total_requests:-0}"
    echo "  Pending quota probes: ${quota_pending:-0}"

    if [[ "$SHOW_METRICS" == "true" ]]; then
        echo ""
        echo "  Recent request distribution:"
        echo "$response" | grep 'gateway_requests_total{' | grep -v '^#' | head -10 | sed 's/^/    /'
    fi
}

# 检查磁盘空间
check_disk_space() {
    print_section "6. Disk Space"

    # 检查 access-body 目录
    local access_body_dir=$(grep -A 10 "access_log:" "$CONFIG_FILE" | grep "body_dir:" | awk '{print $2}' | tr -d '"')
    if [[ -d "$access_body_dir" ]]; then
        local size=$(du -sh "$access_body_dir" 2>/dev/null | awk '{print $1}')
        local disk_usage=$(df -h "$access_body_dir" | tail -1 | awk '{print $5}')
        print_status "OK" "Access log directory: $access_body_dir"
        echo "  Size: $size"
        echo "  Disk usage: $disk_usage"
    else
        print_status "WARN" "Access log directory not found: $access_body_dir"
    fi
}

# 检查最近日志错误
check_recent_errors() {
    print_section "7. Recent Errors (last 50 lines)"

    if journalctl -u llm-gateway -n 50 --no-pager 2>/dev/null | grep -iE "(error|fatal|panic)" | tail -10; then
        print_status "WARN" "Found errors in recent logs (showing last 10)"
    else
        print_status "OK" "No critical errors in recent logs"
    fi
}

# 主函数
main() {
    echo "Native LLM Gateway - Health Check"
    echo "Time: $(date)"
    echo "Gateway URL: $GATEWAY_URL"

    local exit_code=0

    check_systemd_service || exit_code=1
    check_process || exit_code=1
    check_health_endpoints || exit_code=1
    check_database || exit_code=1
    check_metrics || exit_code=1
    check_disk_space || exit_code=1
    check_recent_errors || true  # Don't fail on warnings

    print_section "Summary"
    if [[ $exit_code -eq 0 ]]; then
        print_status "OK" "All critical checks passed"
    else
        print_status "FAIL" "Some checks failed - see above"
    fi

    echo ""
    echo "For detailed metrics, run: SHOW_METRICS=true $0"
    echo "To check specific URL: GATEWAY_URL=http://custom:port $0"

    exit $exit_code
}

main "$@"
