#!/usr/bin/env bash
# gateway-backup.sh — 编译新二进制前,把「当前正在运行、已知可用」的 gateway 备份
#
# 用途:配合 gateway-rollback.sh 使用。本脚本在每次 make build 替换 bin/gateway 之前
#       手动调用(或由编译流程自动调用),把当前 bin/gateway 物理拷贝为带时间戳的备份。
#       这样即使我(agent)改代码改到一半、或新二进制有问题起不来,你随时能用
#       gateway-rollback.sh 一键复原到最后一个已知可用的二进制,网关不断 → 我还能继续干。
#
# 为什么物理拷贝而非 git:bin/gateway 是我「最后一次成功编译并部署」的产物;而 git
#       工作区可能正被我改到一半(未编译/未测),不一定能 reload 回可运行状态。
#       物理备份保证即使仓库坏掉,回滚脚本也能瞬时恢复一个确定能用的二进制。
#
# 设计点:
#   - 备份命名 gateway.<ts>,按时间倒序,回滚脚本取最新。
#   - 只在新二进制替换前备份「已有可运行产物」;gateway 只备份这个单体二进制。
#   - 自动保留最近 N 份(N=5),避免攒太多占盘。
#
# 用法:  ./scripts/gateway-backup.sh
# 退出码:0 成功(含"无二进制可备份"视为成功);1 失败

set -euo pipefail
cd "$(dirname "$0")/.."   # 仓库根

BIN="bin/gateway"
BACKUP_DIR="bin/backups"
MAX_KEEP=5

if [ ! -f "$BIN" ]; then
    echo "gateway-backup: 无 $BIN 可备份(可能还没编译过),跳过。"
    exit 0
fi

mkdir -p "$BACKUP_DIR"
TS="$(date +%Y%m%d-%H%M%S)"
DEST="$BACKUP_DIR/gateway.$TS"

# 已存在同秒备份则不再重复(极难触发,防呆)
if [ -e "$DEST" ]; then
    echo "gateway-backup: $DEST 已存在,跳过。"
else
    cp -p "$BIN" "$DEST"
fi

echo "gateway-backup: 已备份 → $DEST ($(du -h "$DEST" | cut -f1))"

# 按修改时间从新到旧排序，只删除第 MAX_KEEP 份之后的时间戳备份。
# pre-rollback 文件不属于本脚本的轮换集合。
mapfile -t backups < <(
    find "$BACKUP_DIR" -maxdepth 1 -type f -name 'gateway.[0-9]*' -printf '%T@ %p\n' \
        | sort -nr | cut -d' ' -f2-
)
for ((i = MAX_KEEP; i < ${#backups[@]}; i++)); do
    rm -f -- "${backups[$i]}"
    echo "gateway-backup: 清理旧备份 → ${backups[$i]}"
done

echo "gateway-backup: 完成(保留最近 $MAX_KEEP 份)。"
