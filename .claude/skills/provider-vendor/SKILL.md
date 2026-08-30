---
name: provider-vendor
description: 新增或更新 Native LLM Gateway 的内置 Provider 厂商包。先判断动态 relay 是否足够，再按当前 vendor/face、DB 模型、KeyPool、quota 和测试契约实施。
---

# Provider 厂商包

完整规范见 `docs/provider厂商定制包指南.md`；当前目录和运行事实见
`docs/providers.md`。本 skill 只处理需要 Go 代码的内置厂商。

## 决策门

先确认是否真的需要厂商包：

- 标准 OpenAI/Anthropic 兼容上游，只需 base URL + key：按
  `docs/relay-stations.md` 创建 single relay，不改 Go。
- 专属鉴权、URL、body、错误、usage、余额或同厂商多端点：继续本流程。
- Google relay 当前不可用；如目标是 Google，必须按内置包完整实现和测试。

不要恢复已经删除的硬编码商业中转站，也不要向 `config.yaml` 添加 relay 站点。

## 必读代码

开始修改前读取：

- `backend/internal/provider/provider.go`
- `backend/internal/provider/registry.go`
- `backend/internal/provider/manager.go`
- `backend/internal/provider/builtin/builtin.go`
- 对应协议基座：`openai_compatible/`、`anthropic_compatible/` 或 `google/`
- 最接近的现有包：`deepseek/`、`minimax/` 或 `mimo/`
- `backend/internal/provider/modelsync.go`
- `backend/internal/server/server.go` 中 `buildKeyPools`/`toPoolCfg`/`ReloadProviderPool`
- `backend/internal/keypool/` 和 `backend/internal/quotacheck/`
- `config.example.yaml` 与 `docs/provider厂商定制包指南.md`

## 执行流程

1. 从官方文档和可复现直连请求提取协议、完整 URL、鉴权、模型、请求特性、usage、
   流式形状、错误、额度和定价。至少实测正常、流式、401/403、429 和额度耗尽。
2. 列出 vendor 及全部 face。每个 face 明确注册名、protocol、endpoint、
   `billing_source`、Responses 能力和可用 key 类型。
3. 标准兼容面嵌入协议 Base；非标准行为留在厂商包。`ProviderConfig.Pool` 已是
   `*keypool.Pool`，直接传 `cfg.Pool`，不要恢复旧 `toPool(interface{})` 模板。
4. OpenAI endpoint 已含 `/v1` 时显式写 `/chat/completions`、`/responses`、
   `/models`；未含时使用经实测的完整相对路径。把 `cfg.BillingSource` 传给 OpenAI
   Base。
5. Provider 请求优先使用路由层 `req.Key`；只有无路由上下文时自行 acquire。实际向
   pool 上报后设置 `ProviderError.KeyPoolReported`，避免 Proxy 重复计数。
6. 在 `init()` 为每个 face 调
   `RegisterGlobalWithProtocolVendor(face, factory, protocol, vendor)`，并在
   `provider/builtin/builtin.go` blank import 包。
7. 有可靠余额端点时实现 balancer，并为该 vendor 的每个 face 注册；mixed tier 按
   `k.BillingSource` 选额度端点。没有可靠端点就明确走 probe，不伪造余额。
8. 在两个示例配置中增加每个 face 的 enabled/endpoint/protocol/timeout/
   `billing_source`/Responses/circuit 配置。不要添加 keys、models、default_model 或旧
   `cost_per_1k_*`。
9. 增加注册、构造、最终 URL、鉴权、usage、错误、SSE、key 一致性、balancer 和 mixed
   tier 测试。
10. 更新长期文档中的当前厂商目录、特殊契约和已知限制；删除被本次实现取代的一次性
    计划或旧接入说明。
11. 运行 Provider 定向测试、后端全量测试和 `go vet`。部署验证只有在用户要求且环境
    明确时执行，不把本地服务管理方式写死在代码步骤里。
12. 运行后添加上游 key、同步模型、填写三档价格、保存路由顺序，再做各协议流式与
    非流式 E2E，并从 Access Log 核对 vendor、protocol、key 和最终模型。

## 不变量

- 同一实体的所有 face 使用同一个 vendor；只有 endpoint 对应的计费层可以不同。
- face 名全局唯一，factory 的 `Name()` 与注册名一致。
- `provider_api_keys.provider_name` 使用 vendor；`protocols` 空表示所有协议。
- 模型和定价唯一真相源是 `provider_models`/`provider_model_faces`，不是 config。
- 内置 Responses 候选由 `responses_api` 过滤；只给真实支持的 OpenAI face 开启。
- OpenAI 缓存 token 必须从普通 input 中扣除，不能同时按 input 和 cache read 计费。
- HTTP 200 body 和 SSE 事件都可能表示失败；分类以结构和实测为准。
- 429、auth、quota、invalid request、model not found 和 5xx 不能合并成一个错误类别。
- 共享池中的发送 key、Access Log key 和冷却/熔断上报 key 必须是同一把。

## 提交门禁

```bash
cd backend
go test -count=1 ./internal/provider/acme ./internal/provider/...
go test -count=1 ./...
go vet ./...
```

提交前确认：

- [ ] 动态 relay 不足以完成需求的理由明确。
- [ ] 每个 face 的 Protocol/Vendor/blank import 有回归测试。
- [ ] endpoint 含/不含 `/v1` 和尾斜杠均锁定最终 URL。
- [ ] 请求复用 `req.Key`，错误只上报一次。
- [ ] balancer 的零额度与查询失败语义分开。
- [ ] config 没有旧 key、模型或价格字段。
- [ ] 模型同步后 face 归属正确，价格单位是 CNY/1M token。
- [ ] 定向测试、全量测试和 `go vet` 零失败。
