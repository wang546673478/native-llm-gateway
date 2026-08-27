-- 孤儿归属数据预览（只读，不改任何数据）
-- 活面来源三方并集：config.yaml 内建面 + providers 表 + relay_stations（全 single 模式，面名=站名）
-- 用法：psql "$DSN" -f scripts/orphan-faces-preview.sql

\set ON_ERROR_STOP on

-- 活面清单（显式枚举，不用模糊匹配 —— 前缀 LIKE 会把 tokenmarket-cc 误判成活面）
CREATE TEMP TABLE live_faces(face text PRIMARY KEY);
INSERT INTO live_faces VALUES
  -- config.yaml 内建面
  ('deepseek'), ('deepseek-anthropic'),
  ('minimax'), ('minimax-openai'),
  ('mimo'), ('mimo-anthropic'), ('mimo-token-plan'), ('mimo-token-plan-anthropic'),
  -- providers 表
  ('tokenmarket'),
  -- relay_stations（11 个）
  ('tokenmarket-codex'), ('tokenmarket-codex1'),
  ('tokenmarket-kiro'), ('tokenmarket-kiro2'), ('tokenmarket-kiro4'),
  ('tokenmarket-plus'), ('tokenmarket-plus3'),
  ('tokenmarket-pro'), ('tokenmarket-pro2'), ('tokenmarket-pro3'), ('tokenmarket-pro+plus');

-- 活 vendor = 活面对应的 vendor（一个 vendor 可带多面，如 mimo → mimo + mimo-token-plan）
CREATE TEMP TABLE live_vendors(vendor text PRIMARY KEY);
INSERT INTO live_vendors
  SELECT DISTINCT f.vendor FROM provider_model_faces f JOIN live_faces l ON l.face = f.face;

\echo '=== [1] 活面自检：每个活面都应有归属行（0 行 = 该面未同步过，属正当情形，不清理）==='
SELECT l.face,
       (SELECT count(*) FROM provider_model_faces f WHERE f.face = l.face) AS face_rows
FROM live_faces l ORDER BY 2, 1;

\echo ''
\echo '=== [2] 待删孤儿面（provider_model_faces）==='
SELECT f.face, f.vendor, count(*) AS rows, min(f.synced_at) AS synced_at
FROM provider_model_faces f
WHERE f.face NOT IN (SELECT face FROM live_faces)
GROUP BY 1,2 ORDER BY 3 DESC, 1;

\echo ''
\echo '=== [3] 待删孤儿面里的具体模型（确认没有正在用的）==='
SELECT f.face, string_agg(f.model_id, ', ' ORDER BY f.sort_order) AS models
FROM provider_model_faces f
WHERE f.face NOT IN (SELECT face FROM live_faces)
GROUP BY 1 ORDER BY 1;

\echo ''
\echo '=== [4] 待删孤儿 vendor（provider_models 定价行）==='
SELECT m.vendor, count(*) AS rows
FROM provider_models m
WHERE m.vendor NOT IN (SELECT vendor FROM live_vendors)
GROUP BY 1 ORDER BY 2 DESC, 1;

\echo ''
\echo '=== [5] 总计 ==='
SELECT
  (SELECT count(*) FROM provider_model_faces WHERE face NOT IN (SELECT face FROM live_faces)) AS face_rows_to_delete,
  (SELECT count(*) FROM provider_models     WHERE vendor NOT IN (SELECT vendor FROM live_vendors)) AS model_rows_to_delete,
  (SELECT count(*) FROM provider_model_faces) AS face_rows_total,
  (SELECT count(*) FROM provider_models)      AS model_rows_total;

\echo ''
\echo '=== [6] 保留行自检（删除后应剩这些）==='
SELECT count(DISTINCT face) AS live_faces_with_rows,
       count(*) AS rows_kept
FROM provider_model_faces WHERE face IN (SELECT face FROM live_faces);

\echo ''
\echo '=== [7] 近 7 天有流量的 provider 是否全在活面里（防误删在用面）==='
SELECT a.provider_name,
       count(*) AS reqs,
       (a.provider_name IN (SELECT face FROM live_faces)) AS is_live_face
FROM access_logs a
WHERE a.created_at > now() - interval '7 days'
  AND COALESCE(a.provider_name,'') <> ''
GROUP BY 1 ORDER BY 3, 2 DESC;
