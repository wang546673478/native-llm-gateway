-- 孤儿归属数据清理（事务内执行，带行数断言）
-- 前置：先跑 scripts/orphan-faces-preview.sql 确认待删清单
-- 回滚：psql "$DSN" -f backups/orphan-cleanup-<TS>.sql（需先 TRUNCATE 两表）
--
-- 背景：relay_stations 硬删除不级联 —— provider_model_faces.face 与
-- provider_models.vendor 是字符串列而非外键，删站后归属行原地留下。
-- 这些孤儿行不参与路由（Registry 里没有对应面，不会成为候选），
-- 但污染模型管理页并让「无归属」判定失真。

\set ON_ERROR_STOP on

BEGIN;

-- 活面清单（与 preview 脚本保持一致 —— 改一处必须改两处）
CREATE TEMP TABLE live_faces(face text PRIMARY KEY);
INSERT INTO live_faces VALUES
  ('deepseek'), ('deepseek-anthropic'),
  ('minimax'), ('minimax-openai'),
  ('mimo'), ('mimo-anthropic'), ('mimo-token-plan'), ('mimo-token-plan-anthropic'),
  ('tokenmarket'),
  ('tokenmarket-codex'), ('tokenmarket-codex1'),
  ('tokenmarket-kiro'), ('tokenmarket-kiro2'), ('tokenmarket-kiro4'),
  ('tokenmarket-plus'), ('tokenmarket-plus3'),
  ('tokenmarket-pro'), ('tokenmarket-pro2'), ('tokenmarket-pro3'), ('tokenmarket-pro+plus');

CREATE TEMP TABLE live_vendors(vendor text PRIMARY KEY);
INSERT INTO live_vendors
  SELECT DISTINCT f.vendor FROM provider_model_faces f JOIN live_faces l ON l.face = f.face;

-- 守卫 1：活面数必须是 20（清单被改动过就中止）
DO $$
DECLARE n int;
BEGIN
  SELECT count(*) INTO n FROM live_faces;
  IF n <> 20 THEN
    RAISE EXCEPTION '活面清单行数异常: 期望 20, 实际 %', n;
  END IF;
END $$;

-- 守卫 2：不能把所有行都删掉（NOT IN 遇 NULL 会退化成全集）
DO $$
DECLARE to_del int; total int;
BEGIN
  SELECT count(*) INTO total FROM provider_model_faces;
  SELECT count(*) INTO to_del FROM provider_model_faces
    WHERE face NOT IN (SELECT face FROM live_faces);
  IF to_del >= total THEN
    RAISE EXCEPTION '待删行数 % >= 总行数 % — 拒绝执行(疑似清单失效)', to_del, total;
  END IF;
  RAISE NOTICE 'provider_model_faces: 待删 % / 总 %', to_del, total;
END $$;

-- 守卫 3：活面里有归属行的面不能为 0
DO $$
DECLARE n int;
BEGIN
  SELECT count(DISTINCT face) INTO n FROM provider_model_faces
    WHERE face IN (SELECT face FROM live_faces);
  IF n = 0 THEN
    RAISE EXCEPTION '活面归属行为 0 — 拒绝执行';
  END IF;
  RAISE NOTICE '保留活面数: %', n;
END $$;

\echo '=== 删除孤儿归属行 ==='
DELETE FROM provider_model_faces
WHERE face NOT IN (SELECT face FROM live_faces);

\echo '=== 删除孤儿定价行 ==='
DELETE FROM provider_models
WHERE vendor NOT IN (SELECT vendor FROM live_vendors);

\echo ''
\echo '=== 删除后核对 ==='
SELECT
  (SELECT count(*) FROM provider_model_faces) AS face_rows_left,
  (SELECT count(DISTINCT face) FROM provider_model_faces) AS faces_left,
  (SELECT count(*) FROM provider_models) AS model_rows_left,
  (SELECT count(DISTINCT vendor) FROM provider_models) AS vendors_left;

\echo ''
\echo '=== 残留孤儿自检（应为 0 行）==='
SELECT f.face FROM provider_model_faces f
WHERE f.face NOT IN (SELECT face FROM live_faces)
GROUP BY 1;

\echo ''
\echo '=== 关键验证：luna 是否已随 codex 面清除 ==='
SELECT face, model_id FROM provider_model_faces WHERE model_id = 'gpt-5.6-luna';

COMMIT;
