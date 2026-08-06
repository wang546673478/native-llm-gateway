# provider-vendor skill Step 0 改造实施计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 把 provider-vendor skill 的 Step 0 从「自研 /llms.txt 硬搜」改为「用户提供官方文档 URL 驱动的调研流程」,落地到指南文档和 SKILL.md。

**Architecture:** 纯文档改动,零代码改动。详细流程(0.1~0.5 + 提取清单 6 类 + 完成度标准)写进 `docs/provider厂商定制包指南.md` 的 Step 0 章节;`SKILL.md` 保持薄,只更新执行顺序速记、检查点、常见坑。内容来源 = 已 commit 的 spec `docs/superpowers/specs/2026-08-06-provider-vendor-step0-design.md`(commit 5b0f443)。

**Tech Stack:** Markdown 文档,无代码。

## Global Constraints

- 官方文档是**唯一权威来源**;用户只提供官方文档 URL,不存在自研兜底来源
- 用户提供的必须是 URL;非 URL → 要求重新提供;URL 拉取失败(404/非官方域名)→ 请用户换一个
- 遍历只在用户给的官方文档站内(WebFetch 入口 + WebSearch 站内定位)
- 产出物 = 对话内速查表 + 分散落点(config.yaml cost 字段 / 包 header 注释 / config 块),**不留独立调研文件**
- 官方文档与现有 config/文档冲突 → 标差异再动,不静默覆盖
- 不碰注册/balancer/config 的任何代码机制(本计划只改两个文档)

---

### Task 1:重写指南文档 Step 0 章节

**Files:**
- Modify: `docs/provider厂商定制包指南.md:26-33`(现有「Step 0:调研官方文档」章节)

**Interfaces:**
- Consumes: spec §3(流程)、§4(提取清单)、§5(完成度标准)
- Produces: 指南的 Step 0 章节 = 后续写包步骤(Step 1~6)的输入标准

- [ ] **Step 1:替换现有 Step 0 章节**

把第 26-33 行:

```markdown
### Step 0:调研官方文档(先做,防返工)

拉文档站 `/llms.txt` 全量索引,逐项确认四件事:

1. **模型 ID 与能力**:模型页(思考模式 / 工具调用 / 上下文长度)
2. **Anthropic/Claude 兼容面**:很多厂商有 anthropic 端点但藏在「Claude API 兼容」章节 —— **必须 `grep -i "anthropic\|claude"` 索引**,别只按关键词页碰运气(真实教训:GLM 的 Claude API 兼容页在索引第 117 行,漏查导致第一版只有 openai 面)
3. **Responses API**:支持 → `responses_api: true` + `ResponsesPath`;不支持 → false(Codex 请求不会路由到它)
4. **余额查询接口**:有 → 写 balancer;没有 → 不写(qwen/gemini/glm 同款,api 计费)
```

替换为:

