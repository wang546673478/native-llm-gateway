---
name: provider-vendor
description: 新增或更新一个 Provider 厂商定制包(如加回 kimi)。按 docs/provider厂商定制包指南.md 执行:写协议面包 → 双注册 → balancer → config 块 → 测试验证。用于「增加 kimi provider」「加一个新厂商」「更新某厂商的模型/端点」等请求。
---

# Provider 厂商定制包

按 `docs/provider厂商定制包指南.md` 执行,本 skill 提供检查清单与顺序。

## 概念速记

- 厂商(vendor)一个目录,内含多协议面注册名;同 vendor 共享 key 池
- `RegisterGlobalWithProtocolVendor(注册名, 工厂, 协议, 厂商)` — vendor 参数决定共享/归一
- 每个注册名都要 `quotacheck.RegisterBalancer`(token_plan 厂商必须有余额查询)
- 无官方余额 API 的厂商:查控制台未文档化端点(社区逆向,如 MiMo)→ cookie 鉴权
  (账号级,1 天过期)→ 放 config `quota_cookie` + 管理 API 热更新,balancer 按
  `k.BillingSource` 分端点;cookie 过期轮询退化保守(踩坑 #19)
- 原生支持 Responses API 的厂商标 `responses_api: true` + 配 `ResponsesPath`
- 请求路径主动查额度走统一入口 quotacheck.CheckQuota(复用 RegisterBalancer 注册表),厂商包不需要写自己的查询调用

## 执行顺序

0. **调研官方文档**(先做,防返工):官方文档是唯一权威,**用户提供 URL** — ① 确认厂商(请求未点明时先问)② 要官方文档 URL(非 URL → 重新提供;拉取失败 → 换)③ 站内遍历(`/llms.txt` 入口 + grep `anthropic|claude`)④ 提取 6 类(协议面 / 模型能力 / 定价含单位换算 / 余额 / 定制特性 / 入口)⑤ 与现有 config 冲突 → 标差异再动
1. **读模板**:`backend/internal/provider/deepseek/` 全套(deepseek.go / anthropic.go / balancer.go / registry_test.go)作为模板复制
2. **写 openai 面**(继承 `openai_compatible.Base`):协议校验、endpoint 校验、ChatPath/ResponsesPath、StreamUsage: true
3. **写 anthropic 面**(可选,继承 `anthropic_compatible.Base`)
4. **注册**:`init()` 里 `RegisterGlobalWithProtocolVendor` 每个协议面一条,vendor = 厂商名;**同时 `cmd/gateway/main.go` 加一行 blank import** 触发 init(Go 只编译被 import 的包,漏了它新厂商不进二进制、`/providers` 不出现)
5. **balancer**(可选但推荐):`RegisterBalancer` 每个注册名一条;余额接口先实测(错误可能藏在 HTTP 200 body)
6. **config.yaml 加块**:enabled/billing_source/endpoint/protocol/models(+default_model / responses_api)
7. **写 registry_test.go**:断言每个注册名的 Protocol 和 Vendor(复制 deepseek 的改名字)
8. **构建 + 全量测试**:`cd backend && go build ./... && go test ./...`
9. **重载网关验证**:`./gateway-reload.sh`(自动编译 + 优雅排空 + 新进程接管,不用手动 kill+起)→ `/api/v1/providers` 出现新厂商 → 页面加 key → 发请求 E2E(access log 看厂商名 + 实际模型)

## 检查点(提交前逐项过)

- [ ] 厂商已确认;官方文档 URL 来自用户(非 URL 已要求重新提供、拉取失败已换);6 类提取项全覆盖:协议面 / 模型能力 / 定价(含单位换算)/ 余额 / 定制特性 / 入口(grep `anthropic|claude`);与现有 config 冲突已标差异
- [ ] 每个协议面的注册名都有 `RegisterGlobalWithProtocolVendor`,vendor 都是厂商名
- [ ] `cmd/gateway/main.go` 有该厂商的 blank import(漏了 init() 不跑,不进二进制)
- [ ] 每个注册名都有 `RegisterBalancer`(有余额查询的厂商;无官方 API → 控制台 cookie 端点,见踩坑 #19)
- [ ] 同厂商所有 config 块 `billing_source` 一致(例外:mimo 双端点有意双值——按量/套餐各一套端点,靠 per-key BillingSource 隔离,balancer 按 key 分端点)
- [ ] `responses_api` 与官方支持矩阵一致;支持的话 `ResponsesPath` 不拼错 /v1(端点已含 /v1 用 `/responses`)
- [ ] 新厂商的 `default_model` 或 models[0] 是真实可用的模型 id
- [ ] `registry_test.go` 断言了双注册(防未来误删)
- [ ] 全量测试通过(允许 0 失败;有失败必须先修)
- [ ] 网关已重启、E2E 已通、access log 显示厂商名

## 常见坑(详情 docs/踩坑与排错.md)

- 上游错误可能藏在 HTTP 200 的 body(base_resp 模式)——只认状态码会漏
- **无终端禁用状态**:auth → COOLING 5 分钟自动重试;400 invalid_request 只计数;5xx/timeout/connection → per-key 熔断。别把「禁用」逻辑加回去
- 发请求必须复用 `req.Key`(路由层已 acquire),**不能内部二次 acquire** —— 双 acquire 会把 429 冷却标到没发过请求的 healthy key 上(踩坑 #15)
- 跨厂商推理块由网关统一剥离,厂商包无需处理
- 白名单选择语义:厂商声明过白名单模型就用白名单模型——新厂商的模型没进白名单,链上不会用它
- 注册了但 `/providers` 不出现 → 忘了 main.go 的 blank import(Go 只编译被 import 的包)
- 文档调研漏 anthropic 兼容章节 → 厂商只有 openai 面,Claude Code 用户用不了(真实教训:GLM 的 Claude API 兼容页在索引第 117 行,藏在 `/cn/guide/develop/claude/` 下,不 grep `anthropic|claude` 必漏)
- 用户给的文档不是 URL / URL 拉取失败 → 先要求重新提供,别自己搜替代来源(官方文档是唯一权威)
- 价格单位没换算 → 文档 0.42 元/M 直接抄进 `cost_per_1k`,价格差 1000 倍(÷1000 才进 config)
- 同 vendor 多注册名共协议(如 mimo openai×2 + anthropic×2)→ 前端协议下拉去重(踩坑 #19)
- 429 双义(限流 vs 套餐耗尽)厂商 → body 区分信号先实测,别假设
