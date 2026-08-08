#!/usr/bin/env bash
# gateway-rollback.sh — 一键复原网关到最后一个已知可用版本
#
# 用途:我(agent)重新编译替换了 bin/gateway,如果新的起不来 / 行为异常、网关病态,
#       你用本脚本把备份恢复,几秒内回到最后一个能跑的二进制。网关不断 → 我能继续工作。
#
# 用法(任选):
#   ./scripts/gateway-rollback.sh                 # 恢复到最新一份备份
#   ./scripts/gateway-rollback.sh <备份名>         # 恢复到指定备份(见 bin/backups/)
#   ./scripts/gateway-rollback.sh --list          # 列出所有可用备份
#
# 端口:默认 8080,可用环境变量 GATEWAY_PORT 覆盖
#
# 机制:完全不依赖 sudo —— 用 `kill $(MainPID)` 触发 systemd's Restart=always(unit 已配
#       RestartSec=3)自动拉起新进程。已验证本环境当前用户即可用(不需要 root)。
#
# 退出码:0 复原成功且 healthz 通过;1 失败(无备份/复制错/网关没起来)

set -euo pipefail
cd "$(dirname "$0")/.."   # 仓库根

BIN="bin/gateway"
BACKUP_DIR="bin/backups"
# 端口默认读 config.yaml 的 server.port(唯一真相),GATEWAY_PORT 可覆盖
DEFAULT_PORT="$(awk '/^  port:/{print $2}' config.yaml 2>/dev/null | head -1)"
PORT="${GATEWAY_PORT:-${DEFAULT_PORT:-8080}}"
SVC="llm-gateway"

log() { echo -e "\033[1;32m[gateway-rollback]\033[0m $*"; }
die() { echo -e "\033[1;31m[gateway-rollback]\033[0m 错误: $*" >&2; exit 1; }

if [ "${1:-}" = "--list" ]; then
    echo "可用备份($BACKUP_DIR/):"
    ls -1t "$BACKUP_DIR"/gateway.* 2>/dev/null | sed 's#.*/##' || echo "  (无)"
    exit 0
fi

# 选定备份:参数指定 / 默认最新
PICK="${1:-}"
if [ -z "$PICK" ]; then
    PICK="$(ls -1t "$BACKUP_DIR"/gateway.* 2>/dev/null | head -1 || true)"
fi
[ -n "$PICK" ] && [ -f "$PICK" ] || die "找不到备份 '$PICK'(可用:ls $BACKUP_DIR/)"

BACKUP="$(cd "$(dirname "$PICK")" && pwd)/$(basename "$PICK")"

# 记录要回滚前的当前二进制(便于再往前走)
TS="$(date +%Y%m%d-%H%M%S)"
if [ -f "$BIN" ]; then
    mv "$BIN" "$BACKUP_DIR/gateway.pre-rollback.$TS"
    log "保存当前二进制(回滚前) → $BACKUP_DIR/gateway.pre-rollback.$TS"
fi

log "恢复 $BACKUP → $BIN"
cp -p "$BACKUP" "$BIN"
chmod 755 "$BIN"

log "触发 systemd 重启 (kill MainPID → Restart=always 自动拉起)..."
OLD_PID=""
if systemctl is-active --quiet "$SVC" 2>/dev/null; then
    OLD_PID="$(systemctl show -p MainPID --value "$SVC" 2>/dev/null)"
    if [ -n "$OLD_PID" ] && [ "$OLD_PID" != "0" ]; then
        kill "$OLD_PID" 2>/dev/null || true
        log "已 kill 旧进程 PID $OLD_PID,等待新进程接管..."
    else
        # 没拿到 PID 也能重启
        systemctl kill -s HUP "$SVC" 2>/dev/null || true
    fi
else
    log "服务未运行,尝试 systemctl start(可能需要 sudo)..."
    sudo systemctl start "$SVC" 2>/dev/null || systemctl start "$SVC" || true
fi

# 第一阶段:等 MainPID 变化(新进程接管),最多 30s
NEWPID=""
for i in $(seq 1 30); do
    NEWPID="$(systemctl show -p MainPID --value "$SVC" 2>/dev/null)"
    # 服务未运行则直接别等了
    if ! systemctl is-active --quiet "$SVC" 2>/dev/null; then
        sleep 1
        continue
    fi
    if [ -n "$NEWPID" ] && { [ "$NEWPID" = "0" ] || [ "$NEWPID" != "$OLD_PID" ]; } && \
       systemctl is-active --quiet "$SVC" 2>/dev/null; then
        break
    fi
    sleep 1
done

# 第二阶段:新进程就绪 + healthz 通过
for i in $(seq 1 30); do
    if [ -n "$NEWPID" ] && [ "$NEWPID" != "0" ] && systemctl is-active --quiet "$SVC" 2>/dev/null \
       && curl -s -m 2 "http://127.0.0.1:${PORT}/healthz" >/dev/null 2>&1; then
        log "✔ 网关已复原并健康 (PID $NEWPID) — 端口 $PORT"
        log "   恢复用: $BACKUP"
        log "   healthz: http://127.0.0.1:${PORT}/healthz"
        exit 0
    fi
    sleep 1
done

die "网关未在 ${PORT} 端口就绪(旧 PID=${OLD_PID:-?},最终 PID=$NEWPID)。请检查 systemctl status $SVC"