```markdown
### Step 0:调研官方文档(先做,防返工)

**官方文档是唯一权威来源** — 用户只提供官方文档 URL,不依赖搜索镜像/二手信息。

**0.1 确认厂商**:请求未点明厂商时(如「加一个新厂商」)先问清是哪家;已点明(如「加 kimi」)跳过。

**0.2 要官方文档 URL**:
- 用户给的必须是 URL;不是 URL(如「去搜 XX 官网」)→ 要求重新提供
- URL 拉取失败(404 / 非官方域名)→ 请用户换一个

**0.3 遍历官方文档站**(只在用户给的站内):
- 入口:优先拉 `/llms.txt` 全量索引(抓不到就抓首页)
- WebSearch 限定站内定位章节
- **必须 `grep -i "anthropic\|claude"` 索引** —— anthropic 兼容面常藏在「Claude API 兼容」章节(真实教训:GLM 的 Claude API 兼容页在索引第 117 行,漏查导致第一版只有 openai 面)

**0.4 提取 6 类信息**(对话内速查表,每项标消费方):

| # | 提取项 | 消费方 |
|---|--------|--------|
| ① 协议面 | openai/anthropic base URL、Responses 支持与路径(端点是否已含 `/v1`)、鉴权方式 | ChatPath/ResponsesPath、config 块、`responses_api` |
| ② 模型与能力 | 真实模型 ID、上下文窗口(512k 悬崖)、思考模式(默认开/关、effort、thinking 参数名)、工具调用(带 tools 的回传要求)、流式格式、JSON output 触发条件 | config `models[].id`、`default_model`、包代码 |
| ③ 定价 | input/output 单价(**单位换算**:元/M → ÷1000 得 `cost_per_1k`)、缓存 read/creation 有无与数值、缓存计费语义(`prompt_tokens` 含不含 cached)、峰谷价 | `cost_per_1k_input/output/cache_read/cache_creation` |
| ④ 余额 | 官方余额 API 端点与响应字段;没有 → 替代方案(如未文档化 token_plan);额度错误藏 200 body? | `balancer.go`、`RegisterBalancer` |
| ⑤ 定制特性 | 响应包裹格式(base_resp)、reasoning 字段名与回传规则、缓存机制差异、厂商专属参数(service_tier 等)、429 语义(套餐耗尽 vs 真限流) | 包 header 注释 |
| ⑥ 入口 | `/llms.txt` 索引、模型表/定价表/协议章节各自 URL | 遍历路线 |

**0.5 差异标注**:官方文档与现有 config/文档冲突(如 MiniMax 旧域名)时,标出差异再动,不静默覆盖。

**完成度标准**(全部满足 = 调研完,进入 Step 1):

| 项 | 标准 |
|---|---|
| 协议 | 每个面 base URL + 路径确认,能写出 ChatPath/ResponsesPath |
| 模型 | ≥1 个真实可用模型 ID(填 `default_model`) |
| 价格 | input/output 单价确认;文档没给 → 显式标「无定价 → cost 缺省 0」,不是跳过 |
| 特性 | 能写出完整 header 注释清单 |
| 余额 | 有官方 API / 无(用替代)/ 文档未提及 —— 三选一显式结论 |
| 未知项 | 标「文档未提及」≠「没有」,写代码时按最保守处理 |
```

- [ ] **Step 2:验证替换**

Run: `grep -n "0.1 确认厂商\|0.2 要官方文档 URL\|0.3 遍历官方文档站\|0.4 提取\|0.5 差异标注\|完成度标准" docs/provider厂商定制包指南.md`
Expected: 6 行全部命中,且位于 Step 0 章节内;`grep -n "拉文档站.*llms" docs/provider厂商定制包指南.md` 不再命中旧措辞。

- [ ] **Step 3:Commit**

```bash
git add docs/provider厂商定制包指南.md
git commit -m "docs(guide): Step 0 改为用户提供官方文档 URL 驱动的调研流程

- 0.1~0.5 流程:确认厂商 → 要 URL(非 URL 重新提供)→ 站内遍历 → 速查表分散落点 → 差异标注
- 提取清单 6 类,每项标注消费方;价格单位换算(元/M → cost_per_1k)
- 完成度标准:6 项「调研完」判据

Co-Authored-By: Claude <noreply@anthropic.com>"
```

---

### Task 2:更新 SKILL.md(执行顺序 / 检查点 / 常见坑)

**Files:**
- Modify: `.claude/skills/provider-vendor/SKILL.md:19-20`(执行顺序第 0 条)
- Modify: `.claude/skills/provider-vendor/SKILL.md:33`(检查点第 1 条)
- Modify: `.claude/skills/provider-vendor/SKILL.md:52`(常见坑最后一条后追加 2 条)

**Interfaces:**
- Consumes: Task 1 写好的指南 Step 0 章节(速记要与之一致)
- Produces: 触发 skill 时加载的执行速记

- [ ] **Step 1:替换执行顺序第 0 条**

把第 19-20 行:

```markdown
0. **调研官方文档**(先做,防返工):拉文档站 `/llms.txt` 全量索引,逐项确认四件事 —
   ① 模型 ID 与能力(思考模式 / 工具调用)② **Anthropic/Claude 兼容面**(有没有 anthropic 端点——很多厂商有但藏在「Claude API 兼容」章节,必须 grep `anthropic|claude`,别只按关键词碰运气)③ Responses API 支持 ④ 余额查询接口
```

替换为:

