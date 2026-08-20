---
name: provider-vendor
description: 新增或更新一个 Provider 厂商定制包(如加回 kimi)。按 docs/provider厂商定制包指南.md 执行:调研官方文档 → 写协议面包 → 双注册 → balancer → config 块 → 测试验证 → 同步模型(否则 no_route)→ 拖拽排序。用于「增加 kimi provider」「加一个新厂商」「更新某厂商的模型/端点」等请求。
---

# Provider 厂商定制包

按 `docs/provider厂商定制包指南.md` 执行,本 skill 提供检查清单与顺序。

## 概念速记

- 厂商(vendor)一个目录,内含多协议面注册名;同 vendor 共享 key 池
- `RegisterGlobalWithProtocolVendor(注册名, 工厂, 协议, 厂商)` — vendor 参数决定共享/归一
- 每个注册名都要 `quotacheck.RegisterBalancer`(有余额查询能力的厂商必须写;真无端点才走 probe 兜底)
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
4. **注册**:`init()` 里 `RegisterGlobalWithProtocolVendor` 每个协议面一条,vendor = 厂商名;**同时在 `internal/provider/builtin/builtin.go` 加一行 blank import** 触发 init(Go 只编译被 import 的包,漏了它新厂商不进二进制、`/providers` 不出现)
5. **balancer**:`RegisterBalancer` 每个注册名一条(有余额查询能力的厂商必须写;无官方 API 但有控制台端点 → cookie 鉴权,见踩坑 #19;真无余额端点才留空走 probe);余额接口先实测(错误可能藏在 HTTP 200 body)
6. **config.yaml 加块**:enabled/billing_source/endpoint/protocol/timeout(+responses_api)。**不要再写 `models:`/`default_model`/`cost_per_1k_*` — 模型已进 DB `provider_models`(2026-08-20 起)**
7. **写 registry_test.go**:断言每个注册名的 Protocol 和 Vendor(复制 deepseek 的改名字)
8. **构建 + 全量测试**:`cd backend && go build ./... && go test ./...`
9. **重载网关验证**:`./gateway-reload.sh`(make build + `sudo systemctl restart` + health check;**需要 sudo**;无 sudo 时手动 `make build` + `kill -TERM <pid>` 靠 Restart=always 拉起)→ `/api/v1/providers` 出现新厂商 → 页面加 key
10. **同步模型(必须)**:模型管理页点该厂商「同步」,或「全部同步」`POST /api/v1/providers/sync-all-models`,把上游模型写进 `provider_models`。**漏了这步,gateway key 白名单命不中模型 → no_route 503**
11. **拖拽排序(可选)**:Routing 页「调度顺序树」拖成想要的优先级,点「保存排序」写 `route_order` + 热生效。不拖则新厂商走默认序「最早 key 加入时间」,且**有改写的厂商恒排在无改写的新厂商之前**(即新厂商通常垫底),仍参与路由
12. **E2E**:发任意模型名 → 命中新厂商;access log 看厂商名 + 实际模型

## 检查点(提交前逐项过)

- [ ] 厂商已确认;官方文档 URL 来自用户(非 URL 已要求重新提供、拉取失败已换);6 类提取项全覆盖:协议面 / 模型能力 / 定价(含单位换算)/ 余额 / 定制特性 / 入口(grep `anthropic|claude`);与现有 config 冲突已标差异
- [ ] 每个协议面的注册名都有 `RegisterGlobalWithProtocolVendor`,vendor 都是厂商名
- [ ] `internal/provider/builtin/builtin.go` 有该厂商的 blank import(漏了 init() 不跑,不进二进制)
- [ ] 每个注册名都有 `RegisterBalancer`(有余额查询的厂商;无官方 API → 控制台 cookie 端点,见踩坑 #19)
- [ ] 同厂商所有 config 块 `billing_source` 一致(例外:mimo 双端点有意双值——按量/套餐各一套端点,靠 per-key BillingSource 隔离,balancer 按 key 分端点)
- [ ] `responses_api` 与官方支持矩阵一致;支持的话 `ResponsesPath` 不拼错 /v1(端点已含 /v1 用 `/responses`)
- [ ] config 块**没写** `models:`/`default_model`/`cost_per_1k_*`(这些已废弃,模型走 DB `provider_models`)
- [ ] `registry_test.go` 断言了双注册(防未来误删)
- [ ] 全量测试通过(允许 0 失败;有失败必须先修)
- [ ] 网关已重启、**已同步模型**(`/providers/sync-all-models` 或模型管理页「全部同步」),`provider_models` 里出现该厂商模型
- [ ] 已按需在 Routing 页拖拽排序并保存(否则新厂商默认垫底;不拖也能用只是排后)
- [ ] E2E 已通、access log 显示厂商名

## 常见坑(详情 docs/踩坑与排错.md)

- 上游错误可能藏在 HTTP 200 的 body(base_resp 模式)——只认状态码会漏
- **无终端禁用状态**:auth → COOLING 5 分钟自动重试;400 invalid_request 只计数;5xx/timeout/connection → per-key 熔断。别把「禁用」逻辑加回去
- 发请求必须复用 `req.Key`(路由层已 acquire),**不能内部二次 acquire** —— 双 acquire 会把 429 冷却标到没发过请求的 healthy key 上(踩坑 #15)
- 跨厂商推理块由网关统一剥离,厂商包无需处理
- 白名单选择语义:厂商声明过白名单模型就用白名单模型——新厂商的模型没进白名单,链上不会用它
- 注册了但 `/providers` 不出现 → 忘了 `internal/provider/builtin/builtin.go` 的 blank import(Go 只编译被 import 的包)
- 文档调研漏 anthropic 兼容章节 → 厂商只有 openai 面,Claude Code 用户用不了(真实教训:GLM 的 Claude API 兼容页在索引第 117 行,藏在 `/cn/guide/develop/claude/` 下,不 grep `anthropic|claude` 必漏)
- 用户给的文档不是 URL / URL 拉取失败 → 先要求重新提供,别自己搜替代来源(官方文档是唯一权威)
- 价格单位没换算 → 文档 0.42 元/M 直接抄进「每百万」,价格差 1000 倍(元/M 即元/百万,进 DB 三档每百万;美元价先换汇)
- 同 vendor 多注册名共协议(如 mimo openai×2 + anthropic×2)→ 前端协议下拉去重(踩坑 #19)
- 429 双义(限流 vs 套餐耗尽)厂商 → body 区分信号先实测,别假设
- **加 key 后忘了同步模型** → 模型没进 `provider_models`,gateway key 白名单命不中它 → 新厂商路由 no_route 503(踩坑 #23)。同步用「全部同步」`/providers/sync-all-models`,别逐个点
- **以为加了厂商就会自动排到想要的优先级** → route_order 表不会自动更新,新厂商默认垫底(排在已拖过序的厂商后)。要在 Routing 页拖拽 + 保存才写进 `route_order`(踩坑 #24)
