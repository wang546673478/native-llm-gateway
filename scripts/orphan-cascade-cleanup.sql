-- 孤儿清理:route_order / provider_api_keys
--
-- 背景:这两张表用**普通字符串列**指向 provider/面名,不是外键。厂商或中转站被
-- 硬删后行会留下,而且不是无害残留:
--   route_order        scope=provider 的孤儿仍占着层内 seq 名次,把活着的候选整体
--                      往后挤(实测已删的 claude-aws / codex 占了 api 层 seq 0/1
--                      两个最高优先级位)。
--   provider_api_keys  在「上游 Key」页显示成幽灵条目(用户实测:界面找不到站
--                      却能查到 codex/claude-aws),且上游 key 明文无限期留库。
--
-- 本脚本只清历史欠账。防复发在代码里:deleteRelayStation 已级联调用
-- RouteOrderStore.DeleteByProvider / ProviderKeyStore.DeleteByProvider。
--
-- 用法(务必先备份,见下方 BACKUP 段):
--   psql ... -f scripts/orphan-cascade-cleanup.sql
-- 脚本末尾是 COMMIT(已于 2026-08-28 执行)。想先预览就临时改成 ROLLBACK。

\set ON_ERROR_STOP on

BEGIN;

-- ── live_providers:活着的 provider/面名全集 ──────────────────────────────
-- 三个权威来源合并,任何一个漏掉都会误删活数据:
--   ① config.yaml 内建厂商(不在 DB 里!必须手工同步 —— 见下方守卫 G1)
--   ② providers 表(vendor 级)
--   ③ relay_stations 表(single → 站名;multi → 站名-协议,与 relay.FaceNames 对齐)
CREATE TEMP TABLE live_providers(name text PRIMARY KEY) ON COMMIT DROP;

-- ① config.yaml 内建厂商(2026-08-28 快照,与 `awk` 抽取的 providers: 顶层键一致)
INSERT INTO live_providers(name) VALUES
  ('deepseek'), ('deepseek-anthropic'),
  ('minimax'), ('minimax-openai'),
  ('mimo'), ('mimo-anthropic'),
  ('mimo-token-plan'), ('mimo-token-plan-anthropic')
ON CONFLICT DO NOTHING;

-- ② providers 表
INSERT INTO live_providers(name)
SELECT name FROM providers WHERE name <> '' ON CONFLICT DO NOTHING;

-- ③ relay_stations:站名 + multi 模式的面名
--    站名恒加(syncRelayStationKeys 按站名写 provider_api_keys)
INSERT INTO live_providers(name)
SELECT name FROM relay_stations WHERE name <> '' ON CONFLICT DO NOTHING;
--    multi 模式再展开 name-协议(与 relay.FaceNames 的 multi 分支对齐)。
--    两个分支分开写而不是塞进 CASE:集合返回函数不能出现在 CASE 里
--    (PG: "set-returning functions are not allowed in CASE")。
--    分支 a:supported_protocols 有内容 → 逐协议展开
INSERT INTO live_providers(name)
SELECT s.name || '-' || proto
FROM relay_stations s,
     jsonb_array_elements_text(s.supported_protocols::jsonb) AS proto
WHERE s.protocol_mode = 'multi'
  AND s.name <> ''
  AND coalesce(s.supported_protocols, '') NOT IN ('', '[]')
ON CONFLICT DO NOTHING;
--    分支 b:supported_protocols 空 → 退回主协议(parseSupportedProtocols 同逻辑)
INSERT INTO live_providers(name)
SELECT s.name || '-' || s.primary_protocol
FROM relay_stations s
WHERE s.protocol_mode = 'multi'
  AND s.name <> ''
  AND coalesce(s.supported_protocols, '') IN ('', '[]')
ON CONFLICT DO NOTHING;

-- ── 守卫 G1:live 集不能可疑地小 ─────────────────────────────────────────
-- 这是最重要的一道闸。真实近失:第一版孤儿清单曾把**活着的** minimax-openai /
-- mimo / mimo-token-plan 算成孤儿(只查了 DB 两张表、漏了 config.yaml 内建),
-- 靠网关日志显示它们正在服务流量才发现。宁可脚本报错停下,不可静默删活数据。
DO $$
DECLARE n int;
BEGIN
  SELECT count(*) INTO n FROM live_providers;
  IF n < 12 THEN
    RAISE EXCEPTION 'G1 守卫:live_providers 只有 % 个,疑似来源缺失(应 ≥ 8 内建 + 3 providers + 11 中转站去重后 ~19)。请检查 config.yaml 内建清单是否同步。', n;
  END IF;
