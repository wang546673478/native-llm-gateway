#!/usr/bin/env bash
# pg-init.sh — 本机 PostgreSQL 初始化:建 gateway 角色 + 生产库 + 测试库
#
# 用法(需要 root/sudo):
#   sudo bash scripts/pg-init.sh
#
# 密码:hex 随机生成(无 URL 特殊字符),写入
#   /home/hhhh/llm-gateway-data/pg-password.txt(0600,仅当前用户可读)
#
# 幂等:重复运行会重置密码、保留已建库。
# 测试库 gateway_test 专供集成测试(PG_TEST_DSN),测试会 DROP SCHEMA。
set -euo pipefail

DATA_DIR="${GATEWAY_DATA_DIR:-/home/hhhh/llm-gateway-data}"
mkdir -p "$DATA_DIR"

# 随机 16 字节 hex 密码(openssl 不可用时回退 /dev/urandom)
if command -v openssl >/dev/null 2>&1; then
    PGPW=$(openssl rand -hex 16)
else
    PGPW=$(od -An -tx1 -N16 /dev/urandom | tr -d ' \n')
fi

if sudo -u postgres psql -tAc "SELECT 1 FROM pg_roles WHERE rolname='gateway'" | grep -q 1; then
    sudo -u postgres psql -v ON_ERROR_STOP=1 -c "ALTER ROLE gateway PASSWORD '$PGPW';" >/dev/null
    echo "[pg-init] 角色 gateway 已存在,密码已重置"
else
    sudo -u postgres psql -v ON_ERROR_STOP=1 -c "CREATE ROLE gateway LOGIN PASSWORD '$PGPW';" >/dev/null
    echo "[pg-init] 角色 gateway 已创建"
fi
sudo -u postgres createdb -O gateway gateway 2>/dev/null || echo "[pg-init] 库 gateway 已存在,跳过"
sudo -u postgres createdb -O gateway gateway_test 2>/dev/null || echo "[pg-init] 库 gateway_test 已存在,跳过"

echo "$PGPW" > "$DATA_DIR/pg-password.txt"
chmod 600 "$DATA_DIR/pg-password.txt"
echo "[pg-init] 完成 — 密码已保存 $DATA_DIR/pg-password.txt (0600)"