```markdown
0. **调研官方文档**(先做,防返工):官方文档是唯一权威,**用户提供 URL** — ① 确认厂商(请求未点明时先问)② 要官方文档 URL(非 URL → 重新提供;拉取失败 → 换)③ 站内遍历(`/llms.txt` 入口 + grep `anthropic|claude`)④ 提取 6 类(协议面 / 模型能力 / 定价含单位换算 / 余额 / 定制特性 / 入口)⑤ 与现有 config 冲突 → 标差异再动
```

- [ ] **Step 2:替换检查点第 1 条**

把第 33 行:

```markdown
- [ ] 调研过官方文档四件事:模型 ID / anthropic 兼容面 / responses / 余额(grep `anthropic|claude`,不只看关键词页)
```

替换为:

```markdown
- [ ] 厂商已确认;官方文档 URL 来自用户(非 URL 已要求重新提供、拉取失败已换);6 类提取项全覆盖:协议面 / 模型能力 / 定价(含单位换算)/ 余额 / 定制特性 / 入口(grep `anthropic|claude`);与现有 config 冲突已标差异
```

- [ ] **Step 3:常见坑追加 2 条**

在第 52 行末尾(文档调研漏 anthropic 兼容章节那条)之后追加:

```markdown
- 用户给的文档不是 URL / URL 拉取失败 → 先要求重新提供,别自己搜替代来源(官方文档是唯一权威)
- 价格单位没换算 → 文档 0.42 元/M 直接抄进 `cost_per_1k`,价格差 1000 倍(÷1000 才进 config)
```

- [ ] **Step 4:验证**

Run: `grep -n "用户提供 URL\|6 类提取项\|单位换算\|唯一权威" .claude/skills/provider-vendor/SKILL.md`
Expected: 至少命中 3 处;确认执行顺序/检查点/坑三处的措辞与指南 Step 0 一致(不出现旧措辞「逐项确认四件事」)。

- [ ] **Step 5:Commit**

```bash
git add .claude/skills/provider-vendor/SKILL.md
git commit -m "docs(skill): provider-vendor Step 0 速记同步 — 用户提供官方文档 URL 驱动

- 执行顺序第 0 条:确认厂商 → 要 URL → 站内遍历 → 6 类提取 → 差异标注
- 检查点第 1 条覆盖新流程;常见坑补 URL 校验与价格单位换算两条

Co-Authored-By: Claude <noreply@anthropic.com>"
```

---

### Task 3:干跑验证新 Step 0

**Files:**
- 无文件改动(只读验证)

**Interfaces:**
- Consumes: Task 1+2 的最终文档
- Produces: 验证结论 — 新 Step 0 流程可执行,速查表产出形态可用

- [ ] **Step 1:确认文档一致性**

Run: `grep -c "0.1\|0.2\|0.3\|0.4\|0.5" docs/provider厂商定制包指南.md && grep -n "官方文档是唯一权威" docs/provider厂商定制包指南.md .claude/skills/provider-vendor/SKILL.md`
Expected: 两处文档都有「唯一权威」表述,指南含 0.1~0.5 全部锚点。

- [ ] **Step 2:模拟 0.2~0.4 干跑(不注册厂商、不改代码)**

用 WebFetch 拉一个真实厂商官方文档站,模拟用户给的 URL,走一遍 0.3 入口 + 0.4 提取,产出一行速查表示例:

Run: `WebFetch("https://api-docs.deepseek.com/llms.txt" 或抓不到时首页 "https://api-docs.deepseek.com/", prompt="列出:OpenAI 兼容端点、Anthropic 兼容端点(如有)、Responses API 支持、模型 ID 列表、输入输出单价")`
Expected: 产出至少覆盖 ① 协议面 ③ 定价 ② 模型三类的速查表行(条目不齐也没关系——正是完成度标准里的「标未提及」场景)。若该站无 `/llms.txt`,改用首页,并记下「该站无 llms.txt → 抓首页」作为 0.3 fallback 路径实测通过。

- [ ] **Step 3:汇报干跑结论**

在对话中汇报:速查表示例、0.3 fallback 是否触发、完成度标准是否满足(哪几项达成、哪几项标「未提及」)。不 commit(无文件改动)。
