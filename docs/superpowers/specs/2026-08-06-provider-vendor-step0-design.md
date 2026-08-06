# provider-vendor skill Step 0 改造:用户提供官方文档 URL 驱动的调研流程

日期:2026-08-06
状态:已确认(用户逐点确认)

## 1. 背景与动机

`provider-vendor` skill 的 Step 0 现状是「假设已知道厂商 → 自己拉 `/llms.txt` 全量索引硬搜」。问题:

1. **入口不确定**:触发话术为「加一个新厂商」时,厂商是谁、文档站在哪都要猜;盲搜容易命中过期镜像。
2. **来源不单一**:搜索结果与官方文档混杂,口径不统一。
3. **提取目标不全**:现清单只有四件事(模型 ID/能力、Claude 兼容面、Responses、余额),缺「各协议接口地址、API 定价、厂商定制特性」——而这正是写 config 块、balancer、包 header 注释直接消费的信息(见 08-04 调研 B1/B2/B3 的落点)。

真实历史印证:2026-08-04 的全量文档调研(DeepSeek 19 页 / MiniMax 23 页)就是「对着官方文档逐页找」完成的;MiniMax `api.minimax.chat` 旧域名错误也曾靠官方文档修正。

## 2. 用户决策(2026-08-06 确认)

| # | 决策 |
|---|---|
| 1 | **官方文档是唯一权威来源**。用户只是提供官方文档 URL;不是「用户优先 + 自研兜底」,不存在自研检索来源。 |
| 2 | **URL 校验规则**:用户提供的必须是 URL;不是 URL → 要求重新提供。URL 拉取失败(404/非官方域名)→ 请用户换一个。 |
| 3 | **遍历范围**:只在用户给的官方文档站内遍历(WebFetch 入口 + WebSearch 站内定位)。 |
| 4 | **产出物**:对话内速查表 + 分散落点,不留独立调研文件(避免多一处要同步维护的文档)。落点:endpoint/协议 → config.yaml 块 + 包代码;定价 → config.yaml cost 字段;特性 → 包 header 注释(08-04 B3 模式)。 |
| 5 | **差异标注**:官方文档与现有 config/文档冲突时,标出差异再动,不静默覆盖。 |
| 6 | **提取清单**(6 类)与完成度标准按本 spec §4/§5 执行。 |

## 3. 新 Step 0 流程

```
Step 0: 调研官方文档(官方文档是唯一权威,用户提供 URL)

0.1 确认厂商
    └─ 请求已点明(如「加 kimi」)→ 跳过;没点明(如「加一个新厂商」)→ 先问

0.2 要官方文档 URL
    ├─ 必须是 URL,不是 → 重新提供
    └─ URL 拉取失败(404 / 非官方域名)→ 请用户换一个

0.3 遍历(只在用户给的官方文档站内)
    ├─ 入口:优先 /llms.txt 全量索引(抓不到就抓首页)
    ├─ WebSearch 限定站内定位章节
    └─ 必须 grep `anthropic|claude` 找兼容章节(GLM 教训:藏在索引第 117 行
       /cn/guide/develop/claude/ 下,不 grep 必漏)

0.4 产出「对话内速查表」+ 分散落点
    ├─ 协议/接口地址 ──→ config.yaml 块 + 包代码(openai/anthropic/Responses 面)
    ├─ API 定价 ──────→ config.yaml 的 cost_per_1k_input/output/cache_read/cache_creation
    └─ 定制特性 ──────→ 包 header 注释(08-04 B3 模式)

0.5 差异标注
    └─ 官方文档与现有 config/文档冲突时(如 MiniMax 旧域名),标出差异再动,不静默覆盖
```

## 4. 调研提取清单(6 类)

每个提取项都标注消费方,即「写到代码/config 的哪里」。这是写包的原料单。

**① 协议面**(→ 决定注册几个协议面、ChatPath/ResponsesPath 写什么)
- OpenAI 兼容 base URL + 路径(`/v1/chat/completions`)
- Anthropic 兼容 base URL(很多厂商是独立 URL,和 openai 面不同 host)
- Responses API:支不支持;endpoint 是否已含 `/v1`(决定 `ResponsesPath` 写 `/responses` 还是 `/v1/responses`)
- 鉴权方式(Bearer 还是有自定义 header)

