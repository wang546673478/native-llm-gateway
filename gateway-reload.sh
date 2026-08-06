#!/usr/bin/env bash
# gateway-reload.sh — 无感重载(方案 C-快):编译新二进制 → 优雅排空旧进程 → 新进程接管
#
# 用途:加了新厂商包 / 改了 provider 代码后,不用手动 kill + 起。
#   ./gateway-reload.sh
# 从任何目录可运行;脚本自动 cd 到仓库根目录。
#
# 原理:旧进程收到 SIGTERM 后走已有优雅关停(http.Server.Shutdown 排空在飞请求),
# 等它完全退出(端口释放)后启动新进程 — 亚秒级 gap,客户端有重试基本无感。
# 已知代价:重载会重置内存状态(熔断器/配额标记,quotacheck 重新 poll 恢复);
# 排空窗口(shutdown_timeout,默认 30s)内未结束的长流会被掐断。
#
# 日志(P-log-persist,2026-08-06):挪出 /tmp 持久化 + 追加模式 + 按天轮转。
# 之前 > "$LOG" 截断覆盖 — 每次重载旧进程日志全丢(10:54-10:57 关键窗口
# 就因此丢失)。现在:
#   - 日志在 logs/gateway.log(仓库内,机器重启不丢)
#   - 跨天重启时旧日志归档为 gateway.log.YYYYMMDD(追加模式,进程内跨天不轮转)
#   - 启动时清理 7 天前的归档

set -euo pipefail
cd "$(dirname "$0")"

BIN="bin/gateway"
PORT="${GATEWAY_PORT:-8080}"   # 与 config.yaml server.port 一致
LOG_DIR="$(pwd)/logs"
LOG="$LOG_DIR/gateway.log"
DRAIN_MAX="${DRAIN_MAX:-40}"   # 排空 + 端口释放等待上限(秒)
LOG_RETENTION_DAYS="${LOG_RETENTION_DAYS:-7}"

echo "[1/4] 编译新二进制..."
# Go module 在 backend/;go -C 进入模块目录构建,产物落到仓库根 bin/
go -C backend build -o "../${BIN}.new" ./cmd/gateway
mv "${BIN}.new" "${BIN}"

# P-log-persist: 日志目录 + 按天轮转(跨天重启时归档旧日志)+ 清理 7 天前归档
mkdir -p "$LOG_DIR"
TODAY="$(date +%Y%m%d)"
if [ -f "$LOG" ] && [ "$(date -r "$LOG" +%Y%m%d)" != "$TODAY" ]; then
    mv "$LOG" "$LOG_DIR/gateway.log.$TODAY"
    echo "[1/4] 日志轮转:$(basename "$LOG") → gateway.log.$TODAY"
fi
find "$LOG_DIR" -maxdepth 1 -name "gateway.log.*" -mtime +"$LOG_RETENTION_DAYS" -delete

OLD_PID="$(pgrep -f "bin/gateway$" || true)"
if [ -z "$OLD_PID" ]; then
    echo "[2/4] 没有在跑的旧进程,直接启动"
else
    echo "[2/4] 向旧进程 ${OLD_PID} 发 SIGTERM(优雅排空)..."
    kill -TERM "$OLD_PID"
fi

echo "[3/4] 等待旧进程退出 + 端口释放(最多 ${DRAIN_MAX}s)..."
for _ in $(seq 1 "$DRAIN_MAX"); do
    # 旧进程退出后 /healthz 即停止响应
    if ! curl -s -m 1 "http://127.0.0.1:${PORT}/healthz" >/dev/null 2>&1; then
        break
    fi
    sleep 1
done
if [ -n "$OLD_PID" ]; then
    # Shutdown 先关监听再排空在飞请求,进程完全退出可能晚于端口释放 —
    # 必须等 PID 真的消失再起新进程,否则新进程 bind 冲突
    for _ in $(seq 1 40); do
        kill -0 "$OLD_PID" 2>/dev/null || break
        sleep 1
    done
fi

echo "[4/4] 启动新进程..."
# P-log-persist: 追加模式(>>),不再截断旧日志
nohup ./"${BIN}" >> "$LOG" 2>&1 &
for _ in $(seq 1 10); do
    if curl -s -m 1 "http://127.0.0.1:${PORT}/healthz" >/dev/null 2>&1; then
        NEW_PID="$(pgrep -f "bin/gateway$" || true)"
        echo "OK — 重载完成,新进程已接管 (PID ${NEW_PID})"
        exit 0
    fi
    sleep 1
done
echo "ERROR — 新进程未就绪,日志尾部:" >&2
tail -20 "$LOG" >&2
exit 1
