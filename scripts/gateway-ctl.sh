#!/bin/bash
# Gateway 进程管理 helper(被 Makefile 调用)
# 用法:gateway-ctl.sh {find-pid|stop|status} [config-file]
set -e

PORT="${PORT:-8080}"
LOG="${LOG:-/tmp/gateway.log}"
PIDFILE="${PIDFILE:-/tmp/gateway.pid}"

# 找 gateway 进程 PID(优先 PID 文件,其次端口)
find_pid() {
  if [ -f "$PIDFILE" ]; then
    local pid
    pid=$(cat "$PIDFILE")
    if [ -n "$pid" ] && kill -0 "$pid" 2>/dev/null; then
      echo "$pid"
      return
    fi
  fi
  # fallback: 从端口找
  if command -v lsof >/dev/null 2>&1; then
    lsof -ti tcp:"$PORT" 2>/dev/null | head -1
  else
    ss -tlnp 2>/dev/null | grep ":$PORT " | grep -oP 'pid=\K[0-9]+' | head -1
  fi
}

stop_gateway() {
  local pid
  pid=$(find_pid)
  if [ -z "$pid" ]; then
    echo "✗ 没有找到 Gateway 进程"
    rm -f "$PIDFILE"
    return 0
  fi
  echo "停止 Gateway (PID $pid)..."
  kill -TERM "$pid" 2>/dev/null || true
  for i in 1 2 3 4 5; do
    sleep 1
    if ! kill -0 "$pid" 2>/dev/null; then
      echo "✓ 已停止"
      rm -f "$PIDFILE"
      return 0
    fi
  done
  echo "✗ 进程未响应 TERM,强制 KILL"
  kill -KILL "$pid" 2>/dev/null || true
  rm -f "$PIDFILE"
}

status_gateway() {
  local pid
  pid=$(find_pid)
  if [ -n "$pid" ] && kill -0 "$pid" 2>/dev/null; then
    echo "✓ Gateway 运行中 (PID $pid)"
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
