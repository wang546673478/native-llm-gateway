#!/bin/bash
# Gateway 进程管理 helper(被 Makefile 调用)— systemd 托管版
# 用法:gateway-ctl.sh {find-pid|stop|status} [config-file]
#
# 2026-08-07 起 gateway 由 systemd(llm-gateway.service)托管:
#   - stop/status 委托 systemctl,不再直接 kill(裸 kill 会被 Restart=always 拉起)
#   - stop 需要 sudo(系统服务);find-pid 返回 MainPID 供外部脚本用
set -e

SERVICE="llm-gateway"

find_pid() {
  systemctl show -p MainPID --value "$SERVICE" 2>/dev/null || true
}

stop_gateway() {
  if ! systemctl is-active --quiet "$SERVICE" 2>/dev/null; then
    echo "✗ 没有找到运行中的 Gateway"
    return 0
  fi
  echo "停止 Gateway(systemd)..."
  sudo systemctl stop "$SERVICE"
  echo "✓ 已停止"
}

status_gateway() {
  local pid
  pid=$(find_pid)
  if [ -n "$pid" ] && [ "$pid" != "0" ] && systemctl is-active --quiet "$SERVICE" 2>/dev/null; then
    echo "✓ Gateway 运行中 (PID $pid, systemd)"
    ps -o pid,etime,rss,cmd -p "$pid" 2>/dev/null | tail -1
  else
    echo "✗ Gateway 未运行"
  fi
}

case "${1:-}" in
  find-pid) find_pid ;;
  stop) stop_gateway ;;
  status) status_gateway ;;
  *) echo "用法: $0 {find-pid|stop|status}" >&2; exit 2 ;;
esac