END $$;

-- ── 守卫 G2:不能删光整表 ───────────────────────────────────────────────
DO $$
DECLARE total int; orphan int;
BEGIN
  SELECT count(*) INTO total FROM route_order;
  SELECT count(*) INTO orphan FROM route_order o
   WHERE (o.scope = 'provider' AND o.name   NOT IN (SELECT name FROM live_providers))
      OR (o.scope = 'key'      AND o.provider NOT IN (SELECT name FROM live_providers));
  IF total > 0 AND orphan = total THEN
    RAISE EXCEPTION 'G2 守卫:route_order 全部 % 行都判为孤儿,live 集必然有问题,中止。', total;
  END IF;
  RAISE NOTICE 'route_order: 总 % 行,孤儿 % 行', total, orphan;

  SELECT count(*) INTO total FROM provider_api_keys;
  SELECT count(*) INTO orphan FROM provider_api_keys k
   WHERE k.provider_name NOT IN (SELECT name FROM live_providers);
  IF total > 0 AND orphan = total THEN
    RAISE EXCEPTION 'G2 守卫:provider_api_keys 全部 % 行都判为孤儿,中止。', total;
  END IF;
  RAISE NOTICE 'provider_api_keys: 总 % 行,孤儿 % 行', total, orphan;
END $$;

-- ── 守卫 G3:NOT IN 的 NULL 陷阱 ────────────────────────────────────────
-- live_providers 里出现 NULL 会让 `NOT IN` 恒为 UNKNOWN → 一行都删不掉(静默无效)。
DO $$
DECLARE n int;
BEGIN
  SELECT count(*) INTO n FROM live_providers WHERE name IS NULL;
  IF n > 0 THEN
    RAISE EXCEPTION 'G3 守卫:live_providers 含 NULL,NOT IN 会静默失效,中止。';
  END IF;
END $$;

-- ── BACKUP:先把要删的行打印出来存档 ────────────────────────────────────
-- 注意 provider_api_keys 含上游 key 明文(key_hash 列名是历史遗留,存的是明文)。
-- 备份文件务必 chmod 600,且**不要**把 key_hash 贴进聊天/工单。
\echo '--- 将被删除的 route_order 行 ---'
SELECT id, scope, provider, name, billing_source, seq
FROM route_order o
WHERE (o.scope = 'provider' AND o.name     NOT IN (SELECT name FROM live_providers))
   OR (o.scope = 'key'      AND o.provider NOT IN (SELECT name FROM live_providers))
ORDER BY scope, billing_source, seq;

\echo '--- 将被删除的 provider_api_keys 行(不含明文)---'
SELECT id, provider_name, name, enabled, billing_source, created_at
FROM provider_api_keys k
WHERE k.provider_name NOT IN (SELECT name FROM live_providers)
ORDER BY provider_name, id;

-- ── 删除 ────────────────────────────────────────────────────────────────
DELETE FROM route_order o
WHERE (o.scope = 'provider' AND o.name     NOT IN (SELECT name FROM live_providers))
   OR (o.scope = 'key'      AND o.provider NOT IN (SELECT name FROM live_providers));

DELETE FROM provider_api_keys k
WHERE k.provider_name NOT IN (SELECT name FROM live_providers);

-- ── 复查:删完应为 0 孤儿 ───────────────────────────────────────────────
\echo '--- 删除后残留孤儿(应为 0)---'
SELECT 'route_order' AS tbl, count(*) AS residual FROM route_order o
 WHERE (o.scope = 'provider' AND o.name     NOT IN (SELECT name FROM live_providers))
    OR (o.scope = 'key'      AND o.provider NOT IN (SELECT name FROM live_providers))
UNION ALL
SELECT 'provider_api_keys', count(*) FROM provider_api_keys k
 WHERE k.provider_name NOT IN (SELECT name FROM live_providers);

-- ── seq 连续性说明 ──────────────────────────────────────────────────────
-- 删掉 seq 0/1 后剩下的行 seq 从 2 起,**不需要**重排:ListByScope 只用
-- `ORDER BY seq ASC` 取相对顺序,不要求从 0 连续。下次前端拖拽保存会由
-- Replace 整体重写成 0..n-1。

-- 已于 2026-08-28 执行(备份 backups/orphan-cascade-20260828-034608.sql)。
-- 重跑安全:孤儿判定是幂等的,无孤儿时 DELETE 0 行。
COMMIT;
