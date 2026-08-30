#!/bin/bash
# sync-provider-models.sh
# 自动同步模型配置从一个 provider face 到另一个 face
#
# 用法:
#   ./sync-provider-models.sh <source-face> <target-face>
#
# 示例（使用实际 face 名替换占位符）:
#   ./sync-provider-models.sh <source-face> <target-face>

set -e

# 数据库连接配置
DB_HOST="${DB_HOST:-127.0.0.1}"
DB_PORT="${DB_PORT:-5432}"
DB_USER="${DB_USER:-gateway}"
DB_NAME="${DB_NAME:-gateway}"
DB_PASSWORD="${DB_PASSWORD:-}"

# 颜色输出
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
NC='\033[0m' # No Color

if [ -z "$DB_PASSWORD" ]; then
    echo "错误: 必须通过环境变量 DB_PASSWORD 提供数据库密码" >&2
    exit 1
fi

# 检查参数
if [ $# -ne 2 ]; then
    echo -e "${RED}错误: 需要提供源 face 和目标 face${NC}"
    echo ""
    echo "用法: $0 <source-face> <target-face>"
    echo ""
    echo "示例（使用实际 face 名替换占位符）:"
    echo "  $0 <source-face> <target-face>"
    echo ""
    exit 1
fi

SOURCE_FACE="$1"
TARGET_FACE="$2"

echo -e "${BLUE}=== Provider 模型配置同步工具 ===${NC}"
echo ""
echo -e "源 face:    ${GREEN}${SOURCE_FACE}${NC}"
echo -e "目标 face:  ${GREEN}${TARGET_FACE}${NC}"
echo ""

# 检查源 face 是否存在
SOURCE_COUNT=$(PGPASSWORD=$DB_PASSWORD psql -h $DB_HOST -p $DB_PORT -U $DB_USER -d $DB_NAME -t -c \
    "SELECT COUNT(*) FROM provider_model_faces WHERE face = '${SOURCE_FACE}';")

if [ "$SOURCE_COUNT" -eq 0 ]; then
    echo -e "${RED}错误: 源 face '${SOURCE_FACE}' 不存在或没有模型配置${NC}"
    exit 1
fi

echo -e "${YELLOW}源 face 有 ${SOURCE_COUNT} 个模型${NC}"
echo ""

# 检查目标 face 的 provider 是否存在
TARGET_PROVIDER_EXISTS=$(PGPASSWORD=$DB_PASSWORD psql -h $DB_HOST -p $DB_PORT -U $DB_USER -d $DB_NAME -t -c \
    "SELECT COUNT(*) FROM providers WHERE name = '${TARGET_FACE}';")

if [ "$TARGET_PROVIDER_EXISTS" -eq 0 ]; then
    echo -e "${RED}警告: 目标 provider '${TARGET_FACE}' 不存在${NC}"
    echo -e "${YELLOW}请先在 providers 表中创建该 provider${NC}"
    echo ""
    read -p "是否继续同步模型配置? (y/N) " -n 1 -r
    echo
    if [[ ! $REPLY =~ ^[Yy]$ ]]; then
        echo "已取消"
        exit 0
    fi
fi

# 显示将要同步的模型列表
echo -e "${BLUE}=== 源 face 的模型列表 ===${NC}"
PGPASSWORD=$DB_PASSWORD psql -h $DB_HOST -p $DB_PORT -U $DB_USER -d $DB_NAME -c \
    "SELECT model_id, sort_order FROM provider_model_faces WHERE face = '${SOURCE_FACE}' ORDER BY sort_order;"
echo ""

# 确认操作
read -p "确认要同步这些模型到 '${TARGET_FACE}'? (y/N) " -n 1 -r
echo
if [[ ! $REPLY =~ ^[Yy]$ ]]; then
    echo "已取消"
    exit 0
fi

# 执行同步
echo ""
echo -e "${BLUE}=== 开始同步 ===${NC}"

PGPASSWORD=$DB_PASSWORD psql -h $DB_HOST -p $DB_PORT -U $DB_USER -d $DB_NAME <<EOF
-- 删除目标 face 的现有模型配置
DELETE FROM provider_model_faces WHERE face = '${TARGET_FACE}';

-- 从源 face 复制所有模型到目标 face
INSERT INTO provider_model_faces (vendor, face, model_id, sort_order, synced_at, created_at, updated_at)
SELECT
    vendor,
    '${TARGET_FACE}' as face,
    model_id,
    sort_order,
    NOW() as synced_at,
    NOW() as created_at,
    NOW() as updated_at
FROM provider_model_faces
WHERE face = '${SOURCE_FACE}'
ORDER BY sort_order;
EOF

# 显示同步结果
echo ""
echo -e "${GREEN}=== 同步完成 ===${NC}"
echo ""
echo -e "${BLUE}目标 face 的模型列表:${NC}"
PGPASSWORD=$DB_PASSWORD psql -h $DB_HOST -p $DB_PORT -U $DB_USER -d $DB_NAME -c \
    "SELECT model_id, sort_order FROM provider_model_faces WHERE face = '${TARGET_FACE}' ORDER BY sort_order;"

# 显示统计
echo ""
TARGET_COUNT=$(PGPASSWORD=$DB_PASSWORD psql -h $DB_HOST -p $DB_PORT -U $DB_USER -d $DB_NAME -t -c \
    "SELECT COUNT(*) FROM provider_model_faces WHERE face = '${TARGET_FACE}';")

echo -e "${GREEN}✓ 已同步 ${TARGET_COUNT} 个模型从 '${SOURCE_FACE}' 到 '${TARGET_FACE}'${NC}"
