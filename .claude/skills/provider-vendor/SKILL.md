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
- 原生支持 Responses API 的厂商标 `responses_api: true` + 配 `ResponsesPath`

## 执行顺序

0. **调研官方文档**(先做,防返工):拉文档站 `/llms.txt` 全量索引,逐项确认四件事 —
   ① 模型 ID 与能力(思考模式 / 工具调用)② **Anthropic/Claude 兼容面**(有没有 anthropic 端点——很多厂商有但藏在「Claude API 兼容」章节,必须 grep `anthropic|claude`,别只按关键词碰运气)③ Responses API 支持 ④ 余额查询接口
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

- [ ] 调研过官方文档四件事:模型 ID / anthropic 兼容面 / responses / 余额(grep `anthropic|claude`,不只看关键词页)
- [ ] 每个协议面的注册名都有 `RegisterGlobalWithProtocolVendor`,vendor 都是厂商名
- [ ] `cmd/gateway/main.go` 有该厂商的 blank import(漏了 init() 不跑,不进二进制)
- [ ] 每个注册名都有 `RegisterBalancer`(有余额查询的厂商)
- [ ] 同厂商所有 config 块 `billing_source` 一致
- [ ] `responses_api` 与官方支持矩阵一致;支持的话 `ResponsesPath` 不拼错 /v1(端点已含 /v1 用 `/responses`)
- [ ] 新厂商的 `default_model` 或 models[0] 是真实可用的模型 id
- [ ] `registry_test.go` 断言了双注册(防未来误删)
- [ ] 全量测试通过(允许 0 失败;有失败必须先修)
- [ ] 网关已重启、E2E 已通、access log 显示厂商名

## 常见坑(详情 docs/踩坑与排错.md)

- 上游错误可能藏在 HTTP 200 的 body(base_resp 模式)——只认状态码会漏
- 400 invalid_request **不禁用 key**(auth 才禁用)— 别把逻辑改回去
- 跨厂商推理块由网关统一剥离,厂商包无需处理
- 白名单选择语义:厂商声明过白名单模型就用白名单模型——新厂商的模型没进白名单,链上不会用它
- 注册了但 `/providers` 不出现 → 忘了 main.go 的 blank import(Go 只编译被 import 的包)
- 文档调研漏 anthropic 兼容章节 → 厂商只有 openai 面,Claude Code 用户用不了(真实教训:GLM 的 Claude API 兼容页在索引第 117 行,藏在 `/cn/guide/develop/claude/` 下,不 grep `anthropic|claude` 必漏)
