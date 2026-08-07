#!/usr/bin/env bash
# gateway-reload.sh — 无感重载(systemd 托管版):编译新二进制 → systemctl restart
#
# 用途:加了新厂商包 / 改了 provider 代码后,更新线上网关。
#   ./gateway-reload.sh
# 从任何目录可运行;脚本自动 cd 到仓库根目录。
#
# 原理(2026-08-07 起 systemd 托管):编译新二进制替换 bin/gateway 后,
# systemctl restart 让 systemd 完成 SIGTERM 优雅排空 + 新进程接管 —
# 不再手动 kill/等待/后台起(旧裸进程版那套会让进程脱离 systemd 管控,
# 状态混乱)。restart 需要 sudo(脚本内调用,终端会提示输入密码)。
#
# 已知代价:重载会重置内存状态(熔断器/配额标记,从 key-state.json 快照
# 恢复 QE/COOLING);排空窗口(shutdown_timeout,默认 30s)内未结束的长流会被掐断。
#
# 日志:logs/gateway.log(仓库内持久)。跨天归档 + 7 天清理由
# scripts/gateway-log-rotate.sh 完成(systemd ExecStartPre 每次启动自动执行,
# 本脚本不需要再做轮转)。

set -euo pipefail
cd "$(dirname "$0")"

BIN="bin/gateway"
PORT="${GATEWAY_PORT:-8080}"

echo "[1/3] 编译新二进制..."
go -C backend build -o "../${BIN}.new" ./cmd/gateway
mv "${BIN}.new" "${BIN}"

echo "[2/3] systemctl restart llm-gateway(优雅排空由 systemd 完成)..."
sudo systemctl restart llm-gateway

echo "[3/3] 等待就绪..."
for _ in $(seq 1 15); do
    if curl -s -m 1 "http://127.0.0.1:${PORT}/healthz" >/dev/null 2>&1; then
        PID="$(systemctl show -p MainPID --value llm-gateway)"
        echo "OK — 重载完成,gateway 已接管 (PID ${PID},systemd)"
        exit 0
    fi
    sleep 1
done
echo "ERROR — gateway 未就绪,日志尾部:" >&2
tail -20 logs/gateway.log >&2
exit 1
