#!/usr/bin/env bash
# gateway-log-rotate.sh — gateway 日志按天轮转 + 清理 7 天前归档
#
# 被两处调用:
#   - systemd(llm-gateway.service 的 ExecStartPre,每次启动前执行 — 否则
#     systemd 接管后日志永不轮转)
#   - gateway-reload.sh(重载前执行)
# 幂等:同一天重复执行无操作。
set -euo pipefail
cd "$(dirname "$0")/.."   # 仓库根

LOG_DIR="$(pwd)/logs"
LOG="$LOG_DIR/gateway.log"
LOG_RETENTION_DAYS="${LOG_RETENTION_DAYS:-7}"

mkdir -p "$LOG_DIR"
TODAY="$(date +%Y%m%d)"
if [ -f "$LOG" ] && [ "$(date -r "$LOG" +%Y%m%d)" != "$TODAY" ]; then
    mv "$LOG" "$LOG_DIR/gateway.log.$TODAY"
    echo "[rotate] 日志轮转:gateway.log → gateway.log.$TODAY"
fi
find "$LOG_DIR" -maxdepth 1 -name "gateway.log.*" -mtime +"$LOG_RETENTION_DAYS" -delete