**② 模型与能力**(→ config.yaml models 块 + 包代码)
- 真实可用的模型 ID 全集(不是展示名/别名)——填 `models[].id` 和 `default_model`
- 上下文窗口——512k 悬崖影响 ComputeCost multiplier
- 思考模式:默认开/关、`reasoning_effort` 支持、anthropic 面 thinking 参数名
- 工具调用:支持否;带 tools 时是否有特殊回传要求(DeepSeek 必须回传 `reasoning_content` 否则 400,踩坑 #5)
- 流式:SSE 格式、keep-alive 行(DeepSeek 有)
- JSON output:有无触发条件(DeepSeek 要求 prompt 含 "json" 字样)

**③ 定价**(→ config.yaml `cost_per_1k_input/output/cache_read/cache_creation`)
- input/output 单价,**单位换算**(元/M → ÷1000 得 cost_per_1k;`$` 还要汇率)
- 缓存价:read/creation 有没有、分别是多少(M3 无主动写价 vs M2.x 有,B2 教训)
- 缓存计费语义:`prompt_tokens` 含不含 cached 部分(B1 教训,决定解析代码)
- 峰谷定价、特殊计费(DeepSeek 高峰 2 倍预告,注释里标)

**④ 余额/额度**(→ balancer.go + RegisterBalancer)
- 有没有官方余额查询 API:端点、响应字段
- 没有 → 用什么代替(MiniMax 用未文档化的 token_plan/remains,踩坑 #14)
- 额度错误是 HTTP 状态码还是藏在 200 body(base_resp,踩坑 #1)

**⑤ 厂商定制特性**(→ 包 header 注释,B3 模式)
- 响应包裹格式(base_resp、错误码结构)
- reasoning 字段名与回传规则(`reasoning_content` / `<think>` 标签内嵌 content 原样回传)
- 缓存机制差异(主动 cache_control 仅 M2.x / 自动缓存 M3+)
- 厂商专属参数(service_tier、priority、reasoning_split)
- anthropic 面特殊行为(DeepSeek 未知模型名静默映射 flash)
- 429 语义:套餐耗尽 vs 真限流(决定熔断/COOLING 行为)

**⑥ 入口定位**(决定怎么开始遍历)
- `/llms.txt` 全量索引;模型表、定价表、协议章节各自 URL
- 必须 grep `anthropic|claude` 找兼容章节(GLM 教训)

## 5. 完成度标准(提取到什么程度 = 调研完)

| 项 | 标准 |
|---|---|
| 协议 | 每个面 base URL + 路径确认,能写出 ChatPath/ResponsesPath |
| 模型 | ≥1 个真实可用模型 ID(填 default_model) |
| 价格 | input/output 单价确认;文档没给 → 显式标「无定价 → cost 缺省 0」,不是跳过 |
| 特性 | 能写出完整 header 注释清单 |
| 余额 | 有官方 API / 无(用替代)/ 文档未提及——三选一显式结论 |
| 未知项 | 标「文档未提及」≠「没有」,写代码时按最保守处理 |

## 6. 改动文件清单

| 文件 | 改动 |
|---|---|
| `.claude/skills/provider-vendor/SKILL.md` | 执行顺序第 0 条改为新流程速记;检查点补充「确认过厂商 + URL 来源」;坑列表补「非 URL → 重新提供」「URL 拉取失败 → 换」 |
| `docs/provider厂商定制包指南.md` | Step 0 章节重写:0.1~0.5 流程 + 提取清单(6 类)+ 完成度标准表(详细内容放这里,SKILL.md 保持薄) |

不做的事:
- 不改注册/balancer/config 的既有机制(本 spec 只改调研环节)
- 不产生独立调研文件(决策 #4)
- 不引入「自研兜底来源」(决策 #1)

## 7. 验证方式

改完后:
1. `cd backend && go build ./... && go test ./...` 不受影响(不碰代码)
2. 用 skill 走一次「新增某厂商」的模拟流程(干跑,不真注册),确认 Step 0 流程可执行
