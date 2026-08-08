#!/usr/bin/env bash
# gateway-deploy.sh — 安全的「备份→编译→部署→验证,失败自动回滚」一键流程
#
# 这是我(agent)每次改完 Go 代码后的标准发布流程封装。它保证:
#   1. 编译前先备份当前「已知可用」的 gateway 二进制(gateway-backup.sh)
#   2. 编译新二进制到 bin/gateway.new(不动正在运行的 bin/gateway)
#   3. 用 kill MainPID 触发 systemd 自动重启(新二进制接管)
#   4. 等 PID 变化 + healthz 就绪
#   5. **万一新二进制起不来 → 自动恢复备份 + 重启 + 再验证**,网关不断
#
# 用法:  ./scripts/gateway-deploy.sh
#   - 可选环境变量 GATEWAY_PORT 覆盖健康检查端口(默认 8080)
#   - 可选环境变量 SKIP_TEST=1 跳过 go test(仅想快速部署已测过的代码时用)
#
# 退出码:0 新版本部署成功;2 新版本失败但已自动回滚到旧版;1 其它错误

set -uo pipefail   # 注意:不用 -e,我们需要捕获失败做回滚
cd "$(dirname "$0")/.."   # 仓库根

BIN="bin/gateway"
NEWBIN="bin/gateway.new"
BACKUP_DIR="bin/backups"
# 端口默认读 config.yaml 的 server.port(唯一真相),而非硬编码 8080;
# GATEWAY_PORT 环境变量可覆盖。
DEFAULT_PORT="$(awk '/^  port:/{print $2}' config.yaml 2>/dev/null | head -1)"
PORT="${GATEWAY_PORT:-${DEFAULT_PORT:-8080}}"
SVC="llm-gateway"
SKIP_TEST="${SKIP_TEST:-0}"

log() { echo -e "\033[1;36m[gateway-deploy]\033[0m $*"; }
die() { echo -e "\033[1;31m[gateway-deploy]\033[0m 错误: $*" >&2; exit 1; }

# 0) 先做单元测试(可选跳过)
if [ "$SKIP_TEST" != "1" ]; then
    log "[0/5] go test ./...(全量,失败则中止部署)"
    if ! (cd backend && go test ./... >/tmp/gw-test.log 2>&1); then
        echo "--- 测试失败,中止。日志 /tmp/gw-test.log ---" >&2
        tail -20 /tmp/gw-test.log >&2
        die "单元测试失败,不部署。请先修代码。"
    fi
    log "   测试通过"
fi

# 1) 备份当前可用二进制
log "[1/5] 备份当前 gateway → bin/backups/"
./scripts/gateway-backup.sh || die "备份失败,中止"

# 2) 编译新二进制(到 .new,不覆盖正在运行的)
log "[2/5] 编译新二进制 → $NEWBIN"
rm -f "$NEWBIN"
if ! (cd backend && go build -trimpath -ldflags "-s -w" -o "../$NEWBIN" ./cmd/gateway); then
    die "编译失败,中止(保留旧 bin/gateway 不变)"
fi
ls -1sh "$NEWBIN"

# 3) 用新二进制替换 bin/gateway(旧 bin/gateway 已被 step1 备份)
log "[3/5] 部署新二进制 → bin/gateway"
mv "$NEWBIN" "$BIN"
chmod 755 "$BIN"

# 4) 触发重启
OLD_PID="$(systemctl show -p MainPID --value "$SVC" 2>/dev/null)"
log "[4/5] 触发 systemd 重启(旧 PID=$OLD_PID)..."
[ -n "$OLD_PID" ] && [ "$OLD_PID" != "0" ] && kill "$OLD_PID" 2>/dev/null || true

# 5) 等 PID 变化 + healthz
log "[5/5] 等待新进程就绪 + healthz..."
NEW_PID=""
for i in $(seq 1 30); do
    NEW_PID="$(systemctl show -p MainPID --value "$SVC" 2>/dev/null)"
    if systemctl is-active --quiet "$SVC" 2>/dev/null && \
       [ -n "$NEW_PID" ] && [ "$NEW_PID" != "0" ] && [ "$NEW_PID" != "$OLD_PID" ] && \
       curl -s -m 2 "http://127.0.0.1:${PORT}/healthz" >/dev/null 2>&1; then
        log "✔ 新版本已部署并健康 (PID $NEW_PID)"
        log "   新二进制: $BIN ; 回滚备份: $(ls -1t "$BACKUP_DIR"/gateway.* | head -1)"
        exit 0
    fi
    sleep 1
done

# ====== 走到这里 ====== 新版本没起来或有异常 → 自动回滚
log "✗ 新版本未就绪(最终 PID=${NEW_PID:-无})。触发自动回滚..."
if ./scripts/gateway-rollback.sh; then
    log "已自动回滚到备份,网关恢复。请检查新代码问题再重试部署。"
    exit 2
else
    die "回滚也失败了!请手动执行 ./scripts/gateway-rollback.sh --list 挑选最早备份恢复。"
fi
