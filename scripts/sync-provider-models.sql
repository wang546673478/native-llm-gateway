-- sync-provider-models.sql
-- 自动同步模型配置从一个 provider face 到另一个 face
-- 用法: psql -h 127.0.0.1 -U gateway -d gateway -v source=tokenmarket -v target=tokenmarket-codex -f sync-provider-models.sql

-- 显示将要同步的模型
\echo '=== 源 face 的模型列表 ==='
SELECT vendor, face, model_id, sort_order
FROM provider_model_faces
WHERE face = :'source'
ORDER BY sort_order;

\echo ''
\echo '=== 开始同步 ==='

-- 删除目标 face 的现有模型配置
DELETE FROM provider_model_faces
WHERE face = :'target';

-- 从源 face 复制所有模型到目标 face
INSERT INTO provider_model_faces (vendor, face, model_id, sort_order, synced_at, created_at, updated_at)
SELECT
    vendor,
    :'target' as face,  -- 使用目标 face 名称
    model_id,
    sort_order,
    NOW() as synced_at,
    NOW() as created_at,
    NOW() as updated_at
FROM provider_model_faces
WHERE face = :'source'
ORDER BY sort_order;

-- 显示同步结果
\echo ''
\echo '=== 同步完成 ==='
\echo '目标 face 的模型列表:'
SELECT vendor, face, model_id, sort_order
FROM provider_model_faces
WHERE face = :'target'
ORDER BY sort_order;

-- 显示统计
\echo ''
\echo '=== 统计信息 ==='
SELECT
    :'source' as source_face,
    COUNT(*) as source_count
FROM provider_model_faces
WHERE face = :'source'
UNION ALL
SELECT
    :'target' as target_face,
    COUNT(*) as target_count
FROM provider_model_faces
WHERE face = :'target';
